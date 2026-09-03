package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// SecretKeep replaces literal secret values in snapshots; `${VAR}` env
// references are preserved verbatim.
const SecretKeep = "__TOKENROUTE_KEEP_SECRET__"

type Snapshot struct {
	Revision             string         `json:"revision"`
	Document             map[string]any `json:"document"`
	RawYAML              string         `json:"raw_yaml"`
	Schema               *FieldSchema   `json:"schema"`
	RestartRequiredPaths []string       `json:"restart_required_paths"`
}

type Store struct {
	path        string
	backupLimit int
	mu          sync.Mutex
}

func NewStore(path string, backupLimit int) *Store {
	return &Store{path: path, backupLimit: backupLimit}
}

// envRef matches a whole scalar that is exactly $NAME or ${NAME}.
var envRef = regexp.MustCompile(`^\$(?:[A-Za-z_][A-Za-z0-9_]*|\{[A-Za-z_][A-Za-z0-9_]*\})$`)

// Read loads the config file, redacts literal secrets, and returns a snapshot.
// The revision hashes the original on-disk bytes; errors name paths only.
func (s *Store) Read(ctx context.Context) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", s.path, err)
	}
	sum := sha256.Sum256(raw)
	revision := "sha256:" + hex.EncodeToString(sum[:])

	root, err := parseDocument(raw)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", s.path, err)
	}

	clone := cloneNode(root)
	redactNode(clone, "")

	doc := map[string]any{}
	if err := clone.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode config %s: %w", s.path, scrubErr(err))
	}

	var sb strings.Builder
	enc := yaml.NewEncoder(&sb)
	enc.SetIndent(2)
	if err := enc.Encode(clone); err != nil {
		return nil, fmt.Errorf("encode config %s: %w", s.path, err)
	}
	enc.Close()

	return &Snapshot{
		Revision:             revision,
		Document:             doc,
		RawYAML:              sb.String(),
		Schema:               FormSchema(),
		RestartRequiredPaths: RestartRequiredPaths(),
	}, nil
}

// cloneNode deep-copies a yaml.Node graph. Alias pointers are retargeted to
// nodes in the clone, so sanitizing the clone cannot reach or expose originals.
func cloneNode(n *yaml.Node) *yaml.Node {
	return cloneNodeGraph(n, map[*yaml.Node]*yaml.Node{})
}

func cloneNodeGraph(n *yaml.Node, cloned map[*yaml.Node]*yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	if c := cloned[n]; c != nil {
		return c
	}
	c := *n
	c.Content = nil
	c.Alias = nil
	cloned[n] = &c
	if n.Content != nil {
		c.Content = make([]*yaml.Node, len(n.Content))
		for i, ch := range n.Content {
			c.Content[i] = cloneNodeGraph(ch, cloned)
		}
	}
	c.Alias = cloneNodeGraph(n.Alias, cloned)
	return &c
}

// redactNode walks mapping values, replacing literal scalars at secret
// wildcard paths with SecretKeep. path uses [] for list items and .* for
// map entries, matching IsSecretPath.
func redactNode(n *yaml.Node, path string) {
	redactNodeWalk(n, path, map[*yaml.Node]bool{})
}

func redactNodeWalk(n *yaml.Node, path string, active map[*yaml.Node]bool) {
	if n == nil || active[n] {
		return
	}
	active[n] = true
	defer delete(active, n)

	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			redactNodeWalk(c, path, active)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			child := key.Value
			if path != "" {
				child = path + "." + key.Value
			}
			redactValueWalk(val, child, active)
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			redactValueWalk(c, path+"[]", active)
		}
	case yaml.AliasNode:
		redactValueWalk(n.Alias, path, active)
	}
}

// redactValue redacts scalars at secret paths, else recurses. Map values
// under a wildcard-secret parent (path.*) are redacted wholesale.
func redactValue(n *yaml.Node, path string) {
	redactValueWalk(n, path, map[*yaml.Node]bool{})
}

func redactValueWalk(n *yaml.Node, path string, active map[*yaml.Node]bool) {
	if n == nil {
		return
	}
	if n.Kind == yaml.AliasNode {
		if active[n] {
			return
		}
		active[n] = true
		defer delete(active, n)
		redactValueWalk(n.Alias, path, active)
		return
	}
	if n.Kind == yaml.ScalarNode {
		if isSecret(path) && n.Value != "" && !envRef.MatchString(n.Value) {
			n.Value = SecretKeep
			n.Tag = "!!str"
		}
		return
	}
	if isSecret(path) || isSecret(path+"[]") {
		// Malformed config put a container at a secret path (or at a path
		// whose list items are secret): redact every literal scalar
		// descendant, preserving exact env references.
		redactDescendantsWalk(n, active)
		return
	}
	if n.Kind == yaml.MappingNode && isSecret(path+".*") {
		for i := 1; i < len(n.Content); i += 2 {
			if v := n.Content[i]; v.Kind == yaml.ScalarNode && v.Value != "" && !envRef.MatchString(v.Value) {
				v.Value = SecretKeep
				v.Tag = "!!str"
			}
		}
	}
	redactNodeWalk(n, path, active)
}

// redactDescendants replaces every non-empty literal scalar value in the
// subtree with SecretKeep, keeping mapping keys and exact $NAME/${NAME}
// references verbatim.
func redactDescendants(n *yaml.Node) {
	redactDescendantsWalk(n, map[*yaml.Node]bool{})
}

func redactDescendantsWalk(n *yaml.Node, active map[*yaml.Node]bool) {
	if n == nil || active[n] {
		return
	}
	active[n] = true
	defer delete(active, n)

	if n.Kind == yaml.AliasNode {
		redactDescendantsWalk(n.Alias, active)
		return
	}
	if n.Kind == yaml.ScalarNode {
		if n.Value != "" && !envRef.MatchString(n.Value) {
			n.Value = SecretKeep
			n.Tag = "!!str"
		}
		return
	}
	if n.Kind == yaml.MappingNode {
		for i := 1; i < len(n.Content); i += 2 {
			redactDescendantsWalk(n.Content[i], active)
		}
		return
	}
	for _, c := range n.Content {
		redactDescendantsWalk(c, active)
	}
}

func isSecret(path string) bool {
	return IsSecretPath(path)
}

// scrubErr strips yaml parser messages that may echo source lines (which
// could contain secrets), keeping only the error type and position.
func scrubErr(err error) error {
	msg := err.Error()
	if i := strings.Index(msg, "\n"); i >= 0 {
		msg = msg[:i]
	}
	return fmt.Errorf("%s", msg)
}

