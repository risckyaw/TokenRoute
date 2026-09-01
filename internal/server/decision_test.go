package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/provider/openai"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

func TestDecisionHeader_200(t *testing.T) {
	good := upstream(t, 200, `{"id":"ok"}`)
	h := setup(t, good.URL, good.URL, nil)
	rec := post(t, h)
	got := rec.Header().Get("X-TokenRoute-Decision")
	if got != "provider=p1;model=m1;strategy=priority;attempts=1" {
		t.Fatalf("decision %q", got)
	}
}

func TestDecisionHeader_FailoverAttempts2(t *testing.T) {
	bad := upstream(t, 500, `{"error":"boom"}`)
	good := upstream(t, 200, `{"id":"ok"}`)
	h := setup(t, bad.URL, good.URL, nil)
	rec := post(t, h)
	got := rec.Header().Get("X-TokenRoute-Decision")
	if got != "provider=p2;model=m2;strategy=priority;attempts=2" {
		t.Fatalf("decision %q", got)
	}
}

func TestDecisionHeader_AllFail502(t *testing.T) {
	h := setup(t, deadURL(t), deadURL(t), nil)
	rec := post(t, h)
	if rec.Code != 502 {
		t.Fatalf("status %d, want 502", rec.Code)
	}
	got := rec.Header().Get("X-TokenRoute-Decision")
	if !strings.HasSuffix(got, ";attempts=2") {
		t.Fatalf("decision %q, want attempts=2", got)
	}
}

func TestRetryAfter429OpensCircuit(t *testing.T) {
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer limited.Close()
	good := upstream(t, 200, `{"id":"ok"}`)

	p1 := openai.New(openai.Config{Name: "p1", BaseURL: limited.URL, Priority: 1, TimeoutMs: 5000})
	p2 := openai.New(openai.Config{Name: "p2", BaseURL: good.URL, Priority: 2, TimeoutMs: 5000})
	routes := []*router.Route{{Model: "auto", Candidates: []router.Candidate{
		{Provider: p1, Model: "m1"}, {Provider: p2, Model: "m2"},
	}}}
	rt := router.New([]provider.Provider{p1, p2}, routes)
	rt.SetCircuit("p1", router.CircuitConfig{FailureThreshold: 100, CooldownMs: 1})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	h := New(rt, ul, nil)

	rec := post(t, h)
	if rec.Code != 200 {
		t.Fatalf("status %d, want failover to 200", rec.Code)
	}
	// Circuit opened for ~120s despite high threshold + 1ms cooldown.
	if st := rt.CircuitState("p1"); st != "open" {
		t.Fatalf("circuit %s, want open", st)
	}
	if rem := time.Until(rt.CircuitOpenUntil("p1")); rem < 60*time.Second {
		t.Fatalf("remaining %v, want >60s (Retry-After 120)", rem)
	}
	// And the 429 locked the model: next request skips p1 entirely.
	if !rt.IsModelLocked("p1", "m1") {
		t.Fatal("p1|m1 not locked after 429")
	}
}

func TestModelLockout404(t *testing.T) {
	missing := upstream(t, 404, `{"error":{"message":"model not found"}}`)
	good := upstream(t, 200, `{"id":"ok"}`)
	h := setup(t, missing.URL, good.URL, nil)

	// 404 relays as-is (no failover) but locks p1|m1.
	rec := post(t, h)
	if rec.Code != 404 {
		t.Fatalf("status %d, want 404 relayed", rec.Code)
	}
	// Wait for the lock to be visible, then p1 must be skipped on next request.
	// The lock was set synchronously before relay, so it's immediate.
	hits := 0
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"p2"}`))
	}))
	defer counting.Close()

	p1 := openai.New(openai.Config{Name: "p1", BaseURL: missing.URL, Priority: 1, TimeoutMs: 5000})
	p2 := openai.New(openai.Config{Name: "p2", BaseURL: counting.URL, Priority: 2, TimeoutMs: 5000})
	routes := []*router.Route{{Model: "auto", Candidates: []router.Candidate{
		{Provider: p1, Model: "m1"}, {Provider: p2, Model: "m2"},
	}}}
	rt := router.New([]provider.Provider{p1, p2}, routes)
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	h2 := New(rt, ul, nil)

	post(t, h2) // 404 -> locks p1|m1
	if !rt.IsModelLocked("p1", "m1") {
		t.Fatal("p1|m1 not locked after 404")
	}
	rec = post(t, h2)
	if rec.Code != 200 || hits != 1 {
		t.Fatalf("status %d hits %d, want 200 served by p2 only", rec.Code, hits)
	}
}

func TestTokenAndCostHeaders(t *testing.T) {
	good := upstream(t, 200, `{"id":"ok","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	p1 := openai.New(openai.Config{Name: "p1", BaseURL: good.URL, Priority: 1, TimeoutMs: 5000})
	routes := []*router.Route{{Model: "auto", Candidates: []router.Candidate{{Provider: p1, Model: "m1"}}}}
	rt := router.New([]provider.Provider{p1}, routes)
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	prices := map[string]usage.Price{"m1": {PromptPer1M: 2, CompletionPer1M: 4}}
	h := New(rt, ul, prices)

	rec := post(t, h)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	hdr := rec.Header()
	if hdr.Get("X-TokenRoute-Prompt-Tokens") != "10" ||
		hdr.Get("X-TokenRoute-Completion-Tokens") != "5" ||
		hdr.Get("X-TokenRoute-Total-Tokens") != "15" {
		t.Fatalf("token headers: %v", hdr)
	}
	// cost = 10*2/1e6 + 5*4/1e6 = 0.00004
	if got := hdr.Get("X-TokenRoute-Cost-USD"); got != "0.00004" {
		t.Fatalf("cost %q, want 0.00004", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter("120"); d != 120*time.Second {
		t.Fatalf("seconds: %v", d)
	}
	if d := parseRetryAfter("garbage"); d != 0 {
		t.Fatalf("garbage: %v", d)
	}
	if d := parseRetryAfter(""); d != 0 {
		t.Fatalf("empty: %v", d)
	}
	if d := parseRetryAfter(time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)); d < 60*time.Second {
		t.Fatalf("http-date: %v", d)
	}
	if d := parseRetryAfter("-5"); d != 0 {
		t.Fatalf("negative: %v", d)
	}
}
