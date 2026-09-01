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

func TestKeyBudget_402AfterExhausted(t *testing.T) {
	fp := &fakeProvider{nonStream: true,
		body: `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":100,"total_tokens":200}}`}
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
	keys, err := auth.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	// 200 tokens at $2/1M combined -> $0.0002 > budget 0.0001.
	created, err := keys.Create(auth.Key{Name: "b", Enabled: true, BudgetUSD: 0.0001})
	if err != nil {
		t.Fatal(err)
	}
	prices := map[string]usage.Price{"up-model": {PromptPer1M: 1, CompletionPer1M: 1}}
	h := NewWithOptions(Options{Router: rt, Usage: ul, Prices: prices, Keys: keys})

	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto"}`))
		req.Header.Set("Authorization", "Bearer "+created.Key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := do(); rec.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	rec := do()
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("second status = %d, want 402; body %s", rec.Code, rec.Body)
	}
	var out map[string]map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["error"]["type"] != "budget_exceeded" {
		t.Fatalf("error type = %q", out["error"]["type"])
	}

	// spent_usd persisted.
	k, err := keys.GetByKey(created.Key)
	if err != nil || k == nil {
		t.Fatalf("get key: %v", err)
	}
	if k.SpentUSD < 0.0001 {
		t.Fatalf("spent_usd = %v, want >= 0.0001", k.SpentUSD)
	}
}
