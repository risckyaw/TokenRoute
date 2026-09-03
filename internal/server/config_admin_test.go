package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/config"
)

const testConfigYAML = `listen: :8400
admin_key: ${GATEWAY_ADMIN_KEY}
providers:
  - name: p1
    type: openai
    base_url: http://localhost:9
    api_key: sk-literal-secret
    priority: 1
routes:
  - model: auto
    candidates:
      - provider: p1
        model: up-model
`

func editableTestConfigYAML() string {
	lines := strings.Split(testConfigYAML, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "api_key:") {
			indent := line[:len(line)-len(strings.TrimLeft(line, " 	"))]
			lines[i] = indent + "api_key: " + config.SecretKeep
			break
		}
	}
	return strings.Join(lines, "\n")
}

type configApplyRecorder struct {
	calls int
	paths [][]string
	err   error
}

type configValidateRecorder struct {
	calls int
	cfg   *config.Config
	paths []string
	err   error
}

func (r *configApplyRecorder) apply(_ context.Context, _ *config.Config, paths []string) error {
	r.calls++
	r.paths = append(r.paths, paths)
	return r.err
}

func (r *configValidateRecorder) validate(_ context.Context, cfg *config.Config, paths []string) error {
	r.calls++
	r.cfg = cfg
	r.paths = append([]string(nil), paths...)
	return r.err
}

// configAdminSetup builds a handler with a real config.Store over a temp file.
func configAdminSetup(t *testing.T) (http.Handler, string, *configApplyRecorder) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(testConfigYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &configApplyRecorder{}
	h := NewWithOptions(Options{
		AdminKey:    testAdminKey,
		ConfigStore: config.NewStore(path, 5),
		ApplyConfig: rec.apply,
	})
	return h, path, rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("response not JSON: %v\nbody: %s", err, rec.Body.String())
	}
	return v
}

func getSnapshot(t *testing.T, h http.Handler) config.Snapshot {
	t.Helper()
	rec := adminReq(t, h, http.MethodGet, "/admin/config", "", testAdminKey)
	if rec.Code != 200 {
		t.Fatalf("GET snapshot: status = %d, body %s", rec.Code, rec.Body)
	}
	return decodeBody[config.Snapshot](t, rec)
}

func validateRaw(t *testing.T, h http.Handler, expectedRev, raw string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"expected_revision": expectedRev,
		"mode":              "raw",
		"raw_yaml":          raw,
	})
	return adminReq(t, h, http.MethodPost, "/admin/config/validate", string(body), testAdminKey)
}

func TestAdminConfigRejectsQueryAuthentication(t *testing.T) {
	h, _, _ := configAdminSetup(t)
	rec := adminReq(t, h, http.MethodGet, "/admin/config?key="+testAdminKey, "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET query auth: status = %d, want 401", rec.Code)
	}
	rec = adminReq(t, h, http.MethodPost, "/admin/config/validate?key="+testAdminKey, `{}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST query auth: status = %d, want 401", rec.Code)
	}
	rec = adminReq(t, h, http.MethodPut, "/admin/config?key="+testAdminKey, `{}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("PUT query auth: status = %d, want 401", rec.Code)
	}
}

func TestAdminConfigRequiresHeader(t *testing.T) {
	h, _, _ := configAdminSetup(t)
	if rec := adminReq(t, h, http.MethodGet, "/admin/config", "", ""); rec.Code != 401 {
		t.Fatalf("no key: status = %d", rec.Code)
	}
	if rec := adminReq(t, h, http.MethodGet, "/admin/config", "", "wrong"); rec.Code != 401 {
		t.Fatalf("bad key: status = %d", rec.Code)
	}
}

func TestAdminConfigDisabledWithoutAdminKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(testConfigYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewWithOptions(Options{ConfigStore: config.NewStore(path, 5)})
	if rec := adminReq(t, h, http.MethodGet, "/admin/config", "", "anything"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestAdminConfigNilStore503(t *testing.T) {
	h := NewWithOptions(Options{AdminKey: testAdminKey})
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut} {
		path := "/admin/config"
		if m == http.MethodPost {
			path = "/admin/config/validate"
		}
		if rec := adminReq(t, h, m, path, `{}`, testAdminKey); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: status = %d, want 503", m, path, rec.Code)
		}
	}
}

func TestAdminConfigGetRedactsSecrets(t *testing.T) {
	h, _, _ := configAdminSetup(t)
	rec := adminReq(t, h, http.MethodGet, "/admin/config", "", testAdminKey)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	snap := decodeBody[config.Snapshot](t, rec)
	if !strings.HasPrefix(snap.Revision, "sha256:") {
		t.Fatalf("revision = %q", snap.Revision)
	}
	if snap.Schema == nil || len(snap.RestartRequiredPaths) == 0 {
		t.Fatal("schema or restart_required_paths missing")
	}
	if snap.RawYAML == "" || snap.Document == nil {
		t.Fatal("raw_yaml/document missing")
	}
	body := rec.Body.String()
	if strings.Contains(body, "sk-literal-secret") {
		t.Fatal("literal secret leaked in response")
	}
	if !strings.Contains(body, config.SecretKeep) {
		t.Fatal("redaction sentinel missing")
	}
	if !strings.Contains(body, "${GATEWAY_ADMIN_KEY}") {
		t.Fatal("env reference must survive verbatim")
	}
}

func TestAdminConfigValidateSuccessNoSideEffects(t *testing.T) {
	h, path, rec := configAdminSetup(t)
	before, _ := os.ReadFile(path)
	snap := getSnapshot(t, h)

	vr := validateRaw(t, h, snap.Revision, editableTestConfigYAML())
	if vr.Code != 200 {
		t.Fatalf("status = %d, body %s", vr.Code, vr.Body)
	}
	var out struct {
		Valid             bool            `json:"valid"`
		BaseRevision      string          `json:"base_revision"`
		CandidateRevision string          `json:"candidate_revision"`
		Diff              []config.Change `json:"diff"`
	}
	out = decodeBody[struct {
		Valid             bool            `json:"valid"`
		BaseRevision      string          `json:"base_revision"`
		CandidateRevision string          `json:"candidate_revision"`
		Diff              []config.Change `json:"diff"`
	}](t, vr)
	if !out.Valid || out.BaseRevision != snap.Revision || out.CandidateRevision == "" {
		t.Fatalf("unexpected validate response: %+v", out)
	}
	if rec.calls != 0 {
		t.Fatalf("validate applied config %d times", rec.calls)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("validate mutated the config file")
	}
	if strings.Contains(vr.Body.String(), "sk-literal-secret") {
		t.Fatal("literal secret leaked in validate response")
	}
}

func TestAdminConfigValidateRejectsTrailingYAMLDocument(t *testing.T) {
	h, _, _ := configAdminSetup(t)
	snap := getSnapshot(t, h)

	vr := validateRaw(t, h, snap.Revision, editableTestConfigYAML()+"---\n{}\n")
	if vr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body %s", vr.Code, vr.Body)
	}
	body := decodeFieldErrs(t, vr)
	if len(body.Errors) != 1 || body.Errors[0].Code != config.CodeYAMLSyntax || body.Errors[0].Path != "" {
		t.Fatalf("errors = %+v", body)
	}
}

