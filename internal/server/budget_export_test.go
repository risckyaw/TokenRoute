package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/auth"
	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

func budgetSetup(t *testing.T) (http.Handler, *usage.Logger) {
	t.Helper()
	fp := &fakeProvider{
		nonStream: true,
		body:      `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":100,"total_tokens":200}}`,
	}
	rt := router.New([]provider.Provider{fp}, []*router.Route{{
		Model:      "auto",
		Candidates: []router.Candidate{{Provider: fp, Model: "up-model"}},
	}})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	prices := map[string]usage.Price{
		"up-model": {PromptPer1M: 1.0, CompletionPer1M: 1.0}, // $2/1M combined
	}
	return NewWithOptions(Options{Router: rt, Usage: ul, Prices: usage.NewPriceStore(prices)}), ul
}

func TestBudget_PreFlight402(t *testing.T) {
	h, _ := budgetSetup(t)
	// max_tokens 10000 -> est = 10000/1e6 * 2 = $0.02 > budget 0.01
	body := `{"model":"auto","max_tokens":10000}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-Max-Cost-USD", "0.01")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402; body %s", rec.Code, rec.Body)
	}
	var out map[string]map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["error"]["type"] != "budget_exceeded" {
		t.Fatalf("error type = %q", out["error"]["type"])
	}
}

func TestBudget_AllowsAndFlagsExceeded(t *testing.T) {
	h, ul := budgetSetup(t)

	// Tiny budget, no max_tokens -> default 4096 est = $0.008192 <= 0.01: passes.
	// Actual cost: 200 tok -> $0.0002 <= budget: not exceeded.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto"}`))
	req.Header.Set("X-Max-Cost-USD", "0.01")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}

	// Budget below actual cost ($0.0002) but estimate passes only if max_tokens
	// small: max_tokens=1 -> est 2e-6 <= 1e-4 passes; actual 0.0002 > 0.0001.
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"auto","max_tokens":1}`))
	req.Header.Set("X-Max-Cost-USD", "0.0001")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}

	entries, err := ul.QueryRecent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if !entries[0].BudgetExceeded {
		t.Fatal("latest entry should have budget_exceeded=true")
	}
	if entries[1].BudgetExceeded {
		t.Fatal("first entry should have budget_exceeded=false")
	}
}

func TestBudget_UnknownPriceAllows(t *testing.T) {
	fp := &fakeProvider{nonStream: true, body: `{"choices":[]}`}
	rt := router.New([]provider.Provider{fp}, []*router.Route{{
		Model:      "auto",
		Candidates: []router.Candidate{{Provider: fp, Model: "unpriced"}},
	}})
	h := NewWithOptions(Options{Router: rt, Prices: usage.NewPriceStore(nil)})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto"}`))
	req.Header.Set("X-Max-Cost-USD", "0.0000001")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unknown price allows)", rec.Code)
	}
}

func TestAdminUsageExport_CSV(t *testing.T) {
	db, err := usage.OpenDB(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ul := usage.NewLogger(db)
	keys, err := auth.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	cost := 0.001
	if err := ul.Log(context.Background(), usage.Entry{
		RequestID: "r1", TS: time.Now(), KeyName: "k1",
		VirtualModel: "auto", Provider: "fake", Model: "up-model",
		PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
		Status: 200, LatencyMs: 42, CostUSD: &cost, BudgetExceeded: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ul.Log(context.Background(), usage.Entry{
		RequestID: "r0", TS: time.Now().Add(-48 * time.Hour), // outside default window
		VirtualModel: "auto", Provider: "fake", Model: "up-model", Status: 200,
	}); err != nil {
		t.Fatal(err)
	}

	h := NewAdminOnly(Options{Usage: ul, Keys: keys, AdminKey: "adm"})
	req := httptest.NewRequest(http.MethodGet, "/admin/usage/export?format=csv", nil)
	req.Header.Set("X-Admin-Key", "adm")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/csv" {
		t.Fatalf("content-type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "tokenroute-usage.csv") {
		t.Fatalf("content-disposition = %q", cd)
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 2 { // header + 1 in-range row
		t.Fatalf("rows = %d, want 2: %q", len(lines), rec.Body.String())
	}
	wantHeader := "id,ts,key_name,virtual_model,provider,model,prompt_tokens,completion_tokens,total_tokens,stream,status,latency_ms,cost_usd,multiplier,budget_exceeded,cached"
	if lines[0] != wantHeader {
		t.Fatalf("header = %q", lines[0])
	}
	if !strings.Contains(lines[1], "k1,auto,fake,up-model,10,5,15,false,200,42,0.001,1,true,false") {
		t.Fatalf("data row = %q", lines[1])
	}

	// Auth required.
	req = httptest.NewRequest(http.MethodGet, "/admin/usage/export", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status = %d, want 401", rec.Code)
	}
}
