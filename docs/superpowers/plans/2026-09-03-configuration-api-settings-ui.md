# Configuration API and Settings UI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a complete, safe, schema-driven configuration API and Nested Settings UI while keeping `config.yaml` as TokenRoute's source of truth.

**Architecture:** A configuration store owns YAML AST preservation, revision checks, redaction, validation, backups, and atomic replacement. The admin server exposes full-document read/validate/write endpoints and calls a reusable runtime apply coordinator. The dependency-free embedded dashboard renders server-provided schema recursively, adds a Raw YAML escape hatch, and requires validate/diff/confirm before writes.

**Tech Stack:** Go 1.27.0, `gopkg.in/yaml.v3`, `net/http`, chi v5, embedded HTML/CSS/vanilla JavaScript, Go `testing`, browser verification.

**Spec:** `docs/superpowers/specs/2026-09-03-configuration-api-settings-ui-design.md`

## Global Constraints

- `config.yaml` remains the sole persistent configuration source.
- Preserve YAML comments and ordering through `yaml.Node`; no-op saves must be byte-identical.
- Never return resolved or literal secret values.
- Use full-document revisions and reject stale writes with HTTP 409.
- Persist with same-directory temporary files, atomic replacement, five backups, and rollback.
- Keep `listen`, `admin_listen`, `usage_db`, and `admin_key` restart-required.
- No automatic process restart.
- No new Go or frontend dependencies.
- All source, tests, comments, logs, and documentation are English.
- Agents do not run git commands in this repository; each task ends at a verified review checkpoint.

## File Structure

### New files

- `internal/config/schema.go` — reflection-derived form schema plus explicit UI, secret, restart, enum, and sequence-identity metadata.
- `internal/config/schema_test.go` — schema coverage, metadata, enum, secret, and restart tests.
- `internal/config/store.go` — snapshots, revisions, redaction, candidate generation, AST merge, semantic diff, transactional writes, backups, rollback.
- `internal/config/store_test.go` — preservation, secret, conflict, validation, backup, and rollback tests.
- `internal/server/config_admin.go` — `/admin/config` HTTP request/response types and handlers.
- `internal/server/config_admin_test.go` — authentication, GET, validate, PUT, errors, conflict, and concurrent-write tests.
- `internal/server/dashboard_test.go` — embedded asset, schema coverage marker, key-field, and route-control smoke tests.
- `internal/server/web/dashboard.html` — dashboard document shell and accessible modal structure.
- `internal/server/web/dashboard.css` — responsive TokenRoute design system and component styles.
- `internal/server/web/dashboard.js` — existing operations UI plus recursive Settings renderer and save state machine.
- `cmd/tokenroute/reloader.go` — reusable runtime build/swap coordinator shared by SIGHUP and admin PUT.
- `cmd/tokenroute/reloader_test.go` — hot-field application, restart-field deferral, and failed-build preservation tests.

### Modified files

- `internal/config/config.go` — expose strict byte decoding and preserve environment expansion as a runtime-only step.
- `internal/config/config_test.go` — unknown-field and duplicate-key regression tests.
- `internal/server/server.go:42-138` — add configuration dependencies to `Options`, `srv`, and admin routes.
- `internal/server/admin.go:21-43` — separate dashboard bootstrap query authentication from header-only API authentication.
- `internal/server/dashboard.go` — replace the monolithic string with `go:embed` assets and asset handlers.
- `cmd/tokenroute/main.go:36-60,349-545` — initialize the configuration store and runtime reloader; route SIGHUP and admin writes through one coordinator.
- `README.md` — document configuration endpoints, save lifecycle, backups, secret behavior, and restart-required fields.
- `AGENTS.md` — update commands, architecture map, configuration workflow, boundaries, and troubleshooting.
- `CLAUDE.md` — mirror the maintained agent guidance.
- `.gitignore` — ignore `.superpowers/` brainstorming artifacts.

---

### Task 1: Strict Configuration Decoding

**Files:**
- Modify: `internal/config/config.go:277-309`
- Modify: `internal/config/config_test.go`

**Interfaces:**
- Produces: `func Decode(data []byte, expandEnv bool) (*Config, error)`
- Produces: `func Load(path string) (*Config, error)` as the file-reading wrapper.
- Consumes: existing `Config.Validate()`.

- [ ] **Step 1: Write failing strict-decoding tests**

Add tests proving unknown and duplicate fields fail, while an environment placeholder remains unchanged when `expandEnv=false`:

```go
func TestDecodeRejectsUnknownField(t *testing.T) {
	_, err := Decode([]byte("unknown_option: true\n"), false)
	if err == nil || !strings.Contains(err.Error(), "field unknown_option not found") {
		t.Fatalf("error = %v", err)
	}
}

func TestDecodeRejectsDuplicateKey(t *testing.T) {
	_, err := Decode([]byte("listen: :8400\nlisten: :9400\n"), false)
	if err == nil || !strings.Contains(err.Error(), "already defined") {
		t.Fatalf("error = %v", err)
	}
}

func TestDecodeCanPreserveEnvironmentReferences(t *testing.T) {
	cfg, err := Decode([]byte("admin_key: ${GATEWAY_ADMIN_KEY}\n"), false)
	if err != nil { t.Fatal(err) }
	if cfg.AdminKey != "${GATEWAY_ADMIN_KEY}" { t.Fatalf("admin_key = %q", cfg.AdminKey) }
}
```