// --- Task 4: candidate merge, validation, redacted diff -------------------

type EditRequest struct {
	ExpectedRevision string         `json:"expected_revision"`
	Mode             string         `json:"mode"`
	Document         map[string]any `json:"document,omitempty"`
	RawYAML          string         `json:"raw_yaml,omitempty"`
}

type Change struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Before any    `json:"before,omitempty"`
	After  any    `json:"after,omitempty"`
}

type Candidate struct {
	BaseRevision         string         `json:"base_revision"`
	CandidateRevision    string         `json:"candidate_revision"`
	Document             map[string]any `json:"document"`
	RawYAML              string         `json:"raw_yaml"`
	Diff                 []Change       `json:"diff"`
	ChangedPaths         []string       `json:"changed_paths"`
	RestartRequiredPaths []string       `json:"restart_required_paths"`
	Warnings             []string       `json:"warnings"`
	bytes                []byte
	config               *Config
}

// revisionOf hashes raw bytes into the store revision form.
func revisionOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// parseDocument parses exactly one YAML document into an AST root.
func parseDocument(data []byte) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		return nil, scrubErr(err)
	}
	var trailing yaml.Node
	if err := dec.Decode(&trailing); err != io.EOF {
		if err != nil {
			return nil, scrubErr(err)
		}
		return nil, &yamlTypeError{msg: "multiple YAML documents are not allowed"}
	}
	return &root, nil
}

// docRoot unwraps a parsed document to its root content node (nil if absent).
func docRoot(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		return n.Content[0]
	}
	return n
}

// yamlTypeError carries a line/column yaml error verbatim.
type yamlTypeError struct{ msg string }

func (e *yamlTypeError) Error() string { return e.msg }

// syntaxError formats err with line and column when yaml provides them.
// ponytail: yaml.v3 rarely reports columns; we surface "column unknown" then.
func syntaxError(err error) error {
	msg := firstLine(err.Error())
	line := "?"
	rest := msg
	if i := strings.Index(msg, "line "); i >= 0 {
		j := i + len("line ")
		k := j
		for k < len(msg) && msg[k] >= '0' && msg[k] <= '9' {
			k++
		}
		line = msg[j:k]
		rest = strings.TrimSpace(msg[k+1:])
	}
	col := "unknown"
	if m := colRe.FindStringSubmatch(msg); m != nil {
		col = m[1]
	}
	return &yamlTypeError{msg: fmt.Sprintf("syntax error at line %s, column %s: %s", line, col, rest)}
}

var colRe = regexp.MustCompile(`column (\d+)`)

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// Validate merges the edit onto the current file and returns a candidate.
func (s *Store) Validate(ctx context.Context, req EditRequest) (*Candidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", s.path, err)
	}
	return s.validateAgainst(raw, req)
}

func (s *Store) validateAgainst(raw []byte, req EditRequest) (*Candidate, error) {
	baseRev := revisionOf(raw)
	if req.ExpectedRevision != baseRev {
		return nil, fmt.Errorf("revision conflict: expected %s, current %s", req.ExpectedRevision, baseRev)
	}
	baseRoot, err := parseDocument(raw)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", s.path, err)
	}

	var candRoot *yaml.Node
	switch req.Mode {
	case "raw":
		if strings.TrimSpace(req.RawYAML) == "" {
			return nil, newValidationErr("", CodeEmptyRaw, "raw config is empty")
		}
		candRoot, err = parseDocument([]byte(req.RawYAML))
		if err != nil {
			return nil, newValidationErr("", CodeYAMLSyntax, syntaxError(err).Error())
		}
		if root := docRoot(candRoot); root == nil || root.Kind != yaml.MappingNode {
			return nil, newValidationErr("", CodeRootNotMapping, "raw config must be a mapping at the root")
		}
	case "structured":
		desired, merr := docToNode(req.Document)
		if merr != nil {
			return nil, merr
		}
		candRoot = cloneNode(baseRoot)
		if merr := mergeNode(candRoot, desired, FormSchema(), ""); merr != nil {
			return nil, merr
		}
	default:
		return nil, newValidationErr("", CodeUnknownMode, fmt.Sprintf("unknown mode %q", req.Mode))
	}

	if err := validateSubmittedSecrets(candRoot, FormSchema(), "", ""); err != nil {
		return nil, err
	}
	if err := restoreSecrets(baseRoot, candRoot, FormSchema(), ""); err != nil {
		return nil, err
	}

	candBytes, err := encodeNode(candRoot)
	if err != nil {
		return nil, err
	}
	if _, err := Decode(candBytes, false); err != nil {
		return nil, newValidationErr("", CodeCandidateInvalid, "candidate invalid: "+scrubErr(err).Error())
	}
	cfg, err := Decode(candBytes, true)
	if err != nil {
		return nil, newValidationErr("", CodeCandidateInvalid, "candidate invalid: "+scrubErr(err).Error())
	}

	diffBefore := cloneNode(baseRoot)
	redactNode(diffBefore, "")
	diffAfter := cloneNode(candRoot)
	redactNode(diffAfter, "")
	diff := semanticDiff(diffBefore, diffAfter, FormSchema())
	if len(diff) == 0 {
		candBytes = raw
	}

	presentRoot := cloneNode(candRoot)
	redactNode(presentRoot, "")
	doc := map[string]any{}
	if err := presentRoot.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode candidate: %w", scrubErr(err))
	}
	var sb strings.Builder
	enc := yaml.NewEncoder(&sb)
	enc.SetIndent(2)
	if err := enc.Encode(presentRoot); err != nil {
		return nil, fmt.Errorf("encode candidate: %w", err)
	}
	enc.Close()

	cand := &Candidate{
		BaseRevision:      baseRev,
		CandidateRevision: revisionOf(candBytes),
		Document:          doc,
		RawYAML:           sb.String(),
		Diff:              diff,
		bytes:             candBytes,
		config:            cfg,
	}
	for _, ch := range diff {
		cand.ChangedPaths = append(cand.ChangedPaths, ch.Path)
	}
	restart := map[string]bool{}
	for _, p := range RestartRequiredPaths() {
		restart[p] = true
	}
	for _, p := range cand.ChangedPaths {
		if restart[wildPath(p)] {
			cand.RestartRequiredPaths = append(cand.RestartRequiredPaths, p)
		}
	}
	// Non-nil identifies an admin validation/apply, even when this edit changes
	// no bootstrap fields. nil is reserved for the SIGHUP full-apply path.
	if cand.RestartRequiredPaths == nil {
		cand.RestartRequiredPaths = []string{}
	}
	if len(diff) == 0 {
		// No-op candidate: keep the REDACTED rendering — raw on-disk bytes
		// would leak literal secrets into API responses (Task 6 audit).
		cand.RawYAML = sb.String()
	}
	return cand, nil
}

