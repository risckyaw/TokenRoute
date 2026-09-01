package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/provider/openai"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// setupFallback builds a gateway with routes a -> b -> c (fallback_routes),
// one candidate per route.
func setupFallback(t *testing.T, urls map[string]string, fallbacks map[string][]string) http.Handler {
	t.Helper()
	provs := []provider.Provider{}
	byName := map[string]provider.Provider{}
	i := 1
	for _, name := range []string{"pa", "pb", "pc"} {
		p := openai.New(openai.Config{Name: name, BaseURL: urls[name], Priority: i, TimeoutMs: 5000})
		i++
		provs = append(provs, p)
		byName[name] = p
	}
	candProv := map[string]string{"a": "pa", "b": "pb", "c": "pc"}
	routes := []*router.Route{}
	for _, m := range []string{"a", "b", "c"} {
		routes = append(routes, &router.Route{
			Model:          m,
			FallbackRoutes: fallbacks[m],
			Candidates:     []router.Candidate{{Provider: byName[candProv[m]], Model: m + "-model"}},
		})
	}
	rt := router.New(provs, routes)
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	return New(rt, ul, nil)
}

func postModel(t *testing.T, h http.Handler, model string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[]}`)))
	return rec
}

func TestFallbackRoute_SuccessOnSecondRoute(t *testing.T) {
	bad := upstream(t, 500, `{"error":"boom"}`)
	good := upstream(t, 200, `{"id":"ok","usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	dead := upstream(t, 500, `{"error":"boom"}`)
	h := setupFallback(t,
		map[string]string{"pa": bad.URL, "pb": good.URL, "pc": dead.URL},
		map[string][]string{"a": {"b"}})

	rec := postModel(t, h, "a")
	if rec.Code != 200 {
		t.Fatalf("status %d, want 200 via fallback: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.ID != "ok" {
		t.Fatalf("body %q not from fallback route", rec.Body.String())
	}
	if d := rec.Header().Get("X-TokenRoute-Decision"); !strings.Contains(d, "provider=pb") {
		t.Fatalf("decision %q, want provider=pb", d)
	}
}

func TestFallbackRoute_CycleProtection(t *testing.T) {
	s := upstream(t, 500, `{"error":"boom"}`)
	h := setupFallback(t,
		map[string]string{"pa": s.URL, "pb": s.URL, "pc": s.URL},
		map[string][]string{"a": {"b", "a"}, "b": {"a", "b"}})

	rec := postModel(t, h, "a")
	if rec.Code != 500 {
		t.Fatalf("status %d, want 500 relay after cycle-bounded exhaustion", rec.Code)
	}
	// 3 hops max: routes a, b, then stop (a/b both visited) — not an infinite loop.
}

func TestFallbackRoute_NoFallbackOnClientError(t *testing.T) {
	bad := upstream(t, 400, `{"error":{"message":"bad request"}}`)
	good := upstream(t, 200, `{"id":"ok"}`)
	h := setupFallback(t,
		map[string]string{"pa": bad.URL, "pb": good.URL, "pc": good.URL},
		map[string][]string{"a": {"b"}})

	rec := postModel(t, h, "a")
	if rec.Code != 400 {
		t.Fatalf("status %d, want 400 relayed as-is (no fallback on client error)", rec.Code)
	}
}

func TestFallbackRoute_HopLimit(t *testing.T) {
	s500 := upstream(t, 500, `{"error":"boom"}`)
	good := upstream(t, 200, `{"id":"ok"}`)
	// a -> b -> c both fail; chain stops at 3 hops even if more were wired.
	h := setupFallback(t,
		map[string]string{"pa": s500.URL, "pb": s500.URL, "pc": good.URL},
		map[string][]string{"a": {"b"}, "b": {"c"}})
	rec := postModel(t, h, "a")
	if rec.Code != 200 {
		t.Fatalf("status %d, want 200 via a->b->c within hop limit: %s", rec.Code, rec.Body.String())
	}
}

func TestAffinity_PinAfterSuccessAndHit(t *testing.T) {
	good1 := upstream(t, 200, `{"id":"p1","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	good2 := upstream(t, 200, `{"id":"p2","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	p1 := openai.New(openai.Config{Name: "p1", BaseURL: good1.URL, Priority: 1, TimeoutMs: 5000})
	p2 := openai.New(openai.Config{Name: "p2", BaseURL: good2.URL, Priority: 2, TimeoutMs: 5000})
	rt := router.New([]provider.Provider{p1, p2}, []*router.Route{{
		Model: "auto", PromptCacheAffinity: true,
		Candidates: []router.Candidate{{Provider: p1, Model: "m1"}, {Provider: p2, Model: "m2"}},
	}})
	rt.SetAffinity(router.NewAffinityCache(0))
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	h := New(rt, ul, nil)

	body := `{"model":"auto","messages":[{"role":"system","content":"` +
		strings.Repeat("s", 1100) + `"},{"role":"user","content":"u"}]}`
	do := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		return rec
	}
	rec := do() // first request: p1 serves, pin recorded
	if rec.Code != 200 || strings.Contains(rec.Header().Get("X-TokenRoute-Decision"), "affinity=hit") {
		t.Fatalf("first: %d %s", rec.Code, rec.Header().Get("X-TokenRoute-Decision"))
	}
	rec = do() // second: same prefix -> pinned to p1, affinity=hit
	if !strings.Contains(rec.Header().Get("X-TokenRoute-Decision"), "affinity=hit") {
		t.Fatalf("second: decision %q, want affinity=hit", rec.Header().Get("X-TokenRoute-Decision"))
	}
	if !strings.Contains(rec.Header().Get("X-TokenRoute-Decision"), "provider=p1") {
		t.Fatalf("second: decision %q, want provider=p1", rec.Header().Get("X-TokenRoute-Decision"))
	}
}

func TestAffinity_BypassOnCircuitOpen(t *testing.T) {
	good1 := upstream(t, 200, `{"id":"p1","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	good2 := upstream(t, 200, `{"id":"p2","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	p1 := openai.New(openai.Config{Name: "p1", BaseURL: good1.URL, Priority: 1, TimeoutMs: 5000})
	p2 := openai.New(openai.Config{Name: "p2", BaseURL: good2.URL, Priority: 2, TimeoutMs: 5000})
	rt := router.New([]provider.Provider{p1, p2}, []*router.Route{{
		Model: "auto", PromptCacheAffinity: true,
		Candidates: []router.Candidate{{Provider: p1, Model: "m1"}, {Provider: p2, Model: "m2"}},
	}})
	rt.SetAffinity(router.NewAffinityCache(0))
	// Pin p2 (lower priority), then disable p2: pin must be skipped.
	rt.Affinity().Put(router.CachePrefixHash([]byte(affinityBody)), "p2", "m2")
	rt.SetCircuit("p2", router.CircuitConfig{FailureThreshold: 1, CooldownMs: 60000})
	rt.RecordResult("p2", 0, false)
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	h := New(rt, ul, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(affinityBody)))
	if d := rec.Header().Get("X-TokenRoute-Decision"); !strings.Contains(d, "provider=p1") || strings.Contains(d, "affinity=hit") {
		t.Fatalf("decision %q, want provider=p1 without affinity hit (pin filtered)", d)
	}
}

const affinityBody = `{"model":"auto","messages":[{"role":"system","content":"` +
	`xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx` +
	`xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx` +
	`xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx` +
	`xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx` +
	`xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx` +
	`xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx` +
	`xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx` +
	`xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx` +
	`xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx` +
	`xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx` +
	`xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx` +
	`xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx` +
	`xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx` +
	`xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx` +
	`"},{"role":"user","content":"u"}]}`