Add `strings` to the test imports.

- [ ] **Step 2: Run the focused tests and verify RED**

Run:

```bash
go test ./internal/config -run 'TestDecodeRejects|TestDecodeCanPreserve' -count=1
```

Expected: build failure because `Decode` does not exist.

- [ ] **Step 3: Implement strict decoding once**

Implement this shape and move environment expansion behind the flag:

```go
func Decode(data []byte, expandEnv bool) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if expandEnv {
		expandConfigEnv(&cfg)
	}
	if cfg.Listen == "" { cfg.Listen = ":8400" }
	if cfg.UsageDB == "" { cfg.UsageDB = "data/usage.db" }
	if err := cfg.Validate(); err != nil { return nil, err }
	return &cfg, nil
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil { return nil, fmt.Errorf("read config: %w", err) }
	return Decode(data, true)
}
```

`expandConfigEnv` must expand `AdminKey`, provider `APIKey`/`APIKeys`, and search `APIKey`/`APIKeys` exactly as current `Load` does.

- [ ] **Step 4: Verify focused and package tests GREEN**

Run:

```bash
gofmt -w internal/config/config.go internal/config/config_test.go
go test ./internal/config -count=1
```

Expected: PASS.

- [ ] **Step 5: Review checkpoint**

Confirm `Load` behavior is unchanged for runtime callers, while `Decode(data, false)` never expands environment variables.

---

### Task 2: Generate a Complete Form Schema

**Files:**
- Create: `internal/config/schema.go`
- Create: `internal/config/schema_test.go`

**Interfaces:**
- Produces:

```go
type FieldSchema struct {
	Path            string         `json:"path"`
	Name            string         `json:"name"`
	Label           string         `json:"label"`
	Help            string         `json:"help,omitempty"`
	Kind            string         `json:"kind"`
	Section         string         `json:"section,omitempty"`
	Advanced        bool           `json:"advanced,omitempty"`
	Secret          bool           `json:"secret,omitempty"`
	RestartRequired bool           `json:"restart_required,omitempty"`
	Enum            []string       `json:"enum,omitempty"`
	Identity        []string       `json:"identity,omitempty"`
	VisibleWhen     *Visibility    `json:"visible_when,omitempty"`
	Children        []*FieldSchema `json:"children,omitempty"`
	Item            *FieldSchema   `json:"item,omitempty"`
}

type Visibility struct {
	Path   string   `json:"path"`
	Values []string `json:"values"`
}

func FormSchema() *FieldSchema
func RestartRequiredPaths() []string
func IsSecretPath(path string) bool
```

- [ ] **Step 1: Write failing schema coverage tests**

Use reflection in the test to collect every `yaml` tag reachable from `Config`, including nested struct, pointer, slice, and map value types. Assert the generated schema exposes all collected paths.

```go
func TestFormSchemaCoversEveryYAMLField(t *testing.T) {
	want := collectYAMLPaths(reflect.TypeOf(Config{}), "")
	got := flattenSchemaPaths(FormSchema())
	for path := range want {
		if !got[path] { t.Errorf("schema missing %s", path) }
	}
}
```

Also assert:

```go
func TestFormSchemaCriticalMetadata(t *testing.T) {
	assertSecret(t, "providers[].api_key")
	assertSecret(t, "search[].api_keys[]")
	assertRestart(t, "listen")
	assertRestart(t, "admin_key")
	assertEnum(t, "routes[].strategy", "priority", "fusion_judge", "consistent_hash")
	assertIdentity(t, "providers", "name")
	assertIdentity(t, "routes", "model")
}
```

- [ ] **Step 2: Run schema tests and verify RED**

Run:

```bash
go test ./internal/config -run 'TestFormSchema' -count=1
```

Expected: build failure because schema types/functions do not exist.

- [ ] **Step 3: Implement reflection plus the metadata table**

Use one recursive builder over `reflect.TypeOf(Config{})`. Derive kind and children from Go types. Keep explicit metadata in one map keyed by wildcard schema paths:

```go
var fieldMeta = map[string]fieldMetadata{
	"listen":                    {Section: "general", RestartRequired: true},
	"admin_listen":              {Section: "general", RestartRequired: true},
	"usage_db":                  {Section: "general", RestartRequired: true},
	"admin_key":                 {Section: "general", Secret: true, RestartRequired: true},
	"providers":                 {Section: "providers", Identity: []string{"name"}},
	"providers[].type":          {Enum: []string{"openai", "anthropic", "gemini"}},
	"providers[].api_key":       {Secret: true},
	"providers[].api_keys[]":    {Secret: true},
	"routes":                    {Section: "routes", Identity: []string{"model"}},
	"routes[].strategy":         {Enum: strategyNames()},
	"routes[].candidates":       {Identity: []string{"provider", "model"}},
	"prices":                    {Section: "pricing"},
	"free_tier":                 {Section: "pricing", Identity: []string{"provider", "model"}},
	"aliases":                   {Section: "pricing"},
	"group_ratio":               {Section: "pricing"},
	"retry_policy":              {Section: "resilience"},
	"failure_rules":             {Section: "resilience", Identity: []string{"match", "status"}},
	"search":                    {Section: "search", Identity: []string{"backend"}},
	"search[].api_key":          {Secret: true},
	"search[].api_keys[]":       {Secret: true},
}
```

