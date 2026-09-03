package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeStoreFixture(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const storeFixture = `# keep me
admin_key: literal-admin
providers:
  - name: p1
    api_key: ${P1_KEY}
    api_keys: [literal-a, "${P1_KEY_2}"]
`

func TestStoreReadPreservesReferencesAndRedactsLiterals(t *testing.T) {
	s := NewStore(writeStoreFixture(t, storeFixture), 5)

	snap, err := s.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(snap.RawYAML, "literal-admin") || strings.Contains(snap.RawYAML, "literal-a") {
		t.Fatal("literal secret leaked")
	}
	if !strings.Contains(snap.RawYAML, "${P1_KEY}") || !strings.Contains(snap.RawYAML, "# keep me") {
		t.Fatal("reference or comment lost")
	}
	if !strings.Contains(snap.RawYAML, SecretKeep) {
		t.Fatal("redaction marker missing")
	}

	// Document redacted the same way.
	doc, _ := json.Marshal(snap.Document)
	if strings.Contains(string(doc), "literal-admin") || strings.Contains(string(doc), "literal-a") {
		t.Fatalf("literal secret leaked in document: %s", doc)
	}
	providers := snap.Document["providers"].([]any)
	p1 := providers[0].(map[string]any)
	if p1["api_key"] != "${P1_KEY}" {
		t.Fatalf("env reference not preserved: %v", p1["api_key"])
	}
	if p1["name"] != "p1" {
		t.Fatalf("non-secret field mangled: %v", p1["name"])
	}
	if snap.Document["admin_key"] != SecretKeep {
		t.Fatalf("admin_key not redacted: %v", snap.Document["admin_key"])
	}

	// Whole snapshot JSON must not leak literals.
	blob, _ := json.Marshal(snap)
	if strings.Contains(string(blob), "literal-admin") || strings.Contains(string(blob), "literal-a") {
		t.Fatalf("literal secret leaked in snapshot JSON")
	}

	if snap.Schema == nil || len(snap.RestartRequiredPaths) == 0 {
		t.Fatal("schema or restart paths missing")
	}
}

func TestStoreRevisionIsSHA256OfOriginalBytes(t *testing.T) {
	p := writeStoreFixture(t, storeFixture)
	s := NewStore(p, 5)

	raw, _ := os.ReadFile(p)
	want := "sha256:" + hex.EncodeToString(sha256sum(raw))

	snap, err := s.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Revision != want {
		t.Fatalf("revision = %q, want %q", snap.Revision, want)
	}

	// Repeated reads are stable.
	snap2, err := s.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap2.Revision != snap.Revision {
		t.Fatal("revision not stable across reads")
	}
}

func TestStoreReadMissingFile(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "nope.yaml"), 5)
	snap, err := s.Read(context.Background())
	if err == nil || snap != nil {
		t.Fatalf("expected error, got snap=%v err=%v", snap, err)
	}
	// Error must carry the path, never file contents.
	if !strings.Contains(err.Error(), "nope.yaml") {
		t.Fatalf("error missing path: %v", err)
	}
}

func TestStoreReadInvalidYAML(t *testing.T) {
	p := writeStoreFixture(t, "a: [unclosed\n")
	s := NewStore(p, 5)
	if _, err := s.Read(context.Background()); err == nil {
		t.Fatal("expected parse error")
	} else if strings.Contains(err.Error(), "unclosed") {
		t.Fatalf("error leaked scalar value: %v", err)
	}
}

func sha256sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

// --- Task 5: transactional commit, backup rotation, rollback --------------

func commitFixture(t *testing.T) (*Store, string) {
	t.Helper()
	p := writeStoreFixture(t, candidateBase)
	return NewStore(p, 5), p
}

func commitEdit(rev string) EditRequest {
	return EditRequest{
		ExpectedRevision: rev,
		Mode:             "raw",
		RawYAML: `admin_key: ${ADMIN_KEY}
providers:
  - name: p1
    type: openai
    base_url: http://changed
    api_key: ${P1_KEY}
    priority: 5
  - name: p2
    type: anthropic
    base_url: http://y
    priority: 9
routes:
  - model: auto
    candidates:
      - provider: p1
        model: up
`,
	}
}

func okApply(context.Context, *Config, []string) error { return nil }

func TestStoreCommitStaleExpectedRevision(t *testing.T) {
	s, p := commitFixture(t)
	before, _ := os.ReadFile(p)

	_, err := s.Commit(context.Background(), CommitRequest{
		EditRequest:       commitEdit("sha256:stale"),
		CandidateRevision: "sha256:whatever",
	}, okApply)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(before, after) {
		t.Fatal("conflict mutated on-disk bytes")
	}
}

