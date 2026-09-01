package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/provider/openai"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// embUpstream answers /embeddings with the given status/body and counts hits.
func embUpstream(t *testing.T, status int, body string, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			http.NotFound(w, r)
			return
		}
		if hits != nil {
			hits.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

func setupEmb(t *testing.T, base1, base2 string, prices map[string]usage.Price) (http.Handler, *usage.Logger) {
	t.Helper()
	p1 := openai.New(openai.Config{Name: "e1", BaseURL: base1, Priority: 1, TimeoutMs: 5000})
	provs := []provider.Provider{p1}
	cands := []router.Candidate{{Provider: p1, Model: "emb-1"}}
	if base2 != "" {
		p2 := openai.New(openai.Config{Name: "e2", BaseURL: base2, Priority: 2, TimeoutMs: 5000})
		provs = append(provs, p2)
		cands = append(cands, router.Candidate{Provider: p2, Model: "emb-2"})
	}
	rt := router.New(provs, []*router.Route{{Model: "vec", Candidates: cands}})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	return NewWithOptions(Options{Router: rt, Usage: ul, Prices: prices}), ul
}

func postEmb(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/embeddings",
		strings.NewReader(`{"model":"vec","input":"hello"}`)))
	return rec
}

func TestEmbeddings_PassthroughTokensCostDecision(t *testing.T) {
	up := embUpstream(t, 200, `{"object":"list","data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":7,"total_tokens":7}}`, nil)
	prices := map[string]usage.Price{"emb-1": {PromptPer1M: 2, EmbedPer1M: 0.5}}
	h, ul := setupEmb(t, up.URL, "", prices)

	rec := postEmb(t, h)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if dec := rec.Header().Get("X-TokenRoute-Decision"); !strings.Contains(dec, "provider=e1;model=emb-1") {
		t.Fatalf("decision = %q", dec)
	}
	if !strings.Contains(rec.Body.String(), `"embedding"`) {
		t.Fatalf("body = %q", rec.Body.String())
	}
	entries, err := ul.QueryRecent(1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("usage entries: %v %v", entries, err)
	}
	e := entries[0]
	if e.PromptTokens != 7 || e.CompletionTokens != 0 || e.TotalTokens != 7 {
		t.Fatalf("tokens = %+v", e)
	}
	if e.CostUSD == nil {
		t.Fatal("cost missing")
	}
	// 7 tokens @ 0.5 USD/1M via embed_per_1m.
	want := 7.0 / 1e6 * 0.5
	if *e.CostUSD != want {
		t.Fatalf("cost = %v want %v", *e.CostUSD, want)
	}
}

func TestEmbeddings_EmbedPriceFallbackToPrompt(t *testing.T) {
	up := embUpstream(t, 200, `{"usage":{"prompt_tokens":100,"total_tokens":100}}`, nil)
	prices := map[string]usage.Price{"emb-1": {PromptPer1M: 2}} // no embed_per_1m
	h, ul := setupEmb(t, up.URL, "", prices)
	if rec := postEmb(t, h); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	entries, _ := ul.QueryRecent(1)
	if len(entries) != 1 || entries[0].CostUSD == nil {
		t.Fatalf("entries %+v", entries)
	}
	want := 100.0 / 1e6 * 2
	if *entries[0].CostUSD != want {
		t.Fatalf("cost = %v want %v", *entries[0].CostUSD, want)
	}
}

func TestEmbeddings_FailoverOn500(t *testing.T) {
	bad := embUpstream(t, 500, `{"error":"down"}`, nil)
	good := embUpstream(t, 200, `{"data":[{"embedding":[1]}],"usage":{"prompt_tokens":3,"total_tokens":3}}`, nil)
	h, _ := setupEmb(t, bad.URL, good.URL, nil)

	rec := postEmb(t, h)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if dec := rec.Header().Get("X-TokenRoute-Decision"); !strings.Contains(dec, "provider=e2;model=emb-2;strategy=priority;attempts=2") {
		t.Fatalf("decision = %q", dec)
	}
}

func TestEmbeddings_UnsupportedProvider501(t *testing.T) {
	// fakeProvider (server_test.go) returns 501 for Embed.
	fp := &fakeProvider{}
	rt := router.New([]provider.Provider{fp}, []*router.Route{{
		Model: "vec", Candidates: []router.Candidate{{Provider: fp, Model: "x"}},
	}})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	h := New(rt, ul, nil)

	rec := postEmb(t, h)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status %d want 501", rec.Code)
	}
	var got struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Error.Type != "unsupported" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}
