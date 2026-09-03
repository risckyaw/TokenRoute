package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// Expression pricing end-to-end: tiered boundary + tier name in the log.
func TestExprPricing_TieredCost(t *testing.T) {
	body := `{"id":"1","model":"up","choices":[],"usage":{"prompt_tokens":200001,"completion_tokens":100,"total_tokens":200101}}`
	fp := &fakeProvider{body: body, nonStream: true}
	rt := router.New([]provider.Provider{fp}, []*router.Route{{
		Model:      "auto",
		Candidates: []router.Candidate{{Provider: fp, Model: "up"}},
	}})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	prices := map[string]usage.Price{
		"up": {Expr: `p <= 200000 ? tier("standard", p*1.5 + c*7.5) : tier("long_context", p*3.0 + c*11.25)`},
	}
	h := New(rt, ul, prices)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"auto"}`))
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	wantCost := (200001*3.0 + 100*11.25) / 1e6
	if got := rec.Header().Get("X-TokenRoute-Cost-USD"); got == "" {
		t.Fatal("missing cost header")
	}
	entries, err := ul.QueryRecent(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.CostUSD == nil || *e.CostUSD-wantCost > 1e-12 || wantCost-*e.CostUSD > 1e-12 {
		t.Fatalf("cost = %v, want %v", e.CostUSD, wantCost)
	}
	if e.PriceTier != "long_context" {
		t.Fatalf("price_tier = %q, want long_context", e.PriceTier)
	}
}

// Cache-read subtraction end-to-end ([OI] semantics: cached_tokens subtracted
// from billable p, priced by its own term).
func TestExprPricing_CacheReadSubtraction(t *testing.T) {
	body := `{"id":"1","model":"up","choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":100,"total_tokens":1100,"prompt_tokens_details":{"cached_tokens":400}}}`
	fp := &fakeProvider{body: body, nonStream: true}
	rt := router.New([]provider.Provider{fp}, []*router.Route{{
		Model:      "auto",
		Candidates: []router.Candidate{{Provider: fp, Model: "up"}},
	}})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	prices := map[string]usage.Price{"up": {Expr: `p*2.0 + c*8.0 + cr*0.2`}}
	h := New(rt, ul, prices)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"auto"}`))
	h.ServeHTTP(rec, req)

	entries, err := ul.QueryRecent(1)
	if err != nil {
		t.Fatal(err)
	}
	want := (600*2.0 + 100*8.0 + 400*0.2) / 1e6
	if len(entries) != 1 || entries[0].CostUSD == nil || *entries[0].CostUSD-want > 1e-12 || want-*entries[0].CostUSD > 1e-12 {
		t.Fatalf("entries: %+v, want cost %v", entries, want)
	}
}

// Anthropic semantics: input_tokens is text-only; cache tokens not subtracted.
func TestExprPricing_AnthropicSemantics(t *testing.T) {
	body := `{"id":"1","model":"up","choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":100,"total_tokens":1500,"cache_read_input_tokens":400}}`
	fp := &fakeProvider{body: body, nonStream: true}
	rt := router.New([]provider.Provider{fp}, []*router.Route{{
		Model:      "auto",
		Candidates: []router.Candidate{{Provider: fp, Model: "up"}},
	}})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	prices := map[string]usage.Price{"up": {Expr: `p*3.0 + c*15.0 + cr*0.3`}}
	h := NewWithOptions(Options{
		Router: rt, Usage: ul, Prices: usage.NewPriceStore(prices),
		ProviderTypes: map[string]string{"fake": "anthropic"},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"auto"}`))
	h.ServeHTTP(rec, req)

	entries, err := ul.QueryRecent(1)
	if err != nil {
		t.Fatal(err)
	}
	// p stays 1000 (no subtraction), cr priced separately.
	want := (1000*3.0 + 100*15.0 + 400*0.3) / 1e6
	if len(entries) != 1 || entries[0].CostUSD == nil || *entries[0].CostUSD-want > 1e-12 || want-*entries[0].CostUSD > 1e-12 {
		t.Fatalf("entries: %+v, want cost %v", entries, want)
	}
}

// Flat price fallback when no expr configured (current behavior unchanged).
func TestExprPricing_FlatFallback(t *testing.T) {
	body := `{"id":"1","model":"up","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`
	fp := &fakeProvider{body: body, nonStream: true}
	rt := router.New([]provider.Provider{fp}, []*router.Route{{
		Model:      "auto",
		Candidates: []router.Candidate{{Provider: fp, Model: "up"}},
	}})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	prices := map[string]usage.Price{"up": {PromptPer1M: 2.0, CompletionPer1M: 4.0}}
	h := New(rt, ul, prices)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"auto"}`))
	h.ServeHTTP(rec, req)

	entries, err := ul.QueryRecent(1)
	if err != nil {
		t.Fatal(err)
	}
	want := 100.0/1e6*2.0 + 50.0/1e6*4.0
	if len(entries) != 1 || entries[0].CostUSD == nil || *entries[0].CostUSD-want > 1e-12 || want-*entries[0].CostUSD > 1e-12 {
		t.Fatalf("entries: %+v, want cost %v", entries, want)
	}
	if entries[0].PriceTier != "" {
		t.Fatalf("price_tier = %q, want empty (flat pricing)", entries[0].PriceTier)
	}
}