func TestStoreCommitRejectsWrongCandidateRevision(t *testing.T) {
	s, p := commitFixture(t)
	snap := readSnapshot(t, s)
	before, _ := os.ReadFile(p)

	_, err := s.Commit(context.Background(), CommitRequest{
		EditRequest:       commitEdit(snap.Revision),
		CandidateRevision: "sha256:wrong",
	}, okApply)
	if !errors.Is(err, ErrCandidateChanged) {
		t.Fatalf("expected ErrCandidateChanged, got %v", err)
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(before, after) {
		t.Fatal("candidate mismatch mutated on-disk bytes")
	}
}

func TestStoreCommitSuccessWritesCandidateAndAppliesOnce(t *testing.T) {
	s, p := commitFixture(t)
	snap := readSnapshot(t, s)
	before, _ := os.ReadFile(p)

	cand, err := s.Validate(context.Background(), commitEdit(snap.Revision))
	if err != nil {
		t.Fatal(err)
	}
	if cand.RestartRequiredPaths == nil {
		t.Fatal("admin validation returned nil restart paths; nil is reserved for SIGHUP")
	}
	var calls int
	var appliedRestartPaths []string
	res, err := s.Commit(context.Background(), CommitRequest{
		EditRequest:       commitEdit(snap.Revision),
		CandidateRevision: cand.CandidateRevision,
	}, func(ctx context.Context, cfg *Config, restart []string) error {
		calls++
		appliedRestartPaths = restart
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Saved || !res.Applied || res.Restored {
		t.Fatalf("unexpected result: %+v", res)
	}
	if calls != 1 {
		t.Fatalf("apply called %d times, want 1", calls)
	}
	if appliedRestartPaths == nil {
		t.Fatal("admin commit passed nil restart paths; nil is reserved for SIGHUP")
	}
	if res.Revision != cand.CandidateRevision {
		t.Fatalf("revision = %q, want %q", res.Revision, cand.CandidateRevision)
	}
	after, _ := os.ReadFile(p)
	if bytes.Equal(before, after) {
		t.Fatal("commit did not change on-disk bytes")
	}
	if string(after) != cand.RawYAML && revisionOf(after) != cand.CandidateRevision {
		t.Fatalf("on-disk bytes are not the candidate bytes")
	}
	assertNoTempFiles(t, filepath.Dir(p))
}

func TestStoreCommitFailedApplyRestoresExactBytes(t *testing.T) {
	s, p := commitFixture(t)
	snap := readSnapshot(t, s)
	before, _ := os.ReadFile(p)

	cand, err := s.Validate(context.Background(), commitEdit(snap.Revision))
	if err != nil {
		t.Fatal(err)
	}
	applyErr := errors.New("apply exploded")
	res, err := s.Commit(context.Background(), CommitRequest{
		EditRequest:       commitEdit(snap.Revision),
		CandidateRevision: cand.CandidateRevision,
	}, func(context.Context, *Config, []string) error { return applyErr })
	if err == nil || !errors.Is(err, applyErr) {
		t.Fatalf("expected apply error, got %v", err)
	}
	if res == nil || !res.Restored {
		t.Fatalf("expected Restored=true, got %+v", res)
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(before, after) {
		t.Fatal("rollback did not restore exact prior bytes")
	}
	assertNoTempFiles(t, filepath.Dir(p))
}

func TestStoreCommitNoChangeSkipsWriteAndApply(t *testing.T) {
	s, _ := commitFixture(t)
	snap := readSnapshot(t, s)

	cand, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision, Mode: "structured", Document: snap.Document})
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	res, err := s.Commit(context.Background(), CommitRequest{
		EditRequest: EditRequest{
			ExpectedRevision: snap.Revision, Mode: "structured", Document: snap.Document},
		CandidateRevision: cand.CandidateRevision,
	}, func(context.Context, *Config, []string) error { calls++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if res.Saved || res.Applied || calls != 0 {
		t.Fatalf("no-op commit wrote or applied: %+v calls=%d", res, calls)
	}
}

func TestBackupRotationKeepsFive(t *testing.T) {
	s, p := commitFixture(t)
	dir := filepath.Dir(p)

	for i := 0; i < 6; i++ {
		snap := readSnapshot(t, s)
		raw := strings.Replace(candidateBase, "http://x", "http://x"+strings.Repeat("x", i+1), 1)
		cand, err := s.Validate(context.Background(), EditRequest{
			ExpectedRevision: snap.Revision, Mode: "raw", RawYAML: raw})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Commit(context.Background(), CommitRequest{
			EditRequest:       EditRequest{ExpectedRevision: snap.Revision, Mode: "raw", RawYAML: raw},
			CandidateRevision: cand.CandidateRevision,
		}, okApply); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var baks []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "config.yaml.bak.") {
			baks = append(baks, e.Name())
		}
	}
	if len(baks) != 5 {
		t.Fatalf("expected 5 backups after 6 writes, got %d: %v", len(baks), baks)
	}
	assertNoTempFiles(t, dir)
}

func TestConcurrentCommitSerializesAndOneConflicts(t *testing.T) {
	s, p := commitFixture(t)
	snap := readSnapshot(t, s)
	raw := strings.Replace(candidateBase, "http://x", "http://changed", 1)
	edit := EditRequest{ExpectedRevision: snap.Revision, Mode: "raw", RawYAML: raw}
	cand, err := s.Validate(context.Background(), edit)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	type outcome struct {
		res *CommitResult
		err error
	}
	results := make(chan outcome, 2)
	commit := func() {
		<-start
		res, err := s.Commit(context.Background(), CommitRequest{
			EditRequest:       edit,
			CandidateRevision: cand.CandidateRevision,
		}, okApply)
		results <- outcome{res, err}
	}
	go commit()
	go commit()
	close(start)
	a, b := <-results, <-results

	var saved, conflicts int
	for _, o := range []outcome{a, b} {
		switch {
		case o.err == nil && o.res != nil && o.res.Saved:
			saved++
		case errors.Is(o.err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected outcome: %+v", o)
		}
	}
	if saved != 1 || conflicts != 1 {
		t.Fatalf("saved=%d conflicts=%d, want 1/1", saved, conflicts)
	}
	// Winner's bytes on disk; exactly one backup.
	after, _ := os.ReadFile(p)
	if revisionOf(after) != cand.CandidateRevision {
		t.Fatal("winner's candidate bytes not on disk")
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	var baks int
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "config.yaml.bak.") {
			baks++
		}
	}
	if baks != 1 {
		t.Fatalf("expected 1 backup, got %d", baks)
	}
	assertNoTempFiles(t, filepath.Dir(p))
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

// --- Task 4: candidate merge / validate / diff fixtures -------------------

// candidateBase is a fully valid config exercising comments, providers,
// routes, and a secret env reference.
const candidateBase = `# top comment
admin_key: ${ADMIN_KEY}
providers:
  # provider p1
  - name: p1
    type: openai
    base_url: http://x
    api_key: ${P1_KEY}
    priority: 5
  # provider p2
  - name: p2
    type: anthropic
    base_url: http://y
    priority: 9
routes:
  - model: auto
    candidates:
      - provider: p1
        model: up
`

func validateFixture(t *testing.T, body string) *Store {
	t.Helper()
	return NewStore(writeStoreFixture(t, body), 5)
}

func readSnapshot(t *testing.T, s *Store) *Snapshot {
	t.Helper()
	snap, err := s.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestStoreValidateStructuredNoOp(t *testing.T) {
	s := validateFixture(t, candidateBase)
	snap := readSnapshot(t, s)

	cand, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "structured",
		Document:         snap.Document,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(s.path)
	if cand.RawYAML != string(raw) {
		t.Fatalf("no-op must preserve original bytes:\n%s", cand.RawYAML)
	}
	if cand.CandidateRevision != snap.Revision {
		t.Fatalf("revision changed on no-op: %s -> %s", snap.Revision, cand.CandidateRevision)
	}
	if len(cand.Diff) != 0 || len(cand.ChangedPaths) != 0 {
		t.Fatalf("no-op produced diff: %+v", cand.Diff)
	}
}

func TestStoreValidateChangePreservesComments(t *testing.T) {
	s := validateFixture(t, candidateBase)
	snap := readSnapshot(t, s)

	doc := snap.Document
	doc["providers"].([]any)[0].(map[string]any)["priority"] = 1
	cand, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "structured",
		Document:         doc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := cand.RawYAML; !strings.Contains(got, "# provider p1") || !strings.Contains(got, "# top comment") {
		t.Fatalf("comments lost:\n%s", got)
	}
	if !strings.Contains(cand.RawYAML, "priority: 1") {
		t.Fatalf("edit not applied:\n%s", cand.RawYAML)
	}
	if strings.Contains(cand.RawYAML, "literal-secret") {
		t.Fatalf("redaction failed:\n%s", cand.RawYAML)
	}
	var found bool
	for _, ch := range cand.Diff {
		if ch.Path == "providers[p1].priority" && ch.Kind == "update" {
			found = true
		}
	}
	if !found {
		t.Fatalf("diff missing providers[p1].priority update: %+v", cand.Diff)
	}
}

func TestStructuredMergeReordersProvidersKeepingComments(t *testing.T) {
	s := validateFixture(t, candidateBase)
	snap := readSnapshot(t, s)

	providers := snap.Document["providers"].([]any)
	doc := snap.Document
	doc["providers"] = []any{providers[1], providers[0]}

	cand, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "structured",
		Document:         doc,
	})
	if err != nil {
		t.Fatal(err)
	}
	p1 := strings.Index(cand.RawYAML, "# provider p1")
	p2 := strings.Index(cand.RawYAML, "# provider p2")
	if p1 < 0 || p2 < 0 || p2 > p1 {
		t.Fatalf("reorder/comments broken (p1=%d p2=%d):\n%s", p1, p2, cand.RawYAML)
	}
}

func TestStructuredMergeAddsProviderInSchemaOrder(t *testing.T) {
	s := validateFixture(t, candidateBase)
	snap := readSnapshot(t, s)

	providers := snap.Document["providers"].([]any)
	doc := snap.Document
	doc["providers"] = append(providers, map[string]any{
		"priority": 7,
		"name":     "p3",
		"type":     "gemini",
		"base_url": "http://z",
	})

	cand, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "structured",
		Document:         doc,
	})
	if err != nil {
		t.Fatal(err)
	}
	ni := strings.Index(cand.RawYAML, "name: p3")
	ti := strings.Index(cand.RawYAML, "type: gemini")
	bi := strings.Index(cand.RawYAML, "base_url: http://z")
	pi := strings.Index(cand.RawYAML, "priority: 7")
	if !(ni >= 0 && ni < ti && ti < bi && bi < pi) {
		t.Fatalf("field order wrong (name=%d type=%d base=%d prio=%d):\n%s", ni, ti, bi, pi, cand.RawYAML)
	}
}

func TestStructuredMergeInsertsTopLevelFieldBySchemaOrder(t *testing.T) {
	s := validateFixture(t, candidateBase)
	snap := readSnapshot(t, s)
	doc := snap.Document
	doc["max_body_mb"] = 32

	cand, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "structured",
		Document:         doc,
	})
	if err != nil {
		t.Fatal(err)
	}
	admin := strings.Index(cand.RawYAML, "admin_key:")
	maxBody := strings.Index(cand.RawYAML, "max_body_mb: 32")
	providers := strings.Index(cand.RawYAML, "providers:")
	if !(admin >= 0 && admin < maxBody && maxBody < providers) {
		t.Fatalf("top-level field not inserted by schema order (admin=%d max_body=%d providers=%d):\n%s", admin, maxBody, providers, cand.RawYAML)
	}
}