func TestAdminConfigValidateRuntimeBuildFailureIsSideEffectFree(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(testConfigYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	apply := &configApplyRecorder{}
	runtimeCheck := &configValidateRecorder{err: errors.New("provider build exposed-secret-value")}
	h := NewWithOptions(Options{
		AdminKey:       testAdminKey,
		ConfigStore:    config.NewStore(path, 5),
		ApplyConfig:    apply.apply,
		ValidateConfig: runtimeCheck.validate,
	})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	snap := getSnapshot(t, h)
	updated := strings.Replace(editableTestConfigYAML(), "priority: 1", "priority: 2", 1)

	vr := validateRaw(t, h, snap.Revision, updated)
	if vr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body %s", vr.Code, vr.Body)
	}
	body := decodeFieldErrs(t, vr)
	if len(body.Errors) != 1 || body.Errors[0].Code != config.CodeRuntimeInvalid || body.Errors[0].Path != "" {
		t.Fatalf("errors = %+v", body)
	}
	if strings.Contains(vr.Body.String(), "exposed-secret-value") {
		t.Fatalf("runtime error leaked secret: %s", vr.Body)
	}
	if runtimeCheck.calls != 1 || runtimeCheck.cfg == nil {
		t.Fatalf("runtime validation calls = %d, cfg = %p", runtimeCheck.calls, runtimeCheck.cfg)
	}
	if apply.calls != 0 {
		t.Fatalf("validate applied config %d times", apply.calls)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("runtime validation wrote config bytes")
	}
}

func TestAdminConfigValidatePassesRestartRequiredPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(testConfigYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	runtimeCheck := &configValidateRecorder{}
	h := NewAdminOnly(Options{
		AdminKey:       testAdminKey,
		ConfigStore:    config.NewStore(path, 5),
		ValidateConfig: runtimeCheck.validate,
	})
	snap := getSnapshot(t, h)
	updated := strings.Replace(editableTestConfigYAML(), "listen: :8400", "listen: :9400", 1)

	vr := validateRaw(t, h, snap.Revision, updated)
	if vr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", vr.Code, vr.Body)
	}
	if runtimeCheck.calls != 1 || len(runtimeCheck.paths) != 1 || runtimeCheck.paths[0] != "listen" {
		t.Fatalf("runtime validation paths = %v, calls = %d", runtimeCheck.paths, runtimeCheck.calls)
	}
}

func TestAdminConfigValidateConflict409(t *testing.T) {
	h, _, _ := configAdminSetup(t)
	vr := validateRaw(t, h, "sha256:stale", testConfigYAML)
	if vr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body %s", vr.Code, vr.Body)
	}
}

func TestAdminConfigValidateFieldErrors422(t *testing.T) {
	h, _, _ := configAdminSetup(t)
	snap := getSnapshot(t, h)

	// YAML syntax error.
	vr := validateRaw(t, h, snap.Revision, "listen: [unclosed\n")
	if vr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("syntax: status = %d, want 422, body %s", vr.Code, vr.Body)
	}
	var body struct {
		Valid  bool `json:"valid"`
		Errors []struct {
			Path    string `json:"path"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	body = decodeBody[struct {
		Valid  bool `json:"valid"`
		Errors []struct {
			Path    string `json:"path"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}](t, vr)
	if body.Valid || len(body.Errors) == 0 || body.Errors[0].Message == "" {
		t.Fatalf("expected field errors, got %+v", body)
	}

	// Semantic error: unknown provider in route candidate.
	bad := strings.Replace(editableTestConfigYAML(), "provider: p1\n        model: up-model", "provider: missing\n        model: up-model", 1)
	vr = validateRaw(t, h, snap.Revision, bad)
	if vr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("semantic: status = %d, want 422, body %s", vr.Code, vr.Body)
	}

	// Malformed request body -> 400.
	rec := adminReq(t, h, http.MethodPost, "/admin/config/validate", `{not json`, testAdminKey)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: status = %d, want 400", rec.Code)
	}
	// Unknown mode -> 400.
	rec = adminReq(t, h, http.MethodPost, "/admin/config/validate",
		`{"expected_revision":"`+snap.Revision+`","mode":"weird"}`, testAdminKey)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad mode: status = %d, want 400, body %s", rec.Code, rec.Body)
	}
}