// wildPath converts a concrete diff path (providers[p1].priority) to its
// wildcard schema path (providers[].priority).
func wildPath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '[' {
			for i < len(p) && p[i] != ']' {
				i++
			}
			b.WriteString("[]")
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// docToNode converts a JSON-shaped document map to a YAML mapping node.
func docToNode(doc map[string]any) (*yaml.Node, error) {
	if doc == nil {
		return nil, newValidationErr("", CodeDocumentRequired, "document is required for structured mode")
	}
	var v any = doc
	var root yaml.Node
	body, err := yaml.Marshal(v)
	if err != nil {
		return nil, newValidationErr("", CodeDocumentInvalid, "encode document: "+err.Error())
	}
	if err := yaml.Unmarshal(body, &root); err != nil {
		return nil, newValidationErr("", CodeDocumentInvalid, "parse document: "+scrubErr(err).Error())
	}
	return &root, nil
}

// childKey returns the value node for key in mapping m, or nil.
func childKey(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// setChild inserts or replaces key's value in mapping m (appends at end).
func setChild(m *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
}

// childSchema finds the schema child for a mapping key.
func childSchema(f *FieldSchema, key string) *FieldSchema {
	if f == nil {
		return nil
	}
	for _, c := range f.Children {
		if c.Name == key {
			return c
		}
	}
	// map value children are keyed by field name under ".*"
	for _, c := range f.Children {
		if strings.HasPrefix(c.Path, f.Path+".*.") {
			if c.Name == key {
				return c
			}
		}
	}
	return nil
}

// itemSchema returns the schema for list items of f.
func itemSchema(f *FieldSchema) *FieldSchema {
	if f == nil {
		return nil
	}
	return f.Item
}

// nodeScalar renders a scalar node as a plain string for identity matching.
func nodeScalar(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	if n.Kind == yaml.ScalarNode {
		return n.Value
	}
	return ""
}

// identity computes the schema identity key for a mapping list item.
func identity(item *yaml.Node, idKeys []string) (string, bool) {
	if len(idKeys) == 0 || item == nil || item.Kind != yaml.MappingNode {
		return "", false
	}
	var b strings.Builder
	for _, k := range idKeys {
		v := childKey(item, k)
		if v == nil {
			return "", false
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(nodeScalar(v))
		b.WriteByte(0)
	}
	return b.String(), true
}

// orderKeys returns mapping keys ordered by schema children order, then
// any extra keys sorted alphabetically.
func orderKeys(m *yaml.Node, schema *FieldSchema) []string {
	var keys []string
	seen := map[string]bool{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := m.Content[i].Value
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	if schema == nil {
		sort.Strings(keys)
		return keys
	}
	order := map[string]int{}
	for i, c := range schema.Children {
		if _, ok := order[c.Name]; !ok {
			order[c.Name] = i
		}
	}
	sort.SliceStable(keys, func(a, b int) bool {
		ia, oka := order[keys[a]]
		ib, okb := order[keys[b]]
		if oka && okb {
			return ia < ib
		}
		if oka {
			return true
		}
		if okb {
			return false
		}
		return keys[a] < keys[b]
	})
	return keys
}

// schemaPosition returns key's zero-based position among schema children.
func schemaPosition(schema *FieldSchema, key string) (int, bool) {
	if schema == nil {
		return 0, false
	}
	for i, child := range schema.Children {
		if child.Name == key {
			return i, true
		}
	}
	return 0, false
}

// insertChildBySchema inserts a new mapping pair without moving existing pairs.
// Known fields go immediately before the nearest later schema sibling; if none
// exists they follow the nearest earlier sibling. Unknown fields append after
// known/existing content in the deterministic order supplied by the caller.
func insertChildBySchema(m *yaml.Node, key string, val *yaml.Node, schema *FieldSchema) {
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	pos, known := schemaPosition(schema, key)
	if !known {
		m.Content = append(m.Content, keyNode, val)
		return
	}
	insertAt := len(m.Content)
	for i := 0; i+1 < len(m.Content); i += 2 {
		if siblingPos, ok := schemaPosition(schema, m.Content[i].Value); ok && siblingPos > pos {
			insertAt = i
			break
		}
	}
	m.Content = append(m.Content, nil, nil)
	copy(m.Content[insertAt+2:], m.Content[insertAt:len(m.Content)-2])
	m.Content[insertAt] = keyNode
	m.Content[insertAt+1] = val
}

// mergeNode merges desired into base in place, preserving comments and order
// from base while applying additions/removals/updates from desired. Schema
// guides sequence identity matching and new-key insertion order.
func mergeNode(base, desired *yaml.Node, schema *FieldSchema, path string) error {
	if desired == nil {
		return nil
	}
	if base == nil {
		return fmt.Errorf("merge: nil base at %s", path)
	}

	// Structured documents contain resolved alias values rather than alias
	// nodes. Keep an existing alias (and its attached comments) when the
	// submitted value is semantically unchanged; an actual edit may replace it.
	if base.Kind == yaml.AliasNode && desired.Kind != yaml.AliasNode {
		if yamlNodesEqual(base, desired) {
			return nil
		}
		*base = *cloneNode(desired)
		return nil
	}

	// Scalar or kind change: replace base node content wholesale but keep
	// comments from base when the kind is unchanged scalar.
	if base.Kind != desired.Kind {
		*base = *cloneNode(desired)
		return nil
	}

	switch base.Kind {
	case yaml.DocumentNode:
		if len(base.Content) == 0 || len(desired.Content) == 0 {
			return nil
		}
		return mergeNode(base.Content[0], desired.Content[0], schema, path)
	case yaml.MappingNode:
		return mergeMapping(base, desired, schema, path)
	case yaml.SequenceNode:
		return mergeSequence(base, desired, schema, path)
	case yaml.ScalarNode:
		if base.Value != desired.Value {
			base.Value = desired.Value
			base.Tag = desired.Tag
		}
		return nil
	case yaml.AliasNode:
		*base = *cloneNode(desired)
		return nil
	}
	return nil
}

func mergeMapping(base, desired *yaml.Node, schema *FieldSchema, path string) error {
	// Build desired key set.
	desiredKeys := map[string]*yaml.Node{}
	for i := 0; i+1 < len(desired.Content); i += 2 {
		desiredKeys[desired.Content[i].Value] = desired.Content[i+1]
	}
	// Remove base keys absent from desired.
	out := base.Content[:0]
	for i := 0; i+1 < len(base.Content); i += 2 {
		k := base.Content[i].Value
		if _, ok := desiredKeys[k]; ok {
			out = append(out, base.Content[i], base.Content[i+1])
		}
	}
	base.Content = out
	// Merge existing keys in place, then insert new keys in schema order.
	for i := 0; i+1 < len(base.Content); i += 2 {
		k := base.Content[i].Value
		dv := desiredKeys[k]
		childPath := k
		if path != "" {
			childPath = path + "." + k
		}
		if err := mergeNode(base.Content[i+1], dv, childSchema(schema, k), childPath); err != nil {
			return err
		}
	}
	// Insert new keys ordered by schema, appended after existing keys.
	var newKeys []string
	for k := range desiredKeys {
		if childKey(base, k) == nil {
			newKeys = append(newKeys, k)
		}
	}
	if len(newKeys) > 0 {
		// Build a temp mapping of new keys so orderKeys can sort them.
		tmp := &yaml.Node{Kind: yaml.MappingNode}
		for _, k := range newKeys {
			tmp.Content = append(tmp.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, desiredKeys[k])
		}
		for _, k := range orderKeys(tmp, schema) {
			insertChildBySchema(base, k, cloneNode(desiredKeys[k]), schema)
		}
	}
	return nil
}

func mergeSequence(base, desired *yaml.Node, schema *FieldSchema, path string) error {
	item := itemSchema(schema)
	var idKeys []string
	if schema != nil {
		idKeys = schema.Identity
	}
	if len(idKeys) == 0 {
		// No identity: replace wholesale, cloning desired items.
		base.Content = nil
		for _, d := range desired.Content {
			base.Content = append(base.Content, cloneNode(d))
		}
		return nil
	}
	// Search and failure rules deliberately permit duplicate selectors. Their
	// stable identity is the selector plus its 1-based occurrence in the list.
	nextIdentity := func(n *yaml.Node, occurrences map[string]int) (string, string, bool) {
		var id, display string
		var ok bool
		if path == "failure_rules" {
			if match := childKey(n, "match"); match != nil && nodeScalar(match) != "" {
				id, display, ok = "match="+nodeScalar(match)+"\x00", "match="+nodeScalar(match), true
			} else if status := childKey(n, "status"); status != nil {
				id, display, ok = "status="+nodeScalar(status)+"\x00", "status="+nodeScalar(status), true
			}
		} else {
			id, ok = identity(n, idKeys)
			display = identDisplay(idKeys, n)
		}
		if !ok {
			return "", "", false
		}
		if occurrenceIdentityPath(path) {
			occurrences[id]++
			display = fmt.Sprintf("%s#%d", display, occurrences[id])
			id = fmt.Sprintf("%s#%d", id, occurrences[id])
		}
		return id, display, true
	}

	// Identity match: index base items by identity.
	type entry struct {
		node *yaml.Node
		used bool
	}
	baseByIdent := map[string]*entry{}
	baseOccurrences := map[string]int{}
	for _, b := range base.Content {
		id, _, ok := nextIdentity(b, baseOccurrences)
		if !ok {
			continue
		}
		if _, dup := baseByIdent[id]; dup {
			return newValidationErr(path, CodeMergeConflict, "duplicate identity in base")
		}
		baseByIdent[id] = &entry{node: b}
	}
	var newContent []*yaml.Node
	seenIdents := map[string]bool{}
	desiredOccurrences := map[string]int{}
	for _, d := range desired.Content {
		id, display, ok := nextIdentity(d, desiredOccurrences)
		if !ok {
			return newValidationErr(path, CodeMergeConflict, fmt.Sprintf("item missing identity keys %v", idKeys))
		}
		if seenIdents[id] {
			return newValidationErr(path, CodeMergeConflict, "duplicate identity "+display)
		}
		seenIdents[id] = true
		if e, hit := baseByIdent[id]; hit {
			e.used = true
			childPath := path + "[" + display + "]"
			if err := mergeNode(e.node, d, item, childPath); err != nil {
				return err
			}
			newContent = append(newContent, e.node)
		} else {
			// New item: rebuild in schema order, then append.
			rebuilt := &yaml.Node{Kind: yaml.MappingNode}
			if d.Kind == yaml.MappingNode {
				for _, k := range orderKeys(d, item) {
					setChild(rebuilt, k, cloneNode(childKey(d, k)))
				}
			} else {
				rebuilt = cloneNode(d)
			}
			newContent = append(newContent, rebuilt)
		}
	}
	base.Content = newContent
	return nil
}

// identDisplay renders an identity for diffs/errors, e.g. "p1" for name=p1.
func identDisplay(idKeys []string, item *yaml.Node) string {
	if len(idKeys) == 1 {
		return nodeScalar(childKey(item, idKeys[0]))
	}
	var parts []string
	for _, k := range idKeys {
		parts = append(parts, k+"="+nodeScalar(childKey(item, k)))
	}
	return strings.Join(parts, ",")
}

// validateSubmittedSecrets rejects literal values supplied through the admin
// editing surface. Startup Decode remains intentionally permissive. Existing
// literals arrive as SecretKeep and are restored only after this check.
func validateSubmittedSecrets(n *yaml.Node, schema *FieldSchema, wild, concrete string) error {
	aliases := map[*yaml.Node]int{}
	countSentinelAliases(n, aliases, map[*yaml.Node]bool{})
	return validateSubmittedSecretsWalk(n, schema, wild, concrete, false, false, aliases, map[*yaml.Node]bool{})
}

func validateSubmittedSecretsWalk(
	n *yaml.Node,
	schema *FieldSchema,
	wild,
	concrete string,
	secretContext,
	viaAlias bool,
	aliases map[*yaml.Node]int,
	active map[*yaml.Node]bool,
) error {
	if n == nil {
		return nil
	}
	if active[n] {
		return newValidationErr(concrete, CodeYAMLSyntax, "cyclic YAML alias is not allowed")
	}
	active[n] = true
	defer delete(active, n)

	if n.Kind == yaml.AliasNode {
		if n.Alias == nil {
			return newValidationErr(concrete, CodeYAMLSyntax, "YAML alias has no target")
		}
		return validateSubmittedSecretsWalk(
			n.Alias,
			schema,
			wild,
			concrete,
			secretContext,
			true,
			aliases,
			active,
		)
	}
	if n.Kind == yaml.ScalarNode {
		if n.Value == SecretKeep && !isSecret(wild) && aliases[n] == 0 {
			return newValidationErr(wild, CodeSentinelMisuse, SecretKeep+" sentinel not allowed here")
		}
		if (secretContext || isSecret(wild)) && n.Value != "" && n.Value != SecretKeep && !envRef.MatchString(n.Value) {
			return newValidationErr(concrete, CodeLiteralSecretForbidden, "secret replacement must be an environment reference")
		}
		return nil
	}

	childSecretContext := secretContext || isSecret(wild) || isSecret(wild+"[]")
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) > 0 {
			return validateSubmittedSecretsWalk(
				n.Content[0],
				schema,
				wild,
				concrete,
				childSecretContext,
				viaAlias,
				aliases,
				active,
			)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i].Value
			childWild, childConcrete := key, key
			if wild != "" {
				childWild = wild + "." + key
			}
			if concrete != "" {
				childConcrete = concrete + "." + key
			}
			if err := validateSubmittedSecretsWalk(
				n.Content[i+1],
				childSchema(schema, key),
				childWild,
				childConcrete,
				childSecretContext,
				viaAlias,
				aliases,
				active,
			); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		occurrences := map[string]int{}
		for i, child := range n.Content {
			label := fmt.Sprintf("%d", i)
			if schema != nil && len(schema.Identity) > 0 {
				base := identDisplay(schema.Identity, child)
				occurrences[base]++
				label = base
				if occurrenceIdentityPath(wild) {
					label = fmt.Sprintf("%s#%d", base, occurrences[base])
				}
			}
			childConcrete := concrete + "[" + label + "]"
			if err := validateSubmittedSecretsWalk(
				child,
				itemSchema(schema),
				wild+"[]",
				childConcrete,
				childSecretContext,
				viaAlias,
				aliases,
				active,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func occurrenceIdentityPath(path string) bool {
	return path == "search" || path == "failure_rules"
}

// restoreSecrets walks base and candidate in parallel; where base holds a
// redacted-secret path, candidate must hold either SecretKeep (keep base
// value) or a new literal/env ref (replace). A moved or duplicated SecretKeep
// sentinel fails.
func markSentinels(n *yaml.Node, counts map[*yaml.Node]int, active map[*yaml.Node]bool) {
	if n == nil || active[n] {
		return
	}
	active[n] = true
	defer delete(active, n)
	if n.Kind == yaml.ScalarNode && n.Value == SecretKeep {
		counts[n]++
	}
	if n.Kind == yaml.AliasNode {
		markSentinels(n.Alias, counts, active)
	}
	for _, child := range n.Content {
		markSentinels(child, counts, active)
	}
}

func countSentinelAliases(n *yaml.Node, counts map[*yaml.Node]int, active map[*yaml.Node]bool) {
	if n == nil || active[n] {
		return
	}
	active[n] = true
	defer delete(active, n)
	if n.Kind == yaml.AliasNode {
		if containsSentinel(n.Alias, map[*yaml.Node]bool{}) {
			counts[n.Alias]++
			markSentinels(n.Alias, counts, map[*yaml.Node]bool{})
		}
		countSentinelAliases(n.Alias, counts, active)
		return
	}
	for _, child := range n.Content {
		countSentinelAliases(child, counts, active)
	}
}

func restoreSecrets(current, candidate *yaml.Node, schema *FieldSchema, path string) error {
	aliases := map[*yaml.Node]int{}
	countSentinelAliases(candidate, aliases, map[*yaml.Node]bool{})
	if err := restoreSecretsWalk(current, candidate, schema, path, path, aliases, map[[2]*yaml.Node]bool{}); err != nil {
		return err
	}
	return rejectRemainingSentinels(candidate, schema, path)
}

func containsSentinel(n *yaml.Node, active map[*yaml.Node]bool) bool {
	if n == nil || active[n] {
		return false
	}
	active[n] = true
	defer delete(active, n)
	if n.Kind == yaml.ScalarNode && n.Value == SecretKeep {
		return true
	}
	if n.Kind == yaml.AliasNode && containsSentinel(n.Alias, active) {
		return true
	}
	for _, child := range n.Content {
		if containsSentinel(child, active) {
			return true
		}
	}
	return false
}

func restoreSecretsWalk(current, candidate *yaml.Node, schema *FieldSchema, wild, concrete string, aliases map[*yaml.Node]int, active map[[2]*yaml.Node]bool) error {
	if candidate == nil {
		return nil
	}
	pair := [2]*yaml.Node{current, candidate}
	if active[pair] {
		return newValidationErr(concrete, CodeYAMLSyntax, "cyclic YAML alias is not allowed")
	}
	active[pair] = true
	defer delete(active, pair)

	if candidate.Kind == yaml.AliasNode {
		if candidate.Alias == nil {
			return newValidationErr(concrete, CodeYAMLSyntax, "YAML alias has no target")
		}
		if current != nil && current.Kind == yaml.AliasNode && current.Alias != nil {
			return restoreSecretsWalk(current.Alias, candidate.Alias, schema, wild, concrete, aliases, active)
		}
		// A newly introduced alias may not carry a redaction sentinel. Existing
		// aliases are handled above and remain bound to their exact base target.
		if aliases[candidate.Alias] > 0 {
			return newValidationErr(concrete, CodeSentinelMisuse, SecretKeep+" sentinel not allowed through a new alias")
		}
		return restoreSecretsWalk(current, candidate.Alias, schema, wild, concrete, aliases, active)
	}
	// Count sentinels at this node: a sentinel may only appear where current
	// holds a literal secret (or a secret container). We compare against
	// current at the same path; mismatch = moved/duplicated.
	switch candidate.Kind {
	case yaml.DocumentNode:
		var cur *yaml.Node
		if current != nil && current.Kind == yaml.DocumentNode && len(current.Content) > 0 {
			cur = current.Content[0]
		}
		if len(candidate.Content) > 0 {
			return restoreSecretsWalk(cur, candidate.Content[0], schema, wild, concrete, aliases, active)
		}
		return nil
	case yaml.MappingNode:
		var cur *yaml.Node
		if current != nil && current.Kind == yaml.MappingNode {
			cur = current
		}
		for i := 0; i+1 < len(candidate.Content); i += 2 {
			k := candidate.Content[i].Value
			childWild, childConcrete := k, k
			if wild != "" {
				childWild = wild + "." + k
			}
			if concrete != "" {
				childConcrete = concrete + "." + k
			}
			var curVal *yaml.Node
			if cur != nil {
				curVal = childKey(cur, k)
			}
			if err := restoreSecretsWalk(curVal, candidate.Content[i+1], childSchema(schema, k), childWild, childConcrete, aliases, active); err != nil {
				return err
			}
		}
		return nil
	case yaml.SequenceNode:
		var cur *yaml.Node
		if current != nil && current.Kind == yaml.SequenceNode {
			cur = current
		}
		var idKeys []string
		if schema != nil {
			idKeys = schema.Identity
		}
		if len(idKeys) == 0 {
			// Positional match (no identity): sentinels at scalar list items.
			for i, c := range candidate.Content {
				var curItem *yaml.Node
				if cur != nil && i < len(cur.Content) {
					curItem = cur.Content[i]
				}
				childConcrete := concrete + "[" + fmt.Sprintf("%d", i) + "]"
				if err := restoreSecretsWalk(curItem, c, itemSchema(schema), wild+"[]", childConcrete, aliases, active); err != nil {
					return err
				}
			}
			return nil
		}
		curByIdent := map[string]*yaml.Node{}
		nextIdentity := func(n *yaml.Node, occurrences map[string]int) (string, bool) {
			var id string
			var ok bool
			if wild == "failure_rules" {
				if match := childKey(n, "match"); match != nil && nodeScalar(match) != "" {
					id, ok = "match="+nodeScalar(match)+"\x00", true
				} else if status := childKey(n, "status"); status != nil {
					id, ok = "status="+nodeScalar(status)+"\x00", true
				}
			} else {
				id, ok = identity(n, idKeys)
			}
			if ok && occurrenceIdentityPath(wild) {
				occurrences[id]++
				id = fmt.Sprintf("%s#%d", id, occurrences[id])
			}
			return id, ok
		}
		currentOccurrences := map[string]int{}
		if cur != nil {
			for _, ci := range cur.Content {
				if id, ok := nextIdentity(ci, currentOccurrences); ok {
					curByIdent[id] = ci
				}
			}
		}
		candidateOccurrences := map[string]int{}
		for _, c := range candidate.Content {
			id, ok := nextIdentity(c, candidateOccurrences)
			if !ok {
				continue
			}
			label := identDisplay(idKeys, c)
			if occurrenceIdentityPath(wild) {
				if hash := strings.LastIndex(id, "#"); hash >= 0 {
					label += id[hash:]
				}
			}
			childConcrete := concrete + "[" + label + "]"
			if err := restoreSecretsWalk(curByIdent[id], c, itemSchema(schema), wild+"[]", childConcrete, aliases, active); err != nil {
				return err
			}
		}
		return nil
	case yaml.ScalarNode:
		if candidate.Value != SecretKeep {
			return nil
		}
		// A sentinel outside a secret path can be the redacted target of an alias
		// whose concrete use is visited later. Leave it in place for that alias to
		// restore; the final sweep rejects it if no exact existing alias consumes it.
		if !isSecret(wild) {
			if aliases[candidate] > 0 {
				return nil
			}
			return newValidationErr(concrete, CodeSentinelMisuse, SecretKeep+" sentinel not allowed here")
		}
		// Sentinel present: current must hold a literal (non-env) secret here.
		if current == nil || current.Kind != yaml.ScalarNode || envRef.MatchString(current.Value) {
			return newValidationErr(concrete, CodeSentinelMisuse, SecretKeep+" sentinel not allowed here")
		}
		// Keep base value.
		candidate.Value = current.Value
		candidate.Tag = current.Tag
		return nil
	}
	return nil
}

func rejectRemainingSentinels(n *yaml.Node, schema *FieldSchema, path string) error {
	return rejectRemainingSentinelsWalk(n, schema, path, map[*yaml.Node]bool{})
}

func rejectRemainingSentinelsWalk(n *yaml.Node, schema *FieldSchema, path string, active map[*yaml.Node]bool) error {
	if n == nil || active[n] {
		return nil
	}
	active[n] = true
	defer delete(active, n)
	if n.Kind == yaml.AliasNode {
		if n.Alias == nil {
			return newValidationErr(path, CodeYAMLSyntax, "YAML alias has no target")
		}
		return rejectRemainingSentinelsWalk(n.Alias, schema, path, active)
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) > 0 {
			return rejectRemainingSentinelsWalk(n.Content[0], schema, path, active)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i].Value
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			if err := rejectRemainingSentinelsWalk(n.Content[i+1], childSchema(schema, key), childPath, active); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		occurrences := map[string]int{}
		for i, child := range n.Content {
			label := fmt.Sprintf("%d", i)
			if schema != nil && len(schema.Identity) > 0 {
				base := identDisplay(schema.Identity, child)
				occurrences[base]++
				label = base
				if occurrenceIdentityPath(path) {
					label = fmt.Sprintf("%s#%d", base, occurrences[base])
				}
			}
			if err := rejectRemainingSentinelsWalk(child, itemSchema(schema), path+"["+label+"]", active); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if n.Value == SecretKeep {
			return newValidationErr(path, CodeSentinelMisuse, SecretKeep+" sentinel not allowed here")
		}
	}
	return nil
}

func yamlNodesEqual(a, b *yaml.Node) bool {
	seen := map[[2]*yaml.Node]bool{}
	var equal func(*yaml.Node, *yaml.Node) bool
	equal = func(a, b *yaml.Node) bool {
		if a == nil || b == nil {
			return a == b
		}
		pair := [2]*yaml.Node{a, b}
		if seen[pair] {
			return true
		}
		seen[pair] = true
		if a.Kind == yaml.AliasNode {
			return equal(a.Alias, b)
		}
		if b.Kind == yaml.AliasNode {
			return equal(a, b.Alias)
		}
		if a.Kind != b.Kind {
			return false
		}
		if a.Kind == yaml.ScalarNode {
			return a.Value == b.Value && a.Tag == b.Tag
		}
		if len(a.Content) != len(b.Content) {
			return false
		}
		for i := range a.Content {
			if !equal(a.Content[i], b.Content[i]) {
				return false
			}
		}
		return true
	}
	return equal(a, b)
}

// semanticDiff walks base and candidate, reporting changes by concrete path.
// Secret scalars are classified, never valued.
func semanticDiff(before, after *yaml.Node, schema *FieldSchema) []Change {
	if yamlNodesEqual(before, after) {
		return nil
	}
	var out []Change
	var walk func(b, a *yaml.Node, f *FieldSchema, path string)
	walk = func(b, a *yaml.Node, f *FieldSchema, path string) {
		if yamlNodesEqual(b, a) {
			return
		}
		if a == nil {
			return
		}
		// Unwrap documents.
		if a.Kind == yaml.DocumentNode && len(a.Content) > 0 {
			var bb *yaml.Node
			if b != nil && b.Kind == yaml.DocumentNode && len(b.Content) > 0 {
				bb = b.Content[0]
			}
			walk(bb, a.Content[0], f, path)
			return
		}
		if b != nil && b.Kind == yaml.DocumentNode && len(b.Content) > 0 {
			walk(b.Content[0], a, f, path)
			return
		}
		if b == nil {
			out = append(out, classifyAdd(a, f, path))
			return
		}
		if b.Kind != a.Kind {
			out = append(out, classifyUpdate(b, a, f, path))
			return
		}
		switch a.Kind {
		case yaml.ScalarNode:
			if b.Value != a.Value {
				out = append(out, classifyUpdate(b, a, f, path))
			}
		case yaml.MappingNode:
			// Removed keys.
			for i := 0; i+1 < len(b.Content); i += 2 {
				k := b.Content[i].Value
				if childKey(a, k) == nil {
					childPath := k
					if path != "" {
						childPath = path + "." + k
					}
					out = append(out, classifyRemove(b.Content[i+1], childSchema(f, k), childPath))
				}
			}
			for i := 0; i+1 < len(a.Content); i += 2 {
				k := a.Content[i].Value
				childPath := k
				if path != "" {
					childPath = path + "." + k
				}
				walk(childKey(b, k), a.Content[i+1], childSchema(f, k), childPath)
			}
		case yaml.SequenceNode:
			var idKeys []string
			if f != nil {
				idKeys = f.Identity
			}
			if len(idKeys) == 0 {
				if !seqEqual(b, a) {
					out = append(out, classifyUpdate(b, a, f, path))
				}
				return
			}
			bByIdent := map[string]*yaml.Node{}
			nextIdentity := func(n *yaml.Node, occurrences map[string]int) (string, string, bool) {
				var id, display string
				var ok bool
				if path == "failure_rules" {
					if match := childKey(n, "match"); match != nil && nodeScalar(match) != "" {
						id, display, ok = "match="+nodeScalar(match)+"\x00", "match="+nodeScalar(match), true
					} else if status := childKey(n, "status"); status != nil {
						id, display, ok = "status="+nodeScalar(status)+"\x00", "status="+nodeScalar(status), true
					}
				} else {
					id, ok = identity(n, idKeys)
					display = identDisplay(idKeys, n)
				}
				if ok && occurrenceIdentityPath(path) {
					occurrences[id]++
					display = fmt.Sprintf("%s#%d", display, occurrences[id])
					id = fmt.Sprintf("%s#%d", id, occurrences[id])
				}
				return id, display, ok
			}
			bOccurrences := map[string]int{}
			var orderB []string
			for _, bi := range b.Content {
				if id, _, ok := nextIdentity(bi, bOccurrences); ok {
					bByIdent[id] = bi
					orderB = append(orderB, id)
				}
			}
			aIdents := map[string]bool{}
			var orderA []string
			aIdentities := make([]string, len(a.Content))
			aDisplays := make([]string, len(a.Content))
			aOccurrences := map[string]int{}
			for i, ai := range a.Content {
				id, display, ok := nextIdentity(ai, aOccurrences)
				if ok {
					aIdentities[i], aDisplays[i] = id, display
					orderA = append(orderA, id)
				}
			}
			reordered := len(orderB) == len(orderA)
			if reordered {
				same := true
				for i := range orderB {
					if orderB[i] != orderA[i] {
						same = false
						break
					}
				}
				reordered = !same
			}
			if reordered {
				out = append(out, Change{Path: path, Kind: "reorder"})
			}
			for i, ai := range a.Content {
				id := aIdentities[i]
				if id == "" {
					continue
				}
				aIdents[id] = true
				childPath := path + "[" + aDisplays[i] + "]"
				if bByIdent[id] == nil {
					out = append(out, classifyAdd(ai, itemSchema(f), childPath))
				} else {
					walk(bByIdent[id], ai, itemSchema(f), childPath)
				}
			}
			bOccurrences = map[string]int{}
			for _, bi := range b.Content {
				id, display, ok := nextIdentity(bi, bOccurrences)
				if !ok {
					continue
				}
				if !aIdents[id] {
					childPath := path + "[" + display + "]"
					out = append(out, classifyRemove(bi, itemSchema(f), childPath))
				}
			}
		}
	}
	walk(before, after, schema, "")
	return out
}

// seqEqual compares two sequences shallowly (scalar items).
func seqEqual(a, b *yaml.Node) bool {
	if len(a.Content) != len(b.Content) {
		return false
	}
	for i := range a.Content {
		ai, bi := a.Content[i], b.Content[i]
		if ai.Kind != bi.Kind || ai.Value != bi.Value {
			return false
		}
	}
	return true
}

func isSecretField(f *FieldSchema, path string) bool {
	if f != nil && f.Secret {
		return true
	}
	return isSecret(wildPath(path))
}

// secretMarker is the only text allowed in a secret diff Before/After.
func classifyUpdate(b, a *yaml.Node, f *FieldSchema, path string) Change {
	if isSecretField(f, path) {
		return Change{Path: path, Kind: "update", Before: "secret unchanged", After: secretAfterText(b, a)}
	}
	return Change{Path: path, Kind: "update", Before: redactedNodeJSON(b, f, path), After: redactedNodeJSON(a, f, path)}
}

func classifyAdd(a *yaml.Node, f *FieldSchema, path string) Change {
	if isSecretField(f, path) {
		return Change{Path: path, Kind: "add", After: "secret added"}
	}
	return Change{Path: path, Kind: "add", After: redactedNodeJSON(a, f, path)}
}

func classifyRemove(b *yaml.Node, f *FieldSchema, path string) Change {
	if isSecretField(f, path) {
		return Change{Path: path, Kind: "remove", Before: "secret removed"}
	}
	return Change{Path: path, Kind: "remove", Before: redactedNodeJSON(b, f, path)}
}

// redactedNodeJSON serializes a node for a diff with every literal scalar at
// a secret path (the node itself or any descendant) replaced by SecretKeep.
// Env references survive verbatim; only secret classifications, never values.
// Schema-aware: a malformed container shape (e.g. a mapping where the schema
// expects an identity sequence) still redacts fields secret on the item.
func redactedNodeJSON(n *yaml.Node, f *FieldSchema, path string) any {
	if n == nil {
		return nil
	}
	c := cloneNode(n)
	redactForDiff(c, f, wildPath(path))
	return nodeJSON(c)
}

// redactForDiff redacts literal secret scalars in n, descending with schema
// and wildcard path in parallel.
func redactForDiff(n *yaml.Node, f *FieldSchema, path string) {
	if n == nil {
		return
	}
	if n.Kind == yaml.DocumentNode {
		for _, c := range n.Content {
			redactForDiff(c, f, path)
		}
		return
	}
	if n.Kind == yaml.AliasNode {
		redactForDiff(n.Alias, f, path)
		return
	}
	if n.Kind == yaml.ScalarNode {
		if isSecretField(f, path) && n.Value != "" && !envRef.MatchString(n.Value) {
			n.Value = SecretKeep
			n.Tag = "!!str"
		}
		return
	}
	if isSecretField(f, path) || (f != nil && f.Item != nil && f.Item.Secret) || isSecret(path+"[]") {
		// Container at (or whose items are) a secret path: wholesale redact.
		redactDescendants(n)
		return
	}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i].Value
			cf := childSchema(f, k)
			if cf == nil && f != nil && f.Item != nil {
				// Malformed shape: mapping where an identity sequence belongs.
				cf = f.Item
			}
			child := k
			if path != "" {
				child = path + "." + k
			}
			redactForDiff(n.Content[i+1], cf, child)
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			redactForDiff(c, itemSchema(f), path+"[]")
		}
	}
}

// secretAfterText classifies an update at a secret path.
func secretAfterText(b, a *yaml.Node) string {
	bv, av := nodeScalar(b), nodeScalar(a)
	if bv == av {
		return "secret unchanged"
	}
	if av == "" {
		return "secret removed"
	}
	return "secret reference changed"
}

// nodeJSON converts a node to a JSON-friendly value for diffs.
func nodeJSON(n *yaml.Node) any {
	if n == nil {
		return nil
	}
	var v any
	if err := n.Decode(&v); err != nil {
		return nodeScalar(n)
	}
	return v
}

// encodeNode re-encodes an AST root with 2-space indent.
func encodeNode(root *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("encode candidate: %w", err)
	}
	enc.Close()
	return buf.Bytes(), nil
}

// --- Task 5: transactional commit, backup rotation, rollback --------------

var (
	ErrConflict         = errors.New("config conflict: expected revision is stale")
	ErrCandidateChanged = errors.New("candidate changed: re-validate before commit")
)

type CommitRequest struct {
	EditRequest
	CandidateRevision string `json:"candidate_revision"`
}

type CommitResult struct {
	Saved                bool     `json:"saved"`
	Applied              bool     `json:"applied"`
	Revision             string   `json:"revision"`
	RestartRequired      bool     `json:"restart_required"`
	RestartRequiredPaths []string `json:"restart_required_paths"`
	Restored             bool     `json:"restored,omitempty"`
}

type ApplyFunc func(context.Context, *Config, []string) error

// Commit validates, writes, and applies a candidate in one critical section.
// On apply failure the exact prior bytes are restored.
func (s *Store) Commit(ctx context.Context, req CommitRequest, apply ApplyFunc) (*CommitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", s.path, err)
	}
	if revisionOf(current) != req.ExpectedRevision {
		return nil, ErrConflict
	}
	candidate, err := s.validateAgainst(current, req.EditRequest)
	if err != nil {
		return nil, err
	}
	if candidate.CandidateRevision != req.CandidateRevision {
		return nil, ErrCandidateChanged
	}
	if candidate.RestartRequiredPaths == nil {
		candidate.RestartRequiredPaths = []string{}
	}
	res := &CommitResult{
		Revision:             candidate.CandidateRevision,
		RestartRequiredPaths: candidate.RestartRequiredPaths,
		RestartRequired:      len(candidate.RestartRequiredPaths) > 0,
	}
	if bytes.Equal(current, candidate.bytes) {
		return res, nil
	}
	if err := s.writeBackup(current); err != nil {
		return nil, err
	}
	if err := replaceFileAtomic(s.path, candidate.bytes); err != nil {
		return nil, fmt.Errorf("write config %s: %w", s.path, err)
	}
	res.Saved = true
	if err := apply(ctx, candidate.config, candidate.RestartRequiredPaths); err != nil {
		restoreErr := replaceFileAtomic(s.path, current)
		res.Restored = restoreErr == nil
		return res, errors.Join(fmt.Errorf("apply config: %w", err), restoreErr)
	}
	res.Applied = true
	s.rotateBackups()
	return res, nil
}

// writeBackup copies current bytes to <path>.bak.<timestamp> with the same
// permissions. Caller holds s.mu.
func (s *Store) writeBackup(current []byte) error {
	info, err := os.Stat(s.path)
	if err != nil {
		return fmt.Errorf("stat config %s: %w", s.path, err)
	}
	name := fmt.Sprintf("%s.bak.%d", s.path, time.Now().UnixNano())
	if err := os.WriteFile(name, current, info.Mode().Perm()); err != nil {
		return fmt.Errorf("write backup %s: %w", name, err)
	}
	return nil
}

// rotateBackups keeps the newest backupLimit .bak.* files. Caller holds s.mu.
func (s *Store) rotateBackups() {
	if s.backupLimit <= 0 {
		return
	}
	dir := filepath.Dir(s.path)
	base := filepath.Base(s.path) + ".bak."
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var baks []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), base) {
			baks = append(baks, e.Name())
		}
	}
	sort.Strings(baks) // timestamp suffix sorts oldest-first
	for len(baks) > s.backupLimit {
		os.Remove(filepath.Join(dir, baks[0]))
		baks = baks[1:]
	}
}

// replaceFileAtomic writes data to a temp file in the same directory, syncs,
// closes, and renames over path. On Windows the destination must be removed
// first; the temp file is moved into place only after that succeeds, and any
// failure removes the temp file.
func replaceFileAtomic(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		// Windows fallback: remove destination, then rename.
		if rmErr := os.Remove(path); rmErr != nil {
			return errors.Join(err, rmErr)
		}
		if err2 := os.Rename(tmpName, path); err2 != nil {
			return errors.Join(err, err2)
		}
	}
	return nil
}