func TestStructuredMergeInsertsNestedFieldBySchemaOrder(t *testing.T) {
	s := validateFixture(t, candidateBase)
	snap := readSnapshot(t, s)
	doc := snap.Document
	doc["providers"].([]any)[0].(map[string]any)["api_keys"] = []any{"${P1_ALT_KEY}"}

	cand, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "structured",
		Document:         doc,
	})
	if err != nil {
		t.Fatal(err)
	}
	p1Start := strings.Index(cand.RawYAML, "- name: p1")
	p2Start := strings.Index(cand.RawYAML, "- name: p2")
	p1 := cand.RawYAML[p1Start:p2Start]
	apiKey := strings.Index(p1, "api_key:")
	apiKeys := strings.Index(p1, "api_keys:")
	priority := strings.Index(p1, "priority: 5")
	if !(apiKey >= 0 && apiKey < apiKeys && apiKeys < priority) {
		t.Fatalf("nested field not inserted by schema order (api_key=%d api_keys=%d priority=%d):\n%s", apiKey, apiKeys, priority, p1)
	}
}

func TestStructuredMergeUntypedSequenceNoOp(t *testing.T) {
	s := validateFixture(t, `admin_key: ${ADMIN_KEY}
providers:
  - name: p1
    type: openai
    base_url: http://x
    api_key: ${P1_KEY}
    param_override:
      stop: [one, two]
routes:
  - model: auto
    candidates:
      - {provider: p1, model: up}
`)
	snap := readSnapshot(t, s)
	cand, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "structured",
		Document:         snap.Document,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cand.CandidateRevision != snap.Revision || len(cand.Diff) != 0 {
		t.Fatalf("untyped sequence no-op changed candidate: revision=%s diff=%+v", cand.CandidateRevision, cand.Diff)
	}
}