func TestAdminConfigPutSuccess(t *testing.T) {
	h, path, rec := configAdminSetup(t)
	snap := getSnapshot(t, h)
	updated := strings.Replace(editableTestConfigYAML(), "priority: 1", "priority: 2", 1)

	vr := validateRaw(t, h, snap.Revision, updated)
	if vr.Code != 200 {
		t.Fatalf("validate: %d %s", vr.Code, vr.Body)
	}
	cand := decodeBody[struct {
		CandidateRevision string `json:"candidate_revision"`
	}](t, vr)

	putBody, _ := json.Marshal(map[string]any{
		"expected_revision":  snap.Revision,
		"candidate_revision": cand.CandidateRevision,
		"mode":               "raw",
		"raw_yaml":           updated,
	})
	pr := adminReq(t, h, http.MethodPut, "/admin/config", string(putBody), testAdminKey)
	if pr.Code != 200 {
		t.Fatalf("PUT: status = %d, body %s", pr.Code, pr.Body)
	}
	res := decodeBody[config.CommitResult](t, pr)
	if !res.Saved || !res.Applied || res.RestartRequired || res.Restored {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Revision != cand.CandidateRevision {
		t.Fatalf("revision = %q, want %q", res.Revision, cand.CandidateRevision)
	}
	if rec.calls != 1 {
		t.Fatalf("apply calls = %d, want 1", rec.calls)
	}
	after, _ := os.ReadFile(path)
	if !strings.Contains(string(after), "priority: 2") {
		t.Fatal("file not updated")
	}
	if strings.Contains(pr.Body.String(), "sk-literal-secret") {
		t.Fatal("literal secret leaked in PUT response")
	}
	// New snapshot has the new revision.
	if got := getSnapshot(t, h).Revision; got != res.Revision {
		t.Fatalf("post-PUT revision = %q, want %q", got, res.Revision)
	}
}

func TestAdminConfigPutConflict409(t *testing.T) {
	h, _, _ := configAdminSetup(t)
	putBody, _ := json.Marshal(map[string]any{
		"expected_revision":  "sha256:stale",
		"candidate_revision": "sha256:whatever",
		"mode":               "raw",
		"raw_yaml":           editableTestConfigYAML(),
	})
	pr := adminReq(t, h, http.MethodPut, "/admin/config", string(putBody), testAdminKey)
	if pr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body %s", pr.Code, pr.Body)
	}
}

func TestAdminConfigPutCandidateMismatch(t *testing.T) {
	h, _, rec := configAdminSetup(t)
	snap := getSnapshot(t, h)
	putBody, _ := json.Marshal(map[string]any{
		"expected_revision":  snap.Revision,
		"candidate_revision": "sha256:not-the-candidate",
		"mode":               "raw",
		"raw_yaml":           editableTestConfigYAML(),
	})
	pr := adminReq(t, h, http.MethodPut, "/admin/config", string(putBody), testAdminKey)
	if pr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body %s", pr.Code, pr.Body)
	}
	if rec.calls != 0 {
		t.Fatal("apply must not run on candidate mismatch")
	}
}

func TestAdminConfigPutRollbackOnApplyFailure(t *testing.T) {
	h, path, rec := configAdminSetup(t)
	snap := getSnapshot(t, h)
	updated := strings.Replace(editableTestConfigYAML(), "priority: 1", "priority: 2", 1)
	vr := validateRaw(t, h, snap.Revision, updated)
	cand := decodeBody[struct {
		CandidateRevision string `json:"candidate_revision"`
	}](t, vr)

	rec.err = errors.New("boom: provider construction failed")
	putBody, _ := json.Marshal(map[string]any{
		"expected_revision":  snap.Revision,
		"candidate_revision": cand.CandidateRevision,
		"mode":               "raw",
		"raw_yaml":           updated,
	})
	pr := adminReq(t, h, http.MethodPut, "/admin/config", string(putBody), testAdminKey)
	if pr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body %s", pr.Code, pr.Body)
	}
	res := decodeBody[config.CommitResult](t, pr)
	if !res.Saved || res.Applied || !res.Restored {
		t.Fatalf("rollback fields wrong: %+v", res)
	}
	after, _ := os.ReadFile(path)
	if string(after) != testConfigYAML {
		t.Fatal("file not restored to exact prior bytes")
	}
	if strings.Contains(pr.Body.String(), "sk-literal-secret") {
		t.Fatal("literal secret leaked in error response")
	}
}

