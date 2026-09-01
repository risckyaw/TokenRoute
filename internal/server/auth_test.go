package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/auth"
	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/ratelimit"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// authSetup builds a gateway with auth enabled and one upstream.
// key may be customized before the handler is built.
func authSetup(t *testing.T, mutate func(*auth.Key)) (http.Handler, *auth.Store, auth.Key) {
	t.Helper()
	fp := &fakeProvider{body: `{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`}
	fp.nonStream = true
	rt := router.New([]provider.Provider{fp}, []*router.Route{{
		Model:      "auto",
		Candidates: []router.Candidate{{Provider: fp, Model: "up-model"}},
	}})
	db, err := usage.OpenDB(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ul := usage.NewLogger(db)
	store, err := auth.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	k := auth.Key{Name: "test", Enabled: true}
	if mutate != nil {
		mutate(&k)
	}
	k, err = store.Create(k)
	if err != nil {
		t.Fatal(err)
	}
	h := NewWithOptions(Options{Router: rt, Usage: ul, Keys: store, Limiter: ratelimit.NewRegistry()})
	return h, store, k
}

func postChat(t *testing.T, h http.Handler, key, model string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`"}`))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAuth_MissingKey(t *testing.T) {
	h, _, _ := authSetup(t, nil)
	if rec := postChat(t, h, "", "auto"); rec.Code != 401 {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	h, _, k := authSetup(t, nil)
	rec := postChat(t, h, "gw-deadbeef", "auto")
	if rec.Code != 401 {
		t.Fatalf("unknown key: status %d, want 401", rec.Code)
	}
	// Malformed header (no Bearer prefix).
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto"}`))
	req.Header.Set("Authorization", k.Key)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("malformed header: status %d, want 401", rec.Code)
	}
}

func TestAuth_DisabledKey(t *testing.T) {
	h, _, k := authSetup(t, func(k *auth.Key) { k.Enabled = false })
	if rec := postChat(t, h, k.Key, "auto"); rec.Code != 401 {
		t.Fatalf("status %d, want 401", rec.Code)
	}
}

func TestAuth_ModelNotAllowed(t *testing.T) {
	h, _, k := authSetup(t, func(k *auth.Key) { k.AllowedModels = []string{"other"} })
	rec := postChat(t, h, k.Key, "auto")
	if rec.Code != 403 {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "model_not_allowed") {
		t.Fatalf("body %q", rec.Body.String())
	}
	// Allowed model passes.
	h, _, k = authSetup(t, func(k *auth.Key) { k.AllowedModels = []string{"auto"} })
	if rec := postChat(t, h, k.Key, "auto"); rec.Code != 200 {
		t.Fatalf("allowed model: status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAuth_RPMExceeded(t *testing.T) {
	h, _, k := authSetup(t, func(k *auth.Key) { k.RPM = 1 })
	if rec := postChat(t, h, k.Key, "auto"); rec.Code != 200 {
		t.Fatalf("first request: status %d: %s", rec.Code, rec.Body.String())
	}
	rec := postChat(t, h, k.Key, "auto")
	if rec.Code != 429 {
		t.Fatalf("second request: status %d, want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "rate_limit_exceeded") {
		t.Fatalf("body %q", rec.Body.String())
	}
}

func TestAuth_QuotaExceeded(t *testing.T) {
	// Key already at quota -> 403 before any upstream call.
	h, store, k := authSetup(t, func(k *auth.Key) { k.QuotaTokens = 2 })
	if err := store.SpendTokens(k.ID, 2); err != nil {
		t.Fatal(err)
	}
	if rec := postChat(t, h, k.Key, "auto"); rec.Code != 403 {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	if rec := postChat(t, h, k.Key, "auto"); !strings.Contains(rec.Body.String(), "quota_exceeded") {
		t.Fatalf("want quota_exceeded type: %q", rec.Body.String())
	}
	// Under quota: request passes and spent_tokens accumulates.
	h, store, k = authSetup(t, func(k *auth.Key) { k.QuotaTokens = 3 })
	if rec := postChat(t, h, k.Key, "auto"); rec.Code != 200 {
		t.Fatalf("under quota: status %d", rec.Code)
	}
	got, err := store.GetByKey(k.Key)
	if err != nil {
		t.Fatal(err)
	}
	if got.SpentTokens != 2 {
		t.Fatalf("spent_tokens %d, want 2", got.SpentTokens)
	}
}

func TestAuth_HealthzUnauthenticated(t *testing.T) {
	h, _, _ := authSetup(t, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("healthz: status %d, want 200", rec.Code)
	}
}

func TestAuth_KeyInUsageLog(t *testing.T) {
	fp := &fakeProvider{body: `{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`}
	fp.nonStream = true
	rt := router.New([]provider.Provider{fp}, []*router.Route{{
		Model:      "auto",
		Candidates: []router.Candidate{{Provider: fp, Model: "up-model"}},
	}})
	db, err := usage.OpenDB(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ul := usage.NewLogger(db)
	store, err := auth.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	k, err := store.Create(auth.Key{Name: "alice", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	h := NewWithOptions(Options{Router: rt, Usage: ul, Keys: store, Limiter: ratelimit.NewRegistry()})
	rec := postChat(t, h, k.Key, "auto")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	entries, err := ul.QueryRecent(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].KeyID != k.ID || entries[0].KeyName != "alice" {
		t.Fatalf("entry key fields: %+v", entries[0])
	}
	_, _ = io.Copy(io.Discard, rec.Result().Body)
}