func TestStructuredMergeUpdatesUntypedNestedValues(t *testing.T) {
	fixture := `admin_key: ${ADMIN_KEY}
providers:
  - name: p1
    type: openai
    base_url: http://x
    api_key: ${P1_KEY}
    param_override:
      stop: [one, two]
      response_format:
        type: json_object
        options: [compact]
routes:
  - model: auto
    candidates:
      - {provider: p1, model: up}
`
	tests := []struct {
		name string
		edit func(map[string]any)
		want string
	}{
		{
			name: "sequence",
			edit: func(overrides map[string]any) { overrides["stop"] = []any{"one", "three"} },
			want: "three",
		},
		{
			name: "map containing sequence",
			edit: func(overrides map[string]any) {
				overrides["response_format"].(map[string]any)["options"] = []any{"pretty"}
			},
			want: "pretty",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := validateFixture(t, fixture)
			snap := readSnapshot(t, s)
			doc := snap.Document
			overrides := doc["providers"].([]any)[0].(map[string]any)["param_override"].(map[string]any)
			tc.edit(overrides)

			cand, err := s.Validate(context.Background(), EditRequest{
				ExpectedRevision: snap.Revision,
				Mode:             "structured",
				Document:         doc,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(cand.RawYAML, tc.want) {
				t.Fatalf("updated untyped value %q missing:\n%s", tc.want, cand.RawYAML)
			}
			if len(cand.Diff) == 0 {
				t.Fatal("updated untyped value produced no diff")
			}
		})
	}
}

func TestStructuredMergeRemovesRoute(t *testing.T) {
	base := candidateBase + `  - model: extra
    candidates:
      - provider: p2
        model: down
`
	s := validateFixture(t, base)
	snap := readSnapshot(t, s)

	routes := snap.Document["routes"].([]any)
	doc := snap.Document
	doc["routes"] = []any{routes[0]}

	cand, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "structured",
		Document:         doc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cand.RawYAML, "model: extra") || strings.Contains(cand.RawYAML, "model: down") {
		t.Fatalf("route not removed:\n%s", cand.RawYAML)
	}
	if !strings.Contains(cand.RawYAML, "model: auto") {
		t.Fatalf("kept route lost:\n%s", cand.RawYAML)
	}
}