type fieldErrBody struct {
	Valid  bool `json:"valid"`
	Errors []struct {
		Path    string `json:"path"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

func decodeFieldErrs(t *testing.T, rec *httptest.ResponseRecorder) fieldErrBody {
	t.Helper()
	return decodeBody[fieldErrBody](t, rec)
}

func TestAdminConfigValidateTypedFieldErrors(t *testing.T) {
	h, _, _ := configAdminSetup(t)
	snap := getSnapshot(t, h)

	// Empty raw config: 422 typed, not 500.
	vr := validateRaw(t, h, snap.Revision, "  \n")
	if vr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty raw: status = %d, want 422, body %s", vr.Code, vr.Body)
	}
	body := decodeFieldErrs(t, vr)
	if body.Valid || len(body.Errors) != 1 || body.Errors[0].Code != config.CodeEmptyRaw {
		t.Fatalf("empty raw errors = %+v", body)
	}

	// Structured mode without document: 422 typed.
	sbody, _ := json.Marshal(map[string]any{
		"expected_revision": snap.Revision,
		"mode":              "structured",
	})
	vr = adminReq(t, h, http.MethodPost, "/admin/config/validate", string(sbody), testAdminKey)
	if vr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing document: status = %d, want 422, body %s", vr.Code, vr.Body)
	}
	body = decodeFieldErrs(t, vr)
	if len(body.Errors) != 1 || body.Errors[0].Code != config.CodeDocumentRequired {
		t.Fatalf("missing document errors = %+v", body)
	}

	// Sentinel misuse: 422 with stable path.
	bad := strings.Replace(editableTestConfigYAML(), "priority: 1", "priority: "+config.SecretKeep, 1)
	vr = validateRaw(t, h, snap.Revision, bad)
	if vr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("sentinel: status = %d, want 422, body %s", vr.Code, vr.Body)
	}
	body = decodeFieldErrs(t, vr)
	if len(body.Errors) != 1 || body.Errors[0].Code != config.CodeSentinelMisuse ||
		body.Errors[0].Path != "providers[].priority" {
		t.Fatalf("sentinel errors = %+v", body)
	}
}

func TestAdminConfigRejectsLiteralSecretWithoutLeak(t *testing.T) {
	h, _, rec := configAdminSetup(t)
	snap := getSnapshot(t, h)
	literal := "brand-new-api-secret"
	raw := strings.Replace(snap.RawYAML, config.SecretKeep, literal, 1)
	vr := validateRaw(t, h, snap.Revision, raw)
	if vr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body %s", vr.Code, vr.Body)
	}
	body := decodeFieldErrs(t, vr)
	if len(body.Errors) != 1 || body.Errors[0].Code != config.CodeLiteralSecretForbidden || body.Errors[0].Path != "providers[p1].api_key" {
		t.Fatalf("errors = %+v", body)
	}
	if strings.Contains(vr.Body.String(), literal) {
		t.Fatalf("literal secret leaked in response: %s", vr.Body)
	}
	if rec.calls != 0 {
		t.Fatal("validation failure applied config")
	}
}

func TestAdminConfigRejectsAliasedSentinelWithoutLeak(t *testing.T) {
	h, _, rec := configAdminSetup(t)
	snap := getSnapshot(t, h)
	raw := strings.Replace(snap.RawYAML, "api_key: "+config.SecretKeep, "api_key: &keep "+config.SecretKeep, 1)
	raw = strings.Replace(raw, "routes:", `  - name: p2
    type: openai
    base_url: http://localhost:10
    api_key: *keep
    priority: 2
routes:`, 1)

	vr := validateRaw(t, h, snap.Revision, raw)
	if vr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body %s", vr.Code, vr.Body)
	}
	body := decodeFieldErrs(t, vr)
	if len(body.Errors) != 1 || body.Errors[0].Code != config.CodeSentinelMisuse ||
		body.Errors[0].Path != "providers[p2].api_key" {
		t.Fatalf("errors = %+v", body)
	}
	if strings.Contains(vr.Body.String(), "«redacted:sk-…»") {
		t.Fatalf("literal secret leaked in validation response: %s", vr.Body)
	}
	if rec.calls != 0 {
		t.Fatal("validation failure applied config")
	}
}

func TestAdminConfigRejectsAliasToLiteralSecretWithoutLeak(t *testing.T) {
	h, _, rec := configAdminSetup(t)
	snap := getSnapshot(t, h)
	const literal = "api-alias-secret"
	raw := strings.Replace(snap.RawYAML, "providers:", "aliases:\n  injected: &injected "+literal+"\nproviders:", 1)
	raw = strings.Replace(raw, "api_key: "+config.SecretKeep, "api_key: *injected", 1)

	vr := validateRaw(t, h, snap.Revision, raw)
	if vr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body %s", vr.Code, vr.Body)
	}
	body := decodeFieldErrs(t, vr)
	if len(body.Errors) != 1 || body.Errors[0].Code != config.CodeLiteralSecretForbidden ||
		body.Errors[0].Path != "providers[p1].api_key" {
		t.Fatalf("errors = %+v", body)
	}
	if strings.Contains(vr.Body.String(), literal) {
		t.Fatalf("literal secret leaked in validation response: %s", vr.Body)
	}
	if rec.calls != 0 {
		t.Fatal("validation failure applied config")
	}
}

func TestAdminConfigStructuredDuplicateIdentity422(t *testing.T) {
	h, _, _ := configAdminSetup(t)
	snap := getSnapshot(t, h)
	doc := map[string]any{
		"listen":    ":8400",
		"admin_key": "${GATEWAY_ADMIN_KEY}",
		"providers": []any{
			map[string]any{"name": "p1", "type": "openai", "base_url": "http://localhost:9", "api_key": config.SecretKeep, "priority": 1},
			map[string]any{"name": "p1", "type": "openai", "base_url": "http://other", "api_key": config.SecretKeep, "priority": 2},
		},
		"routes": []any{
			map[string]any{"model": "auto", "candidates": []any{map[string]any{"provider": "p1", "model": "up-model"}}},
		},
	}
	sbody, _ := json.Marshal(map[string]any{
		"expected_revision": snap.Revision,
		"mode":              "structured",
		"document":          doc,
	})
	vr := adminReq(t, h, http.MethodPost, "/admin/config/validate", string(sbody), testAdminKey)
	if vr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body %s", vr.Code, vr.Body)
	}
	body := decodeFieldErrs(t, vr)
	if len(body.Errors) != 1 || body.Errors[0].Code != config.CodeMergeConflict ||
		body.Errors[0].Path != "providers" {
		t.Fatalf("errors = %+v", body)
	}
}

func TestAdminConfigRejectsOversizedBody(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(testConfigYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewWithOptions(Options{
		AdminKey:    testAdminKey,
		ConfigStore: config.NewStore(path, 5),
		ApplyConfig: (&configApplyRecorder{}).apply,
		MaxBodyMB:   1,
	})
	big := `{"expected_revision":"x","mode":"raw","raw_yaml":"` + strings.Repeat("a", 2<<20) + `"}`
	rec := adminReq(t, h, http.MethodPost, "/admin/config/validate", big, testAdminKey)
	if rec.Code == 200 {
		t.Fatal("oversized body accepted")
	}
}
