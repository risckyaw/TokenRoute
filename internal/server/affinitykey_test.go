package server

import (
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

// Header-key affinity: pin by X-Session-Id value hash; roundtrip pins to p1,
// distinct values isolate pins.
func TestAffinityKeyHeader_Roundtrip(t *testing.T) {
	good1 := upstream(t, 200, `{"id":"p1","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	good2 := upstream(t, 200, `{"id":"p2","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	p1 := openai.New(openai.Config{Name: "p1", BaseURL: good1.URL, Priority: 1, TimeoutMs: 5000})
	p2 := openai.New(openai.Config{Name: "p2", BaseURL: good2.URL, Priority: 2, TimeoutMs: 5000})
	rt := router.New([]provider.Provider{p1, p2}, []*router.Route{{
		Model: "auto", AffinityKeyHeader: "X-Session-Id",
		Candidates: []router.Candidate{{Provider: p1, Model: "m1"}, {Provider: p2, Model: "m2"}},
	}})
	rt.SetAffinity(router.NewAffinityCache(0))
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	h := New(rt, ul, nil)

	do := func(session string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"auto","messages":[]}`))
		if session != "" {
			req.Header.Set("X-Session-Id", session)
		}
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := do("sess-1")
	if rec.Code != 200 || strings.Contains(rec.Header().Get("X-TokenRoute-Decision"), "affinity=hit") {
		t.Fatalf("first: %d %s", rec.Code, rec.Header().Get("X-TokenRoute-Decision"))
	}
	rec = do("sess-1")
	d := rec.Header().Get("X-TokenRoute-Decision")
	if !strings.Contains(d, "affinity=hit") || !strings.Contains(d, "aff=h") {
		t.Fatalf("second: decision %q, want affinity=hit + aff=h", d)
	}
	if !strings.Contains(d, "provider=p1") {
		t.Fatalf("second: decision %q, want provider=p1", d)
	}
	// A different session value must NOT hit the sess-1 pin.
	rec = do("sess-2")
	if strings.Contains(rec.Header().Get("X-TokenRoute-Decision"), "affinity=hit") {
		t.Fatalf("distinct header value must not hit: %q", rec.Header().Get("X-TokenRoute-Decision"))
	}
	// No header: no pinning at all (falls back to nothing — prefix too small).
	rec = do("")
	if strings.Contains(rec.Header().Get("X-TokenRoute-Decision"), "affinity=hit") {
		t.Fatalf("no header must not hit: %q", rec.Header().Get("X-TokenRoute-Decision"))
	}
}

// skip_retry_on_failure: pinned request that fails retryably must relay the
// failure, NOT fail over to the other candidate.
func TestAffinityKeyHeader_SkipRetryBlocksFailover(t *testing.T) {
	bad1 := upstream(t, 500, `{"error":"down"}`)
	good2 := upstream(t, 200, `{"id":"p2","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	p1 := openai.New(openai.Config{Name: "p1", BaseURL: bad1.URL, Priority: 1, TimeoutMs: 5000})
	p2 := openai.New(openai.Config{Name: "p2", BaseURL: good2.URL, Priority: 2, TimeoutMs: 5000})
	rt := router.New([]provider.Provider{p1, p2}, []*router.Route{{
		Model: "auto", AffinityKeyHeader: "X-Session-Id", AffinitySkipRetry: true,
		Candidates: []router.Candidate{{Provider: p1, Model: "m1"}, {Provider: p2, Model: "m2"}},
	}})
	rt.SetAffinity(router.NewAffinityCache(0))
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	h := New(rt, ul, nil)

	do := func(session string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"auto","messages":[]}`))
		if session != "" {
			req.Header.Set("X-Session-Id", session)
		}
		h.ServeHTTP(rec, req)
		return rec
	}

	// Seed a pin for sess-x onto p2 via a direct cache write (p2 is healthy).
	rt.Affinity().Put(router.HeaderKeyHash("sess-x"), "p2", "m2")
	rec := do("sess-x") // pin hit -> p2 serves
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("X-TokenRoute-Decision"), "affinity=hit") {
		t.Fatalf("seed: %d %s", rec.Code, rec.Header().Get("X-TokenRoute-Decision"))
	}

	// Now pin sess-y onto FAILING p1: hit + skip_retry => 500 relayed, no failover to p2.
	rt.Affinity().Put(router.HeaderKeyHash("sess-y"), "p1", "m1")
	rec = do("sess-y")
	if rec.Code != 500 {
		t.Fatalf("skip_retry: status %d, want 500 relayed (no failover)", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("X-TokenRoute-Decision"), "affinity=hit") {
		t.Fatalf("skip_retry: decision %q, want affinity=hit", rec.Header().Get("X-TokenRoute-Decision"))
	}
}

// Without skip_retry (default), a pinned-then-failing candidate fails over
// normally.
func TestAffinityKeyHeader_NoSkipRetryFailsOver(t *testing.T) {
	bad1 := upstream(t, 500, `{"error":"down"}`)
	good2 := upstream(t, 200, `{"id":"p2","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	p1 := openai.New(openai.Config{Name: "p1", BaseURL: bad1.URL, Priority: 1, TimeoutMs: 5000})
	p2 := openai.New(openai.Config{Name: "p2", BaseURL: good2.URL, Priority: 2, TimeoutMs: 5000})
	rt := router.New([]provider.Provider{p1, p2}, []*router.Route{{
		Model: "auto", AffinityKeyHeader: "X-Session-Id",
		Candidates: []router.Candidate{{Provider: p1, Model: "m1"}, {Provider: p2, Model: "m2"}},
	}})
	rt.SetAffinity(router.NewAffinityCache(0))
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	h := New(rt, ul, nil)

	rt.Affinity().Put(router.HeaderKeyHash("sess-y"), "p1", "m1")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"auto","messages":[]}`))
	req.Header.Set("X-Session-Id", "sess-y")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("no skip_retry: status %d, want 200 (failover to p2)", rec.Code)
	}
}
