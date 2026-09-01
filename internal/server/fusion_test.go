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

// fusionUpstream answers /chat/completions with status+body after delay.
func fusionUpstream(t *testing.T, status int, body string, delay time.Duration, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

func setupFusion(t *testing.T, base1, base2 string, prices map[string]usage.Price) http.Handler {
	t.Helper()
	p1 := openai.New(openai.Config{Name: "fA", BaseURL: base1, Priority: 1, TimeoutMs: 5000})
	p2 := openai.New(openai.Config{Name: "fB", BaseURL: base2, Priority: 2, TimeoutMs: 5000})
	rt := router.New([]provider.Provider{p1, p2}, []*router.Route{{
		Model: "auto", Strategy: "fusion",
		Candidates: []router.Candidate{
			{Provider: p1, Model: "mA"},
			{Provider: p2, Model: "mB"},
		},
	}})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	return NewWithOptions(Options{Router: rt, Usage: ul, Prices: prices})
}

func postFusion(t *testing.T, h http.Handler, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"auto","messages":[]}`
	if stream {
		body = `{"model":"auto","messages":[],"stream":true}`
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	return rec
}

const okUsage = `"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`

func TestFusion_FasterWins(t *testing.T) {
	// B faster AND cheaper: must win even though A is the first candidate.
	var hitsA, hitsB atomic.Int64
	a := fusionUpstream(t, 200, `{"id":"A",`+okUsage+`}`, 50*time.Millisecond, &hitsA)
	b := fusionUpstream(t, 200, `{"id":"B",`+okUsage+`}`, 0, &hitsB)
	prices := map[string]usage.Price{
		"mA": {PromptPer1M: 10, CompletionPer1M: 10},
		"mB": {PromptPer1M: 1, CompletionPer1M: 1},
	}
	h := setupFusion(t, a.URL, b.URL, prices)

	rec := postFusion(t, h, false)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"id":"B"`) {
		t.Fatalf("body = %q, want B (faster+cheaper)", rec.Body.String())
	}
	dec := rec.Header().Get("X-TokenRoute-Decision")
	if !strings.Contains(dec, "attempts=2") || !strings.Contains(dec, ";fusion=1") {
		t.Fatalf("decision = %q", dec)
	}
}

func TestFusion_CheaperWins(t *testing.T) {
	a := fusionUpstream(t, 200, `{"id":"A",`+okUsage+`}`, 0, nil)
	b := fusionUpstream(t, 200, `{"id":"B",`+okUsage+`}`, 0, nil)
	prices := map[string]usage.Price{
		"mA": {PromptPer1M: 1, CompletionPer1M: 1}, // cheaper
		"mB": {PromptPer1M: 10, CompletionPer1M: 10},
	}
	h := setupFusion(t, a.URL, b.URL, prices)

	rec := postFusion(t, h, false)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"id":"A"`) {
		t.Fatalf("body = %q, want A (cheaper)", rec.Body.String())
	}
	if dec := rec.Header().Get("X-TokenRoute-Decision"); !strings.Contains(dec, "model=mA") {
		t.Fatalf("decision = %q", dec)
	}
}

func TestFusion_OneFailsOtherWins(t *testing.T) {
	a := fusionUpstream(t, 500, `{"error":"down"}`, 0, nil)
	b := fusionUpstream(t, 200, `{"id":"B",`+okUsage+`}`, 0, nil)
	h := setupFusion(t, a.URL, b.URL, nil)

	rec := postFusion(t, h, false)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"id":"B"`) {
		t.Fatalf("body = %q, want B", rec.Body.String())
	}
}

func TestFusion_StreamFallsBackToFirstOnly(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	a := fusionUpstream(t, 200, "data: [DONE]\n\n", 0, &hitsA)
	b := fusionUpstream(t, 200, "data: [DONE]\n\n", 0, &hitsB)
	h := setupFusion(t, a.URL, b.URL, nil)

	rec := postFusion(t, h, true)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if hitsA.Load() != 1 || hitsB.Load() != 0 {
		t.Fatalf("stream hits A=%d B=%d, want 1/0 (priority fallback)", hitsA.Load(), hitsB.Load())
	}
	if strings.Contains(rec.Header().Get("X-TokenRoute-Decision"), "fusion=1") {
		t.Fatal("stream must not be fused")
	}
}