const occurrenceIdentityBase = `admin_key: ${ADMIN_KEY}
search:
  # tavily first
  - backend: tavily
    api_key: ${TAVILY_ONE}
  # brave kept
  - backend: brave
    api_key: ${BRAVE_ONE}
  # tavily second
  - backend: tavily
    api_key: ${TAVILY_TWO}
  # exa removed
  - backend: exa
    api_key: ${EXA_ONE}
failure_rules:
  # overloaded first
  - match: overloaded
    cooldown_ms: 1000
  # status kept
  - status: 503
    cooldown_ms: 2000
  # overloaded second
  - match: overloaded
    cooldown_ms: 3000
  # status removed
  - status: 401
    cooldown_ms: 4000
`

func TestStructuredMergeOccurrenceIdentitiesPreservesCommentsAndDiffs(t *testing.T) {
	s := validateFixture(t, occurrenceIdentityBase)
	snap := readSnapshot(t, s)
	doc := snap.Document

	search := doc["search"].([]any)
	search[0].(map[string]any)["api_key"] = "${TAVILY_EDITED}"
	doc["search"] = []any{
		search[0], search[2], search[1],
		map[string]any{"backend": "tavily", "api_key": "${TAVILY_THREE}"},
	}

	rules := doc["failure_rules"].([]any)
	rules[0].(map[string]any)["cooldown_ms"] = 1500
	doc["failure_rules"] = []any{
		rules[0], rules[2], rules[1],
		map[string]any{"match": "overloaded", "cooldown_ms": 5000},
	}

	cand, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "structured",
		Document:         doc,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, comment := range []string{"# tavily first", "# tavily second", "# brave kept", "# overloaded first", "# overloaded second", "# status kept"} {
		if !strings.Contains(cand.RawYAML, comment) {
			t.Errorf("matched comment %q lost:\n%s", comment, cand.RawYAML)
		}
	}
	for _, removed := range []string{"# exa removed", "# status removed"} {
		if strings.Contains(cand.RawYAML, removed) {
			t.Errorf("removed occurrence comment %q retained:\n%s", removed, cand.RawYAML)
		}
	}
	for first, second := range map[string]string{
		"# tavily first":      "# tavily second",
		"# tavily second":     "# brave kept",
		"# overloaded first":  "# overloaded second",
		"# overloaded second": "# status kept",
	} {
		if strings.Index(cand.RawYAML, first) >= strings.Index(cand.RawYAML, second) {
			t.Errorf("occurrence order %q before %q not preserved:\n%s", first, second, cand.RawYAML)
		}
	}

	wantChanges := map[string]string{
		"search":                   "reorder",
		"search[tavily#1].api_key": "update",
		"search[tavily#3]":         "add",
		"search[exa#1]":            "remove",
		"failure_rules":            "reorder",
		"failure_rules[match=overloaded#1].cooldown_ms": "update",
		"failure_rules[match=overloaded#3]":             "add",
		"failure_rules[status=401#1]":                   "remove",
	}
	for _, change := range cand.Diff {
		if wantChanges[change.Path] == change.Kind {
			delete(wantChanges, change.Path)
		}
	}
	if len(wantChanges) != 0 {
		t.Fatalf("missing occurrence-aware changes %v; diff=%+v", wantChanges, cand.Diff)
	}
}

func TestStructuredMergeRemovesDuplicateOccurrences(t *testing.T) {
	s := validateFixture(t, occurrenceIdentityBase)
	snap := readSnapshot(t, s)
	doc := snap.Document

	search := doc["search"].([]any)
	doc["search"] = []any{search[0], search[1], search[3]}
	rules := doc["failure_rules"].([]any)
	doc["failure_rules"] = []any{rules[0], rules[1], rules[3]}

	cand, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "structured",
		Document:         doc,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, comment := range []string{"# tavily first", "# overloaded first"} {
		if !strings.Contains(cand.RawYAML, comment) {
			t.Errorf("kept occurrence comment %q lost:\n%s", comment, cand.RawYAML)
		}
	}
	for _, comment := range []string{"# tavily second", "# overloaded second"} {
		if strings.Contains(cand.RawYAML, comment) {
			t.Errorf("removed duplicate comment %q retained:\n%s", comment, cand.RawYAML)
		}
	}
	wantChanges := map[string]string{
		"search[tavily#2]":                  "remove",
		"failure_rules[match=overloaded#2]": "remove",
	}
	for _, change := range cand.Diff {
		if wantChanges[change.Path] == change.Kind {
			delete(wantChanges, change.Path)
		}
	}
	if len(wantChanges) != 0 {
		t.Fatalf("missing duplicate removals %v; diff=%+v", wantChanges, cand.Diff)
	}
}

func TestStoreValidateRestoresDuplicateSearchSentinelsByOccurrence(t *testing.T) {
	s := validateFixture(t, `admin_key: ${ADMIN_KEY}
search:
  - backend: tavily
    api_key: literal-first
  - backend: brave
    api_key: literal-brave
  - backend: tavily
    api_key: literal-second
`)
	snap := readSnapshot(t, s)
	doc := snap.Document
	search := doc["search"].([]any)
	doc["search"] = []any{search[0], search[2], search[1]}

	cand, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "structured",
		Document:         doc,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"literal-first", "literal-second", "literal-brave"}
	for i, entry := range cand.config.Search {
		if entry.APIKey != want[i] {
			t.Errorf("search[%d].APIKey = %q, want %q", i, entry.APIKey, want[i])
		}
	}

	search = snap.Document["search"].([]any)
	doc["search"] = append(search, map[string]any{"backend": "tavily", "api_key": SecretKeep})
	_, err = s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "structured",
		Document:         doc,
	})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != CodeSentinelMisuse {
		t.Fatalf("new duplicate sentinel error = %#v, want %s", err, CodeSentinelMisuse)
	}
}

