package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/auth"
	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// TestRouteMultiplier_DoublesCost: multiplier 2.0 doubles the logged cost.
func TestRouteMultiplier_DoublesCost(t *testing.T) {
	fp := &fakeProvider{body: `{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`}
	fp.nonStream = true
	rt := router.New([]provider.Provider{fp}, []*router.Route{{
		Model:      "auto",
		Multiplier: 2.0,
		Candidates: []router.Candidate{{Provider: fp, Model: "up-model"}},
	}})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	prices := map[string]usage.Price{"up-model": {PromptPer1M: 2, CompletionPer1M: 4}}
	h := NewWithOptions(Options{Router: rt, Usage: ul, Prices: prices})

	rec := postChat(t, h, "", "auto")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	entries, err := ul.QueryRecent(1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries %v %v", entries, err)
	}
	e := entries[0]
	base := 10.0/1e6*2 + 5.0/1e6*4
	if e.CostUSD == nil || *e.CostUSD != base*2 {
		t.Fatalf("cost = %v want %v", e.CostUSD, base*2)
	}
	if e.Multiplier != 2.0 {
		t.Fatalf("multiplier = %v want 2", e.Multiplier)
	}
}

// TestAuth_ModelRPM: model_rpm=1 -> first 200, second same-model 429,
// different model allowed (per-model bucket).
func TestAuth_ModelRPM(t *testing.T) {
	h, _, k := authSetup(t, func(k *auth.Key) { k.ModelRPM = 1 })
	if rec := postChat(t, h, k.Key, "auto"); rec.Code != 200 {
		t.Fatalf("first request: status %d: %s", rec.Code, rec.Body.String())
	}
	if rec := postChat(t, h, k.Key, "auto"); rec.Code != 429 {
		t.Fatalf("second same model: status %d, want 429", rec.Code)
	}
	// Different model: separate bucket -> allowed (200 from fake upstream;
	// any non-429 proves the bucket didn't deny).
	if rec := postChat(t, h, k.Key, "other-model"); rec.Code == 429 {
		t.Fatalf("different model must not be rate limited")
	}
}

// TestProviderAutoDisable: 3 circuit trips disable the provider; admin
// enable restores it.
func TestProviderAutoDisable(t *testing.T) {
	bad := upstream(t, 500, `{"error":"boom"}`)
	good := upstream(t, 200, `{"id":"ok","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	// threshold 1: every request opens the circuit; cooldown 0ms -> half-open
	// probe immediately, so each post re-trips. auto_disable_after=3.
	h := setup(t, bad.URL, good.URL, &router.CircuitConfig{FailureThreshold: 1, CooldownMs: 1, AutoDisableAfter: 3})
	for i := 0; i < 3; i++ {
		if rec := post(t, h); rec.Code != 200 {
			t.Fatalf("trip %d: status %d", i+1, rec.Code)
		}
	}
	// After 3 trips p1 is disabled: still 200 via p2, and the breaker reports
	// disabled. Probe directly through a fresh handler state isn't needed —
	// p1 never serves again even after cooldown.
	rec := post(t, h)
	if rec.Code != 200 {
		t.Fatalf("after disable: status %d", rec.Code)
	}
	if dec := rec.Header().Get("X-TokenRoute-Decision"); !strings.Contains(dec, "provider=p2") {
		t.Fatalf("disabled provider still used: %q", dec)
	}
}

// TestProviderDisableEnableAdmin exercises /admin/providers/{name}/disable
// and /enable plus the "disabled" field in GET /admin/providers.
func TestProviderDisableEnableAdmin(t *testing.T) {
	h, rt := adminSetup(t)
	rec := adminReq(t, h, http.MethodPost, "/admin/providers/fake/disable", "", testAdminKey)
	if rec.Code != 200 {
		t.Fatalf("disable: status %d: %s", rec.Code, rec.Body.String())
	}
	if !rt.ProviderDisabled("fake") {
		t.Fatal("provider not disabled after POST disable")
	}
	rec = adminReq(t, h, http.MethodGet, "/admin/providers", "", testAdminKey)
	if !strings.Contains(rec.Body.String(), `"disabled":true`) {
		t.Fatalf("providers list missing disabled flag: %s", rec.Body.String())
	}
	rec = adminReq(t, h, http.MethodPost, "/admin/providers/fake/enable", "", testAdminKey)
	if rec.Code != 200 {
		t.Fatalf("enable: status %d", rec.Code)
	}
	if rt.ProviderDisabled("fake") {
		t.Fatal("provider still disabled after POST enable")
	}
	if st := rt.CircuitState("fake"); st != "closed" {
		t.Fatalf("circuit = %q want closed", st)
	}
}

// TestMaxBody_Oversized: small MaxBodyMB override rejects large bodies.
func TestMaxBody_Oversized(t *testing.T) {
	fp := &fakeProvider{body: `{}`}
	fp.nonStream = true
	rt := router.New([]provider.Provider{fp}, []*router.Route{{
		Model:      "auto",
		Candidates: []router.Candidate{{Provider: fp, Model: "up-model"}},
	}})
	h := NewWithOptions(Options{Router: rt, MaxBodyMB: 1})
	big := `{"model":"auto","pad":"` + strings.Repeat("x", 2<<20) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(big))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 400/413", rec.Code)
	}
}

// TestAdmin_CreateKeyModelRPM: model_rpm accepted on create, returned on list.
func TestAdmin_CreateKeyModelRPM(t *testing.T) {
	h, _ := adminSetup(t)
	rec := adminReq(t, h, http.MethodPost, "/admin/keys",
		`{"name":"m","model_rpm":5}`, testAdminKey)
	if rec.Code != 201 {
		t.Fatalf("create: status %d: %s", rec.Code, rec.Body.String())
	}
	var created auth.Key
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ModelRPM != 5 {
		t.Fatalf("model_rpm = %d want 5", created.ModelRPM)
	}
	rec = adminReq(t, h, http.MethodGet, "/admin/keys", "", testAdminKey)
	if !strings.Contains(rec.Body.String(), `"model_rpm":5`) {
		t.Fatalf("list missing model_rpm: %s", rec.Body.String())
	}
}
