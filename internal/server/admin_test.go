package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/auth"
	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/ratelimit"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

const testAdminKey = "test-admin-secret"

func adminSetup(t *testing.T) (http.Handler, *router.Router) {
	t.Helper()
	fp := &fakeProvider{body: `{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`}
	fp.nonStream = true
	rt := router.New([]provider.Provider{fp}, []*router.Route{{
		Model:      "auto",
		Candidates: []router.Candidate{{Provider: fp, Model: "up-model"}},
	}})
	rt.SetCircuit("fake", router.CircuitConfig{FailureThreshold: 1, CooldownMs: 60000})
	db, err := usage.OpenDB(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store, err := auth.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	h := NewWithOptions(Options{
		Router: rt, Usage: usage.NewLogger(db), Keys: store,
		Limiter: ratelimit.NewRegistry(), AdminKey: testAdminKey,
	})
	return h, rt
}

func adminReq(t *testing.T, h http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if key != "" {
		req.Header.Set("X-Admin-Key", key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAdmin_WrongKey(t *testing.T) {
	h, _ := adminSetup(t)
	if rec := adminReq(t, h, http.MethodGet, "/admin/keys", "", "wrong"); rec.Code != 401 {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	if rec := adminReq(t, h, http.MethodGet, "/admin/keys", "", ""); rec.Code != 401 {
		t.Fatalf("no key: status %d, want 401", rec.Code)
	}
}

func TestAdmin_Disabled(t *testing.T) {
	fp := &fakeProvider{body: "x"}
	rt := router.New([]provider.Provider{fp}, nil)
	h := NewWithOptions(Options{Router: rt}) // no admin key, no store
	rec := adminReq(t, h, http.MethodGet, "/admin/keys", "", "anything")
	if rec.Code != 503 {
		t.Fatalf("status %d, want 503", rec.Code)
	}
}

func TestAdmin_KeyRoundTrip(t *testing.T) {
	h, _ := adminSetup(t)
	// Create.
	rec := adminReq(t, h, http.MethodPost, "/admin/keys",
		`{"name":"ci","rpm":60,"allowed_models":["auto"]}`, testAdminKey)
	if rec.Code != 201 {
		t.Fatalf("create: status %d: %s", rec.Code, rec.Body.String())
	}
	var created auth.Key
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Key, "gw-ci-") || len(created.Key) != len("gw-ci-")+24 {
		t.Fatalf("key format %q", created.Key)
	}
	if !created.Enabled || created.RPM != 60 {
		t.Fatalf("created: %+v", created)
	}
	// List masks the key.
	rec = adminReq(t, h, http.MethodGet, "/admin/keys", "", testAdminKey)
	if rec.Code != 200 {
		t.Fatalf("list: status %d", rec.Code)
	}
	var list struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Keys) != 1 {
		t.Fatalf("want 1 key, got %d", len(list.Keys))
	}
	maskedKey, _ := list.Keys[0]["key"].(string)
	if !strings.HasSuffix(maskedKey, "...") || strings.Contains(maskedKey, created.Key[7:]) {
		t.Fatalf("key not masked: %q", maskedKey)
	}
	// Disable -> key no longer authenticates.
	id := created.ID
	rec = adminReq(t, h, http.MethodPost, "/admin/keys/"+itoa(id)+"/disable", "", testAdminKey)
	if rec.Code != 200 {
		t.Fatalf("disable: status %d", rec.Code)
	}
	if rec := postChat(t, h, created.Key, "auto"); rec.Code != 401 {
		t.Fatalf("disabled key: status %d, want 401", rec.Code)
	}
	// Enable again -> works.
	rec = adminReq(t, h, http.MethodPost, "/admin/keys/"+itoa(id)+"/enable", "", testAdminKey)
	if rec.Code != 200 {
		t.Fatalf("enable: status %d", rec.Code)
	}
	if rec := postChat(t, h, created.Key, "auto"); rec.Code != 200 {
		t.Fatalf("re-enabled key: status %d: %s", rec.Code, rec.Body.String())
	}
	// Delete -> gone.
	rec = adminReq(t, h, http.MethodDelete, "/admin/keys/"+itoa(id), "", testAdminKey)
	if rec.Code != 204 {
		t.Fatalf("delete: status %d, want 204", rec.Code)
	}
	rec = adminReq(t, h, http.MethodGet, "/admin/keys", "", testAdminKey)
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Keys) != 0 {
		t.Fatalf("want 0 keys after delete, got %d", len(list.Keys))
	}
}

func TestAdmin_ProvidersAndCircuitReset(t *testing.T) {
	h, rt := adminSetup(t)
	rec := adminReq(t, h, http.MethodGet, "/admin/providers", "", testAdminKey)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var body struct {
		Providers []struct {
			Name    string `json:"name"`
			Circuit string `json:"circuit"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Providers) != 1 || body.Providers[0].Name != "fake" || body.Providers[0].Circuit != "closed" {
		t.Fatalf("providers: %+v", body)
	}
	// Open the breaker, then reset it via the admin API.
	rt.RecordResult("fake", 0, false) // threshold 1 -> open
	if got := rt.CircuitState("fake"); got != "open" {
		t.Fatalf("circuit %q, want open", got)
	}
	rec = adminReq(t, h, http.MethodPost, "/admin/providers/fake/circuit/reset", "", testAdminKey)
	if rec.Code != 200 {
		t.Fatalf("reset: status %d", rec.Code)
	}
	if got := rt.CircuitState("fake"); got != "closed" {
		t.Fatalf("after reset: circuit %q, want closed", got)
	}
}

func TestAdmin_UsageAggregate(t *testing.T) {
	h, _ := adminSetup(t)
	// Create key, make a request so usage exists.
	rec := adminReq(t, h, http.MethodPost, "/admin/keys", `{"name":"agg"}`, testAdminKey)
	var created auth.Key
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if rec := postChat(t, h, created.Key, "auto"); rec.Code != 200 {
		t.Fatalf("chat: status %d", rec.Code)
	}
	rec = adminReq(t, h, http.MethodGet, "/admin/usage", "", testAdminKey)
	if rec.Code != 200 {
		t.Fatalf("usage: status %d", rec.Code)
	}
	var agg struct {
		Keys []struct {
			KeyID    int64  `json:"key_id"`
			KeyName  string `json:"key_name"`
			Requests int    `json:"requests"`
		} `json:"keys"`
		Totals struct {
			Requests int `json:"requests"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &agg); err != nil {
		t.Fatal(err)
	}
	if len(agg.Keys) != 1 || agg.Keys[0].KeyName != "agg" || agg.Keys[0].Requests != 1 {
		t.Fatalf("agg: %+v", agg)
	}
	if agg.Totals.Requests != 1 {
		t.Fatalf("totals: %+v", agg.Totals)
	}
}

func TestAdmin_UsageLogs(t *testing.T) {
	h, _ := adminSetup(t)
	// Seed 3 entries via real chat requests (logged by the handler's logger).
	for i, model := range []string{"m1", "m2", "m3"} {
		rec := adminReq(t, h, http.MethodPost, "/admin/keys", `{"name":"k`+model+`"}`, testAdminKey)
		var created auth.Key
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		if rec := postChat(t, h, created.Key, "auto"); rec.Code != 200 {
			t.Fatalf("chat %d: status %d", i, rec.Code)
		}
	}
	rec := adminReq(t, h, http.MethodGet, "/admin/usage/logs?limit=2", "", testAdminKey)
	if rec.Code != 200 {
		t.Fatalf("logs: status %d: %s", rec.Code, rec.Body.String())
	}
	var entries []usage.Entry
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries (limit), got %d", len(entries))
	}
	// Newest first: IDs descending.
	if entries[0].ID <= entries[1].ID {
		t.Fatalf("order not newest-first: ids %d, %d", entries[0].ID, entries[1].ID)
	}
	e := entries[0]
	if e.RequestID == "" || e.VirtualModel != "auto" || e.Provider != "fake" ||
		e.Model != "up-model" || e.TotalTokens != 2 || e.Status != 200 {
		t.Fatalf("entry fields: %+v", e)
	}
	// Default limit returns all 3, still newest first.
	rec = adminReq(t, h, http.MethodGet, "/admin/usage/logs", "", testAdminKey)
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[0].ID < entries[2].ID {
		t.Fatalf("default limit/order: %+v", entries)
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