Every field without explicit section metadata inherits its parent section. Every otherwise unclassified root field falls into `advanced`. Generate labels from snake_case and use concise hard-coded help only where behavior is non-obvious.

- [ ] **Step 4: Verify schema coverage GREEN**

Run:

```bash
gofmt -w internal/config/schema.go internal/config/schema_test.go
go test ./internal/config -run 'TestFormSchema|TestSecret|TestRestart|TestIdentity' -count=1
```

Expected: PASS with every current YAML field covered.

- [ ] **Step 5: Review checkpoint**

Serialize `FormSchema()` in a test and verify it contains no runtime secret values and no unsupported provider type or router strategy.

---

### Task 3: Read Snapshots and Redact Secrets

**Files:**
- Create: `internal/config/store.go`
- Create: `internal/config/store_test.go`

**Interfaces:**
- Produces:

```go
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

func NewStore(path string, backupLimit int) *Store
func (s *Store) Read(ctx context.Context) (*Snapshot, error)
```

- [ ] **Step 1: Write failing snapshot tests**

Build a temporary YAML file containing comments, `${ENV_VAR}` references, and literal secrets. Assert:

```go
func TestStoreReadPreservesReferencesAndRedactsLiterals(t *testing.T) {
	s := NewStore(writeStoreFixture(t, `# keep me
admin_key: literal-admin
providers:
  - name: p1
    api_key: ${P1_KEY}
    api_keys: [literal-a, ${P1_KEY_2}]
`), 5)
	snap, err := s.Read(context.Background())
	if err != nil { t.Fatal(err) }
	if strings.Contains(snap.RawYAML, "literal-admin") || strings.Contains(snap.RawYAML, "literal-a") {
		t.Fatal("literal secret leaked")
	}
	if !strings.Contains(snap.RawYAML, "${P1_KEY}") || !strings.Contains(snap.RawYAML, "# keep me") {
		t.Fatal("reference or comment lost")
	}
}
```

Assert the revision equals `"sha256:" + hex(sha256(originalBytes))` and repeated reads are stable.

- [ ] **Step 2: Run focused tests and verify RED**

Run:

```bash
go test ./internal/config -run 'TestStoreRead|TestRevision' -count=1
```

Expected: build failure because `Store` does not exist.

- [ ] **Step 3: Implement snapshot parsing and redaction**

Parse exact bytes into a `yaml.Node`. Walk scalar values using normalized wildcard paths. For secret fields:

- Preserve values matching exactly `$NAME` or `${NAME}`.
- Replace all other non-empty values with `SecretKeep` in cloned nodes.
- Never mutate the original AST.

Decode the sanitized clone into `map[string]any` for `Document`; encode the same clone for `RawYAML`. Revision must hash original bytes, not normalized YAML.

- [ ] **Step 4: Verify no secret leakage**

Run:

```bash
gofmt -w internal/config/store.go internal/config/store_test.go
go test ./internal/config -run 'TestStoreRead|TestRevision|TestSecret' -count=1
```

Expected: PASS. Add a final assertion that marshaling the entire `Snapshot` does not contain fixture literals.

- [ ] **Step 5: Review checkpoint**

Confirm logs and returned errors include paths only, never scalar secret values.

---

### Task 4: Build Candidates with AST-Preserving Merge and Redacted Diff

**Files:**
- Modify: `internal/config/store.go`
- Modify: `internal/config/store_test.go`

**Interfaces:**
- Produces:

```go
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

func (s *Store) Validate(ctx context.Context, req EditRequest) (*Candidate, error)
```

- [ ] **Step 1: Write preservation and validation tests**

Cover these exact cases:

1. Structured no-op returns original bytes and identical revision.
2. Changing `providers[p1].priority` preserves top-level and sibling comments.
3. Reordering providers keeps each provider's attached comments.
4. Adding a provider follows schema field order.
5. Removing a route removes only its AST item.
6. Raw YAML syntax errors return line and column.
7. Unknown fields and broken provider references fail.
8. A moved or duplicated `SecretKeep` sentinel fails.
9. Diffs report `secret unchanged`, `secret reference changed`, or `secret removed`, never values.

The central assertion:

```go
if got := candidate.RawYAML; !strings.Contains(got, "# provider p1") || strings.Contains(got, "literal-secret") {
	t.Fatalf("preservation/redaction failed:\n%s", got)
}
```

- [ ] **Step 2: Run candidate tests and verify RED**

Run:

```bash
go test ./internal/config -run 'TestStoreValidate|TestStructuredMerge|TestRawYAML|TestDiff' -count=1
```

Expected: build failure because `Validate`, `EditRequest`, and `Candidate` do not exist.

- [ ] **Step 3: Implement merge and validation helpers**