func TestRawYAMLSyntaxErrorHasPosition(t *testing.T) {
	s := validateFixture(t, candidateBase)
	snap := readSnapshot(t, s)

	_, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "raw",
		RawYAML:          "a: [unclosed\n",
	})
	if err == nil {
		t.Fatal("expected syntax error")
	}
	if !strings.Contains(err.Error(), "line") || !strings.Contains(err.Error(), "column") {
		t.Fatalf("error lacks line/column: %v", err)
	}
}

func TestStoreValidateRejectsUnknownField(t *testing.T) {
	s := validateFixture(t, candidateBase)
	snap := readSnapshot(t, s)

	doc := snap.Document
	doc["bogus_field"] = true
	_, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "structured",
		Document:         doc,
	})
	if err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestStoreValidateRejectsBrokenProviderReference(t *testing.T) {
	s := validateFixture(t, candidateBase)
	snap := readSnapshot(t, s)

	routes := snap.Document["routes"].([]any)
	routes[0].(map[string]any)["candidates"] = []any{map[string]any{"provider": "nope", "model": "up"}}
	_, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "structured",
		Document:         snap.Document,
	})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("broken provider ref not rejected: %v", err)
	}
}

func TestStoreValidateRejectsStaleRevision(t *testing.T) {
	s := validateFixture(t, candidateBase)
	_, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: "sha256:stale",
		Mode:             "structured",
		Document:         map[string]any{},
	})
	if err == nil {
		t.Fatal("stale revision accepted")
	}
}

func TestStoreValidateRejectsMovedSecretKeepSentinel(t *testing.T) {
	s := validateFixture(t, candidateBase)
	snap := readSnapshot(t, s)

	// Move a sentinel: admin_key is ${ADMIN_KEY} (kept verbatim); use literal
	// secret fixture instead so the sentinel actually appears.
	s2 := validateFixture(t, `admin_key: literal-secret
providers:
  - name: p1
    type: openai
    base_url: http://x
    api_key: ${P1_KEY}
routes:
  - model: auto
    candidates:
      - {provider: p1, model: up}
`)
	snap2 := readSnapshot(t, s2)
	if snap2.Document["admin_key"] != SecretKeep {
		t.Fatalf("sentinel missing: %v", snap2.Document["admin_key"])
	}
	doc := snap2.Document
	doc["providers"].([]any)[0].(map[string]any)["base_url"] = SecretKeep // duplicated sentinel
	_, err := s2.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap2.Revision,
		Mode:             "structured",
		Document:         doc,
	})
	if err == nil {
		t.Fatal("duplicated sentinel accepted")
	}
	_ = s
	_ = snap
}

func TestStoreValidateRejectsLiteralSecretReplacement(t *testing.T) {
	s := validateFixture(t, `admin_key: literal-secret
providers:
  - name: p1
    type: openai
    base_url: http://x
routes:
  - model: auto
    candidates:
      - {provider: p1, model: up}
`)
	snap := readSnapshot(t, s)
	doc := snap.Document
	doc["admin_key"] = "brand-new-literal"
	_, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "structured",
		Document:         doc,
	})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != CodeLiteralSecretForbidden || ve.Path != "admin_key" {
		t.Fatalf("error = %#v, want literal_secret_forbidden at admin_key", err)
	}

	doc["admin_key"] = "${NEW_ADMIN_KEY}"
	if _, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision, Mode: "structured", Document: doc,
	}); err != nil {
		t.Fatalf("environment reference rejected: %v", err)
	}
	delete(doc, "admin_key")
	if _, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision, Mode: "structured", Document: doc,
	}); err != nil {
		t.Fatalf("removing a secret field should be allowed: %v", err)
	}
}

func TestStoreValidateRejectsLiteralSecretsAtConcretePaths(t *testing.T) {
	s := validateFixture(t, `admin_key: ${ADMIN_KEY}
providers:
  - name: p1
    type: openai
    base_url: http://x
    api_key: ${P1_KEY}
search:
  - backend: tavily
    api_key: ${SEARCH_KEY}
routes:
  - model: auto
    candidates:
      - {provider: p1, model: up}
`)
	snap := readSnapshot(t, s)
	tests := []struct {
		name string
		raw  string
		path string
	}{
		{"existing provider scalar", strings.Replace(testYAMLFromSnapshot(t, snap), "${P1_KEY}", "literal-provider", 1), "providers[p1].api_key"},
		{"existing search scalar", strings.Replace(testYAMLFromSnapshot(t, snap), "${SEARCH_KEY}", "literal-search", 1), "search[tavily#1].api_key"},
		{"new provider array", strings.Replace(testYAMLFromSnapshot(t, snap), "search:", "  - name: p2\n    type: openai\n    base_url: http://y\n    api_keys: [literal-pool]\nsearch:", 1), "providers[p2].api_keys[0]"},
		{"new duplicate search array", strings.Replace(testYAMLFromSnapshot(t, snap), "routes:", "  - backend: tavily\n    api_keys: [literal-search-pool]\nroutes:", 1), "search[tavily#2].api_keys[0]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Validate(context.Background(), EditRequest{ExpectedRevision: snap.Revision, Mode: "raw", RawYAML: tc.raw})
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != CodeLiteralSecretForbidden || ve.Path != tc.path {
				t.Fatalf("error = %#v, want literal_secret_forbidden at %s", err, tc.path)
			}
			if strings.Contains(err.Error(), "literal-") {
				t.Fatalf("error leaked secret: %v", err)
			}
		})
	}
}

