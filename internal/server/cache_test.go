package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/provider/openai"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

func setupCache(t *testing.T, hits *atomic.Int64, ttlSec int) (http.Handler, *usage.Logger) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"c1","usage":{"prompt_tokens":5,"completion_tokens":5,"total_tokens":10}}`))
	}))
	t.Cleanup(up.Close)
	p1 := openai.New(openai.Config{Name: "c1", BaseURL: up.URL, Priority: 1, TimeoutMs: 5000})
	rt := router.New([]provider.Provider{p1}, []*router.Route{{
		Model: "auto", Candidates: []router.Candidate{{Provider: p1, Model: "m1"}},
	}})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	return NewWithOptions(Options{Router: rt, Usage: ul, Cache: NewCache(true, ttlSec)}), ul
}

func chatBody(stream bool) string {
	if stream {
		return `{"model":"auto","messages":[{"role":"user","content":"hi"}],"stream":true}`
	}
	return `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`
}

func postCacheChat(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	return rec
}

func TestCache_HitOnSecondRequest(t *testing.T) {
	var hits atomic.Int64
	h, ul := setupCache(t, &hits, 60)

	r1 := postCacheChat(t, h, chatBody(false))
	if r1.Code != 200 {
		t.Fatalf("first status %d", r1.Code)
	}
	if r1.Header().Get("X-TokenRoute-Cache") == "HIT" {
		t.Fatal("first request must not be a HIT")
	}
	r2 := postCacheChat(t, h, chatBody(false))
	if r2.Code != 200 {
		t.Fatalf("second status %d", r2.Code)
	}
	if r2.Header().Get("X-TokenRoute-Cache") != "HIT" {
		t.Fatalf("second request: X-TokenRoute-Cache = %q", r2.Header().Get("X-TokenRoute-Cache"))
	}
	if r1.Body.String() != r2.Body.String() {
		t.Fatalf("cached body differs: %q vs %q", r1.Body, r2.Body)
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits.Load())
	}
	if dec := r2.Header().Get("X-TokenRoute-Decision"); !strings.Contains(dec, "provider=cache") {
		t.Fatalf("decision = %q", dec)
	}
	entries, err := ul.QueryRecent(10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries %v %v", entries, err)
	}
	hit := entries[0] // newest first
	if !hit.Cached || hit.Provider != "cache" {
		t.Fatalf("hit entry = %+v", hit)
	}
	if hit.CostUSD == nil || *hit.CostUSD != 0 {
		t.Fatalf("hit cost = %v want 0", hit.CostUSD)
	}
	if entries[1].Cached {
		t.Fatal("first entry must not be cached")
	}
}

func TestCache_TTLExpiry(t *testing.T) {
	var hits atomic.Int64
	h, _ := setupCache(t, &hits, 1) // 1s TTL
	postCacheChat(t, h, chatBody(false))
	postCacheChat(t, h, chatBody(false))
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1 before expiry", hits.Load())
	}
	time.Sleep(1100 * time.Millisecond)
	postCacheChat(t, h, chatBody(false))
	if hits.Load() != 2 {
		t.Fatalf("hits after TTL = %d, want 2", hits.Load())
	}
}

func TestCache_StreamBypass(t *testing.T) {
	var hits atomic.Int64
	h, _ := setupCache(t, &hits, 60)
	r1 := postCacheChat(t, h, chatBody(true))
	r2 := postCacheChat(t, h, chatBody(true))
	if hits.Load() != 2 {
		t.Fatalf("stream upstream hits = %d, want 2 (no cache)", hits.Load())
	}
	if r1.Header().Get("X-TokenRoute-Cache") == "HIT" || r2.Header().Get("X-TokenRoute-Cache") == "HIT" {
		t.Fatal("stream must never HIT")
	}
}