Implement focused unexported helpers:

```go
func parseDocument(data []byte) (*yaml.Node, error)
func cloneNode(n *yaml.Node) *yaml.Node
func mergeNode(base, desired *yaml.Node, schema *FieldSchema, path string) error
func restoreSecrets(current, candidate *yaml.Node, schema *FieldSchema, path string) error
func semanticDiff(before, after *yaml.Node, schema *FieldSchema) []Change
func encodeNode(root *yaml.Node) ([]byte, error)
```

Mapping merge matches keys. Sequence merge uses schema `Identity`; it must reject duplicate identities. Preserve the original byte slice immediately when semantic diff is empty. Decode candidate bytes twice: `Decode(bytes, false)` for safe presentation checks and `Decode(bytes, true)` for runtime validation.

- [ ] **Step 4: Verify candidate behavior GREEN**

Run:

```bash
gofmt -w internal/config/store.go internal/config/store_test.go
go test ./internal/config -run 'TestStoreValidate|TestStructuredMerge|TestRawYAML|TestDiff' -count=1
```

Expected: PASS.

- [ ] **Step 5: Review checkpoint**

Inspect every `Change` generated by secret fixtures. Only classification text may appear in `Before` or `After`.

---

### Task 5: Transactional Writes, Backups, and Rollback

**Files:**
- Modify: `internal/config/store.go`
- Modify: `internal/config/store_test.go`

**Interfaces:**
- Produces:

```go
type CommitRequest struct {
	EditRequest
	CandidateRevision string `json:"candidate_revision"`
}

type CommitResult struct {
	Saved               bool     `json:"saved"`
	Applied             bool     `json:"applied"`
	Revision            string   `json:"revision"`
	RestartRequired     bool     `json:"restart_required"`
	RestartRequiredPaths []string `json:"restart_required_paths"`
	Restored            bool     `json:"restored,omitempty"`
}

type ApplyFunc func(context.Context, *Config, []string) error

func (s *Store) Commit(ctx context.Context, req CommitRequest, apply ApplyFunc) (*CommitResult, error)
```

- [ ] **Step 1: Write failing transaction tests**

Add tests for:

- stale `ExpectedRevision` returns typed `ErrConflict` and preserves bytes;
- incorrect `CandidateRevision` returns `ErrCandidateChanged`;
- two concurrent commits serialize and one conflicts;
- successful commit writes candidate bytes and calls `apply` once;
- failed `apply` restores the exact prior bytes and returns `Restored=true`;
- six successful writes leave five `.bak.*` files;
- temp files are absent after success and failure.

Use channels in the concurrent test so ordering is deterministic.

- [ ] **Step 2: Run transaction tests and verify RED**

Run:

```bash
go test ./internal/config -run 'TestStoreCommit|TestBackupRotation|TestConcurrentCommit' -count=1
```

Expected: build failure because `Commit` does not exist.

- [ ] **Step 3: Implement the transaction**

Within one `s.mu` critical section:

```go
current, err := os.ReadFile(s.path)
if err != nil { return nil, err }
if revision(current) != req.ExpectedRevision { return nil, ErrConflict }
candidate, err := s.validateAgainst(current, req.EditRequest)
if err != nil { return nil, err }
if candidate.CandidateRevision != req.CandidateRevision { return nil, ErrCandidateChanged }
if bytes.Equal(current, candidate.bytes) { return unchangedResult(candidate), nil }
backup, err := s.writeBackup(current)
if err != nil { return nil, err }
if err := replaceFileAtomic(s.path, candidate.bytes); err != nil { return nil, err }
if err := apply(ctx, candidate.config, candidate.RestartRequiredPaths); err != nil {
	restoreErr := replaceFileAtomic(s.path, current)
	return failedApplyResult(restoreErr == nil), errors.Join(err, restoreErr)
}
s.rotateBackups()
return successResult(candidate), nil
```

Create the temp file in `filepath.Dir(s.path)`, copy current permissions, call `Sync`, close, and rename. On Windows, remove the destination only inside a guarded replace fallback after the backup exists; if replacement fails, restore from the captured bytes.

- [ ] **Step 4: Verify transaction tests GREEN**

Run:

```bash
gofmt -w internal/config/store.go internal/config/store_test.go
go test -race ./internal/config -run 'TestStoreCommit|TestBackupRotation|TestConcurrentCommit' -count=1
```

Expected: PASS with no race report.

- [ ] **Step 5: Review checkpoint**

Verify every test compares exact bytes before and after failure, not only decoded values.

---

### Task 6: Add Header-Only Configuration Endpoints

**Files:**
- Create: `internal/server/config_admin.go`
- Create: `internal/server/config_admin_test.go`
- Modify: `internal/server/server.go:42-138`
- Modify: `internal/server/admin.go:21-43`

**Interfaces:**
- Consumes: `*config.Store`, `config.ApplyFunc`.
- Extends `server.Options`:

```go
ConfigStore *config.Store
ApplyConfig config.ApplyFunc
```

- Produces routes:

```text
GET  /admin/config
POST /admin/config/validate
PUT  /admin/config
```

- [ ] **Step 1: Write failing API tests**