func testYAMLFromSnapshot(t *testing.T, snap *Snapshot) string {
	t.Helper()
	return snap.RawYAML
}

func TestDiffSecretClassifications(t *testing.T) {
	mk := func(adminVal string) (*Store, *Snapshot) {
		s := validateFixture(t, adminVal+`
providers:
  - name: p1
    type: openai
    base_url: http://x
    api_key: ${P1_KEY}
routes:
  - model: auto
    candidates:
      - {provider: p1, model: up}
`)
		return s, readSnapshot(t, s)
	}

	// Unchanged env-ref secret: no diff entry at all.
	s, snap := mk("admin_key: ${ADMIN_KEY}")
	cand, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision, Mode: "structured", Document: snap.Document})
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range cand.Diff {
		blob := fmtAny(ch.Before) + fmtAny(ch.After)
		if strings.Contains(blob, "${P1_KEY}") || strings.Contains(blob, "${ADMIN_KEY}") {
			t.Fatalf("secret value leaked in diff: %+v", ch)
		}
	}

	// Changed reference: diff says "secret reference changed".
	s2, snap2 := mk("admin_key: ${ADMIN_KEY}")
	doc := snap2.Document
	doc["providers"].([]any)[0].(map[string]any)["api_key"] = "${OTHER_KEY}"
	cand2, err := s2.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap2.Revision, Mode: "structured", Document: doc})
	if err != nil {
		t.Fatal(err)
	}
	var refChange *Change
	for i, ch := range cand2.Diff {
		if strings.Contains(ch.Path, "api_key") {
			refChange = &cand2.Diff[i]
		}
	}
	if refChange == nil {
		t.Fatalf("no api_key diff entry: %+v", cand2.Diff)
	}
	blob := fmtAny(refChange.Before) + fmtAny(refChange.After)
	if strings.Contains(blob, "${OTHER_KEY}") || strings.Contains(blob, "${P1_KEY}") {
		t.Fatalf("reference values leaked: %+v", refChange)
	}
	if !strings.Contains(blob, "secret") {
		t.Fatalf("diff should classify secret change: %+v", refChange)
	}
}

func fmtAny(v any) string {
	if v == nil {
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// literalDiffBase has a literal (non-env) secret nested inside a provider,
// so an add/remove/shape-change of the whole container would serialize it.
const literalDiffBase = `admin_key: ${ADMIN_KEY}
providers:
  - name: p1
    type: openai
    base_url: http://x
    api_key: literal-p1-secret
    api_keys:
      - literal-key-one
      - ${P1_ALT}
routes:
  - model: auto
    candidates:
      - {provider: p1, model: up}
`

func assertDiffHasNoLiterals(t *testing.T, cand *Candidate, literals ...string) {
	t.Helper()
	for _, ch := range cand.Diff {
		blob := fmtAny(ch.Before) + fmtAny(ch.After)
		for _, lit := range literals {
			if strings.Contains(blob, lit) {
				t.Fatalf("literal %q leaked in diff change %+v", lit, ch)
			}
		}
	}
}

func TestDiffSecretListUpdateRedactsLiteralItems(t *testing.T) {
	s := validateFixture(t, literalDiffBase)
	snap := readSnapshot(t, s)

	doc := snap.Document
	provider := doc["providers"].([]any)[0].(map[string]any)
	provider["api_keys"] = []any{SecretKeep} // remove the env-ref item

	cand, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision, Mode: "structured", Document: doc})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ch := range cand.Diff {
		if ch.Path == "providers[p1].api_keys" && ch.Kind == "update" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected api_keys update: %+v", cand.Diff)
	}
	assertDiffHasNoLiterals(t, cand, "literal-key-one", "literal-p1-secret")
}

func TestDiffAddProviderRedactsNestedSecrets(t *testing.T) {
	s := validateFixture(t, literalDiffBase)
	snap := readSnapshot(t, s)

	doc := snap.Document
	providers := doc["providers"].([]any)
	providers = append(providers, map[string]any{
		"name":     "p3",
		"type":     "openai",
		"base_url": "http://z",
		"api_key":  "${P3_KEY}",
	})
	doc["providers"] = providers

	cand, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision, Mode: "structured", Document: doc})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ch := range cand.Diff {
		if ch.Path == "providers[p3]" && ch.Kind == "add" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected add change at providers[p3]: %+v", cand.Diff)
	}
	assertDiffHasNoLiterals(t, cand, "literal-p3-secret", "literal-p1-secret", "literal-key-one")
}