Create a test handler with a temporary config store and recording apply callback. Verify:

```go
func TestAdminConfigRejectsQueryAuthentication(t *testing.T) {
	h := configAdminSetup(t)
	rec := adminReq(t, h, http.MethodGet, "/admin/config?key="+testAdminKey, "", "")
	if rec.Code != http.StatusUnauthorized { t.Fatalf("status = %d", rec.Code) }
}
```

Also test GET redaction, validate side-effect freedom, PUT success, 409 conflict, 422 field errors, candidate hash mismatch, and rollback response fields.

- [ ] **Step 2: Run endpoint tests and verify RED**

Run:

```bash
go test ./internal/server -run 'TestAdminConfig' -count=1
```

Expected: 404 or build failure because routes and options do not exist.

- [ ] **Step 3: Implement dedicated authentication and handlers**

Keep `/admin/` bootstrap behavior but split middleware:

```go
func (s *srv) requireAdminHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.adminKey == "" { writeErr(w, 503, "admin disabled", "admin_disabled"); return }
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Admin-Key")), []byte(s.adminKey)) != 1 {
			writeErr(w, 401, "invalid admin key", "invalid_admin_key")
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

Register `/admin/config` in a nested group using `requireAdminHeader`. Keep existing endpoint compatibility in this task; migrate other mutations in Task 11.

Map typed store errors to 400/409/422/500. Limit request bodies with `http.MaxBytesReader` using the existing configured maximum.

- [ ] **Step 4: Verify endpoint tests GREEN**

Run:

```bash
gofmt -w internal/server/config_admin.go internal/server/config_admin_test.go internal/server/server.go internal/server/admin.go
go test -race ./internal/server -run 'TestAdminConfig' -count=1
```

Expected: PASS.

- [ ] **Step 5: Review checkpoint**

Search serialized responses in tests for fixture secrets and assert zero matches.

---

### Task 7: Share Runtime Reload Between SIGHUP and Admin PUT

**Files:**
- Create: `cmd/tokenroute/reloader.go`
- Create: `cmd/tokenroute/reloader_test.go`
- Modify: `cmd/tokenroute/main.go:349-545`

**Interfaces:**
- Produces:

```go
type runtimeReloader struct {
	mu           sync.Mutex
	current      *atomic.Pointer[serverState]
	active       *atomic.Pointer[config.Config]
	metrics      *metrics.Registry
	build        func(*config.Config, map[string]usage.Price) (*serverState, error)
	startWorkers func(context.Context, context.CancelFunc, *config.Config, *serverState)
}

func (r *runtimeReloader) Apply(ctx context.Context, persisted *config.Config, restartPaths []string) error
func overlayRestartFields(candidate, active *config.Config) *config.Config
```

`serverState` gains one `workersCancel context.CancelFunc`. Each state owns its provider health checks, balance probes, model-catalog sync, and pricing sync. A reload builds a fresh configured price map instead of mutating the prior state's map; this ensures removing a manual price actually takes effect.

- [ ] **Step 1: Write failing reloader tests**

Test exact behavior:

- route/provider/price edits appear in the newly stored state;
- removing a configured price removes it from the new runtime state;
- `listen`, `admin_listen`, `usage_db`, and `admin_key` remain from active config;
- build failure leaves both atomic pointers unchanged;
- successful swap reuses usage store, key store, limiter, and metrics;
- successful swap starts workers for the new state, then cancels workers from the old state;
- toggling `model_catalog`, `pricing_sync`, health checks, or balance probes changes the new worker set.

Use fake state-builder and worker hooks in the reloader so tests do not start listeners or network probes.

- [ ] **Step 2: Run reloader tests and verify RED**

Run:

```bash
go test ./cmd/tokenroute -run 'TestRuntimeReloader|TestOverlayRestartFields' -count=1
```

Expected: build failure because `runtimeReloader` does not exist.

- [ ] **Step 3: Extract and implement the coordinator**

Move the current SIGHUP build/swap block into `Apply`. Build each replacement state with `buildState(effectiveConfig, nil)` so it receives a fresh map seeded only by the new explicit `prices:` entries. Carry over only process-lifetime services: usage store, key store, limiter, and metrics. Create a child worker context for the candidate; start health checks, balance probes, model-catalog sync, and pricing sync only after state construction succeeds. Swap state and active config atomically under `r.mu`, then cancel the old state's worker context. If worker initialization can fail, do it before the swap and cancel the candidate context on failure.

The worker starter must:

- attach the new model-catalog `Modalities` lookup to the new router when enabled;
- load its persisted catalog cache before exposing the new state, then run refresh in the background;
- start a fresh pricing syncer against the new state's price map when enabled;
- start health and balance loops against the new router;
- start none of the corresponding workers when their config is `off` or disabled.

Initialize once in `main`:

```go
var current atomic.Pointer[serverState]
var activeConfig atomic.Pointer[config.Config]
current.Store(state)
activeConfig.Store(cfg)
reloader := &runtimeReloader{current: &current, active: &activeConfig, metrics: mreg}
configStore := config.NewStore(*configPath, 5)
```

Pass `configStore` and `reloader.Apply` through both public and dedicated admin handlers. Replace the SIGHUP body with `config.Load` followed by `reloader.Apply(ctx, cfg, nil)`: an empty restart-path list means explicit operator reload applies the complete persisted config. The admin path passes the detected restart paths, causing `overlayRestartFields` to retain active bootstrap values.

- [ ] **Step 4: Verify reloader and existing runtime tests GREEN**

Run:

```bash
gofmt -w cmd/tokenroute/reloader.go cmd/tokenroute/reloader_test.go cmd/tokenroute/main.go
go test -race ./cmd/tokenroute ./internal/server -count=1
```

Expected: PASS.

- [ ] **Step 5: Review checkpoint**

Confirm failed admin apply leaves disk restored and `current.Load()` unchanged. Confirm SIGHUP still applies the complete persisted config as before.

---

### Task 8: Split and Embed Dashboard Assets Without Regression

**Files:**
- Create: `internal/server/web/dashboard.html`
- Create: `internal/server/web/dashboard.css`
- Create: `internal/server/web/dashboard.js`
- Modify: `internal/server/dashboard.go`
- Create: `internal/server/dashboard_test.go`
- Modify: `.gitignore`

**Interfaces:**
- Produces embedded paths `/admin/assets/dashboard.css` and `/admin/assets/dashboard.js`.
- Preserves `GET /admin/` and all current element IDs used by the operations UI.

- [ ] **Step 1: Write failing asset and regression tests**

```go
func TestDashboardAssetsEmbedded(t *testing.T) {
	h, _ := adminSetup(t)
	for _, path := range []string{"/admin/", "/admin/assets/dashboard.css", "/admin/assets/dashboard.js"} {
		rec := adminReq(t, h, http.MethodGet, path, "", testAdminKey)
		if rec.Code != http.StatusOK { t.Fatalf("%s: %d", path, rec.Code) }
	}
}

func TestDashboardKeepsOperationsSections(t *testing.T) {
	body := dashboardHTMLForTest(t)
	for _, id := range []string{"s-overview", "s-keys", "s-providers", "s-logs"} {
		if !strings.Contains(body, `id="`+id+`"`) { t.Errorf("missing %s", id) }
	}
}
```

- [ ] **Step 2: Run dashboard tests and verify RED**

Run:

```bash
go test ./internal/server -run 'TestDashboardAssets|TestDashboardKeeps' -count=1
```

Expected: asset routes return 404.

- [ ] **Step 3: Move current markup, styles, and script verbatim first**

Use `//go:embed web/dashboard.html web/dashboard.css web/dashboard.js`. Serve assets with explicit content types and `Cache-Control: no-store`. Preserve all existing IDs and behavior before adding Settings. Add `.superpowers/` to `.gitignore`.

- [ ] **Step 4: Verify zero-regression split GREEN**

Run:

```bash
gofmt -w internal/server/dashboard.go internal/server/dashboard_test.go
go test ./internal/server -run 'TestDashboard' -count=1
go test ./internal/server -count=1
```

Expected: PASS.

- [ ] **Step 5: Review checkpoint**

Open the dashboard and verify Overview, Keys, Providers, and Logs still load and refresh. Check the browser console contains no errors.

---

### Task 9: Build Nested Settings and the Generic Schema Renderer

**Files:**
- Modify: `internal/server/web/dashboard.html`
- Modify: `internal/server/web/dashboard.css`
- Modify: `internal/server/web/dashboard.js`
- Modify: `internal/server/dashboard_test.go`

**Interfaces:**
- Consumes: `GET /admin/config` response and `FieldSchema` recursively.
- Produces JS state:

```js
const configState = {
  base: null,
  draft: null,
  schema: null,
  revision: "",
  validated: null,
  mode: "structured",
  dirtyPaths: new Set(),
};
```

- [ ] **Step 1: Write failing dashboard schema tests**

Assert embedded assets contain:

- top-level `Settings` navigation and `s-settings` section;
- sub-navigation IDs for general, providers, routes, pricing, resilience, search, advanced, and raw YAML;
- renderer functions `renderField`, `renderObject`, `renderList`, and `renderMap`;
- controls for add, duplicate, reorder, delete, search, discard, and validate;
- visible labels and ARIA attributes for modal and tabs.

Also compare every leaf path returned by `config.FormSchema()` against the renderer's supported `kind` values; fail on an unsupported kind.

- [ ] **Step 2: Run dashboard schema tests and verify RED**

Run:

```bash
go test ./internal/server -run 'TestDashboardSettings|TestDashboardSchemaKinds' -count=1
```

Expected: FAIL because Settings markup and renderer are absent.

- [ ] **Step 3: Implement the settings shell and recursive renderer**

Implement these JavaScript entry points exactly:

```js
async function loadSettings() {}
function renderSettingsSection(section) {}
function renderField(schema, value, path) {}
function renderObject(schema, value, path) {}
function renderList(schema, value, path) {}
function renderMap(schema, value, path) {}
function setDraftValue(path, value) {}
function markDirty(path) {}
function filterSettings(query) {}
```