func TestDiffRemoveProviderRedactsNestedSecrets(t *testing.T) {
	s := validateFixture(t, literalDiffBase)
	snap := readSnapshot(t, s)

	doc := snap.Document
	doc["providers"] = []any{} // drop p1 entirely
	doc["routes"] = []any{}    // routes reference p1; drop them too

	cand, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision, Mode: "structured", Document: doc})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ch := range cand.Diff {
		if ch.Path == "providers[p1]" && ch.Kind == "remove" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected remove change at providers[p1]: %+v", cand.Diff)
	}
	assertDiffHasNoLiterals(t, cand, "literal-p1-secret", "literal-key-one")
}

func TestDiffContainerShapeChangeRedactsNestedSecrets(t *testing.T) {
	// semanticDiff must redact nested secrets even when a node's kind changes
	// (scalar/sequence/mapping) and the whole container is serialized once.
	parse := func(body string) *yaml.Node {
		t.Helper()
		root, err := parseDocument([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return root
	}
	base := parse(`providers:
  - name: p1
    api_key: literal-p1-secret
`)
	cand := parse(`providers:
  p1:
    api_key: literal-nested
    ref: ${P1_REF}
`)
	diff := semanticDiff(base, cand, FormSchema())
	if len(diff) == 0 {
		t.Fatal("expected a shape-change diff")
	}
	for _, ch := range diff {
		blob := fmtAny(ch.Before) + fmtAny(ch.After)
		for _, lit := range []string{"literal-p1-secret", "literal-nested"} {
			if strings.Contains(blob, lit) {
				t.Fatalf("literal %q leaked in diff change %+v", lit, ch)
			}
		}
	}
}

func TestStoreValidateRejectsEmptyRaw(t *testing.T) {
	s := validateFixture(t, candidateBase)
	snap := readSnapshot(t, s)

	for _, raw := range []string{"", "   \n\n", "null\n", "~\n"} {
		if _, err := s.Validate(context.Background(), EditRequest{
			ExpectedRevision: snap.Revision, Mode: "raw", RawYAML: raw}); err == nil {
			t.Fatalf("empty/non-mapping raw %q accepted", raw)
		}
	}
}

// Malformed config may turn a secret field into a sequence or map. Every
// literal scalar descendant under the secret path must still be redacted.
func TestStoreReadRedactsContainerUnderSecretPath(t *testing.T) {
	s := NewStore(writeStoreFixture(t, `admin_key:
  - literal-nested-a
  - ${ADMIN_REF}
providers:
  - name: p1
    api_key:
      user: literal-nested-b
      ref: ${P1_REF}
      deep:
        - literal-nested-c
`), 5)

	snap, err := s.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, lit := range []string{"literal-nested-a", "literal-nested-b", "literal-nested-c"} {
		if strings.Contains(snap.RawYAML, lit) {
			t.Fatalf("literal %q leaked in RawYAML:\n%s", lit, snap.RawYAML)
		}
	}
	if !strings.Contains(snap.RawYAML, "${ADMIN_REF}") || !strings.Contains(snap.RawYAML, "${P1_REF}") {
		t.Fatalf("env reference lost:\n%s", snap.RawYAML)
	}
	blob, _ := json.Marshal(snap)
	for _, lit := range []string{"literal-nested-a", "literal-nested-b", "literal-nested-c"} {
		if strings.Contains(string(blob), lit) {
			t.Fatalf("literal %q leaked in snapshot JSON: %s", lit, blob)
		}
	}
}

// A malformed mapping at a secret *list* field (providers[].api_keys,
// search[].api_keys) must also be redacted: the schema marks the scalar
// item path (path+"[]"), so a map at the bare path is otherwise missed.
func TestStoreReadRedactsMalformedMapAtSecretListPath(t *testing.T) {
	s := NewStore(writeStoreFixture(t, `providers:
  - name: p1
    api_keys:
      primary: literal-prov-a
      backup: ${P1_BACKUP}
search:
  - backend: tavily
    api_keys:
      primary: literal-search-a
      nested:
        - literal-search-b
        - ${SEARCH_REF}
`), 5)

	snap, err := s.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, lit := range []string{"literal-prov-a", "literal-search-a", "literal-search-b"} {
		if strings.Contains(snap.RawYAML, lit) {
			t.Fatalf("literal %q leaked in RawYAML:\n%s", lit, snap.RawYAML)
		}
	}
	if !strings.Contains(snap.RawYAML, "${P1_BACKUP}") || !strings.Contains(snap.RawYAML, "${SEARCH_REF}") {
		t.Fatalf("env reference lost:\n%s", snap.RawYAML)
	}
	// Mapping keys under the malformed field survive.
	if !strings.Contains(snap.RawYAML, "primary") || !strings.Contains(snap.RawYAML, "backup") {
		t.Fatalf("mapping keys mangled:\n%s", snap.RawYAML)
	}
	blob, _ := json.Marshal(snap)
	for _, lit := range []string{"literal-prov-a", "literal-search-a", "literal-search-b"} {
		if strings.Contains(string(blob), lit) {
			t.Fatalf("literal %q leaked in snapshot JSON: %s", lit, blob)
		}
	}
}