Render common fields immediately and advanced children inside native `<details>`. Render enum fields as `<select>`, booleans as accessible switches, numbers with `type="number"`, maps as key/value rows, objects as groups, and lists as reorderable cards. Use buttons for reorder; do not require pointer dragging. Route strategy and provider type drive conditional visibility from `VisibleWhen` without deleting hidden draft values.

- [ ] **Step 4: Apply the approved visual system**

Use the existing Fira Sans/Fira Code pairing, slate tokens, blue action color, semantic status colors, 44px minimum targets, 150-200ms transitions, visible focus, and `prefers-reduced-motion`. Implement mobile-first breakpoints at 768px and 1024px. At 375px, settings sub-navigation becomes a select or horizontal scroll region while the document itself remains free of horizontal overflow.

- [ ] **Step 5: Verify renderer tests GREEN**

Run:

```bash
go test ./internal/server -run 'TestDashboardSettings|TestDashboardSchemaKinds' -count=1
```

Expected: PASS.

- [ ] **Step 6: Review checkpoint**

Load a full `config.example.yaml` snapshot and verify every leaf schema path creates an enabled editor or an intentional read-only secret/restart control.

---

### Task 10: Implement Validate, Diff, Confirm, Raw YAML, and Conflict UX

**Files:**
- Modify: `internal/server/web/dashboard.html`
- Modify: `internal/server/web/dashboard.css`
- Modify: `internal/server/web/dashboard.js`
- Modify: `internal/server/dashboard_test.go`

**Interfaces:**
- Consumes: `POST /admin/config/validate`, `PUT /admin/config`.
- Produces UI states: clean, dirty, validating, invalid, review, saving, saved, conflict, restored, restart-required.

- [ ] **Step 1: Write failing workflow marker tests**

Assert the assets include exact request methods and guards:

```go
func TestDashboardConfigWorkflowMarkers(t *testing.T) {
	js := dashboardJSForTest(t)
	for _, marker := range []string{
		`api("/admin/config/validate", {method:"POST"`,
		`api("/admin/config", {method:"PUT"`,
		"candidate_revision",
		"expected_revision",
		"restart_required_paths",
		"config-diff-dialog",
	} {
		if !strings.Contains(js, marker) { t.Errorf("missing %q", marker) }
	}
}
```

- [ ] **Step 2: Run workflow tests and verify RED**

Run:

```bash
go test ./internal/server -run 'TestDashboardConfigWorkflow' -count=1
```

Expected: FAIL because workflow functions are absent.

- [ ] **Step 3: Implement the state machine**

Implement:

```js
async function validateConfig() {}
function openDiffDialog(candidate) {}
async function confirmConfigSave() {}
function switchEditorMode(mode) {}
function showFieldErrors(errors) {}
function handleConfigConflict(errorBody) {}
function discardConfigDraft() {}
```

Validation sends the full structured document or raw YAML with `expected_revision`. A successful response replaces both draft views with server-normalized values and stores the complete validated candidate. Save uses only that candidate and disables Confirm after any draft mutation. A 409 keeps the draft and offers `Reload and compare`; it never retries automatically. Field errors focus the first invalid editor. Restart-required success displays persisted and active values.

- [ ] **Step 4: Add the accessible diff dialog**

Use native `<dialog id="config-diff-dialog">`. Group changes by settings section. Represent additions, removals, and edits with text labels plus color. Trap is native; explicitly focus the heading on open and return focus to Validate on close. Escape closes only before a save request starts.

- [ ] **Step 5: Verify workflow tests GREEN**

Run:

```bash
go test ./internal/server -run 'TestDashboardConfigWorkflow|TestDashboardSettings' -count=1
```

Expected: PASS.

- [ ] **Step 6: Review checkpoint**

In a browser, edit one structured field, switch to Raw YAML through validation, introduce invalid YAML, verify line/column feedback, fix it, review diff, save, then trigger a 409 through an external file edit and confirm the draft survives.

---

### Task 11: Complete Existing Key and Provider Operations UI

**Files:**
- Modify: `internal/server/web/dashboard.html`
- Modify: `internal/server/web/dashboard.css`
- Modify: `internal/server/web/dashboard.js`
- Modify: `internal/server/dashboard_test.go`
- Modify: `internal/server/server.go:121-138`
- Modify: `internal/server/admin.go:21-43`

**Interfaces:**
- Consumes all eleven existing `POST /admin/keys` fields.
- Consumes existing provider `disabled` and `balance_low` properties.
- Consumes existing provider enable/disable and usage export endpoints.

- [ ] **Step 1: Write failing coverage tests**

Assert every existing key field appears in dashboard JS payload construction:

```go
var keyFields = []string{
	"name", "rpm", "tpm", "model_rpm", "limit_by_header", "daily_quota",
	"quota_tokens", "budget_usd", "allowed_models", "groups", "expires_at",
}
```

Assert controls call:

```text
/admin/providers/{name}/disable
/admin/providers/{name}/enable
/admin/usage/export?format=csv
```

Assert every non-dashboard mutating admin route uses header-only authentication.

- [ ] **Step 2: Run completion tests and verify RED**

Run:

```bash
go test ./internal/server -run 'TestDashboardKeyFields|TestDashboardProviderActions|TestAdminMutationAuth' -count=1
```

Expected: FAIL for missing fields and controls.

- [ ] **Step 3: Implement complete key creation**

Keep four common fields visible. Put `model_rpm`, `limit_by_header`, `daily_quota`, `budget_usd`, `allowed_models`, `groups`, and `expires_at` under an Advanced disclosure. Parse comma-separated models/groups into trimmed arrays. Use `datetime-local` input converted to RFC3339. Show key rows compactly with expandable details for all limits and scopes.

- [ ] **Step 4: Implement provider actions and CSV export**

Show `disabled` and `balance_low` independently from circuit state. Replace the runtime action area with Test, Reset circuit, Enable/Disable, and Edit configuration. Add Export CSV to Logs and preserve the admin header by fetching the file as a Blob instead of navigating directly.

- [ ] **Step 5: Enforce header-only authentication for mutations**

Keep query authentication only for loading the dashboard document. Route POST/PUT/DELETE operations through `requireAdminHeader`. Existing JavaScript already sends `X-Admin-Key`; update tests that relied on query mutation authentication.

- [ ] **Step 6: Verify completion tests GREEN**

Run:

```bash
gofmt -w internal/server/server.go internal/server/admin.go internal/server/dashboard_test.go
go test ./internal/server -run 'TestDashboardKeyFields|TestDashboardProviderActions|TestAdminMutationAuth|TestAdmin_KeyRoundTrip|TestProviderDisableEnableAdmin' -count=1
```

Expected: PASS.

- [ ] **Step 7: Review checkpoint**

Create a key with all eleven fields through the UI, list it, expand details, disable/enable it, export logs, and verify no raw key is shown after initial creation.

---

### Task 12: Documentation and Full Verification

**Files:**
- Modify: `README.md:419-475`
- Modify: `AGENTS.md:28-73,82-120,146-168`
- Modify: `CLAUDE.md` corresponding sections
- Modify: `config.example.yaml` only if metadata/help uncovers a missing or inaccurate field example

**Interfaces:**
- Documents the final API and operational behavior; no new runtime interface.

- [ ] **Step 1: Update documentation with exact contracts**

Document:

- `GET /admin/config`
- `POST /admin/config/validate`
- `PUT /admin/config`
- `X-Admin-Key` requirement
- revision conflict behavior
- secret sentinel behavior without exposing its value as a user-editable feature
- backup filename/location and five-file retention
- restart-required fields
- validation/diff/save flow
- recovery when apply fails

Update project architecture to include `internal/config/store.go`, `internal/config/schema.go`, `internal/server/config_admin.go`, and embedded web assets.

- [ ] **Step 2: Run format and static checks**

Run:

```bash
gofmt -w internal/config/config.go internal/config/schema.go internal/config/schema_test.go internal/config/store.go internal/config/store_test.go internal/server/server.go internal/server/admin.go internal/server/config_admin.go internal/server/config_admin_test.go internal/server/dashboard.go internal/server/dashboard_test.go cmd/tokenroute/main.go cmd/tokenroute/reloader.go cmd/tokenroute/reloader_test.go
go vet ./...
```

Expected: exit 0.

- [ ] **Step 3: Run race tests, full tests, and build**

Run:

```bash
go test -race ./internal/config ./internal/server ./cmd/tokenroute -count=1
go test ./... -count=1
go build ./...
```

Expected: every command exits 0.

- [ ] **Step 4: Run a real temporary gateway**

Create a temporary config using local fake upstreams from a test harness, a temporary SQLite path, and a non-production admin key. Start the built gateway on unused loopback ports. Exercise with curl:

```bash
curl -fsS -H "X-Admin-Key: $TEST_ADMIN_KEY" "$ADMIN_URL/admin/config"
curl -fsS -X POST -H "X-Admin-Key: $TEST_ADMIN_KEY" -H "Content-Type: application/json" "$ADMIN_URL/admin/config/validate" --data-binary @validate-request.json
curl -fsS -X PUT -H "X-Admin-Key: $TEST_ADMIN_KEY" -H "Content-Type: application/json" "$ADMIN_URL/admin/config" --data-binary @commit-request.json
```

Verify the actual file bytes, backup count, response revisions, runtime route behavior, and restart-required response. Stop all temporary processes and remove temporary artifacts afterward.

- [ ] **Step 5: Browser verification at four widths**

At 375px, 768px, 1024px, and 1440px verify:

- no page-level horizontal overflow;
- all settings sections reachable;
- keyboard-only navigation and visible focus;
- advanced disclosure and conditional fields;
- add/duplicate/reorder/delete controls;
- structured/raw synchronization;
- field errors and focus;
- redacted diff confirmation;
- conflict draft preservation;
- save/reload and restart-required banners;
- Overview, Keys, Providers, and Logs regressions;
- no console errors or failed API calls.

- [ ] **Step 6: Final source and workspace audit**

Run:

```bash
go test ./... -count=1
go vet ./...
go build ./...
```

Confirm no `.env`, real key, session, database, temporary config, browser profile, listener, or test process remains. Because repository policy forbids agent git commands, report changed files and verification output without committing.
