package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

func ctxSetup(t *testing.T, prices map[string]usage.Price, cands ...router.Candidate) http.Handler {
	t.Helper()
	provs := []provider.Provider{}
	for _, c := range cands {
		provs = append(provs, c.Provider)
	}
	rt := router.New(provs, []*router.Route{{Model: "auto", Candidates: cands}})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	return NewWithOptions(Options{Router: rt, Usage: ul, Prices: usage.NewPriceStore(prices)})
}

func bigPrompt(n int) string {
	msg := map[string]any{"model": "auto", "messages": []map[string]string{{"role": "user", "content": strings.Repeat("a", n)}}}
	b, _ := json.Marshal(msg)
	return string(b)
}

func TestContextGuard_RejectsOversize(t *testing.T) {
	fp := &fakeProvider{nonStream: true, body: `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`}
	h := ctxSetup(t, map[string]usage.Price{
		"up-model": {PromptPer1M: 1, ContextTokens: 100},
	}, router.Candidate{Provider: fp, Model: "up-model"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(bigPrompt(1000))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body)
	}
	var out map[string]map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["error"]["type"] != "context_length_exceeded" {
		t.Fatalf("error type = %q", out["error"]["type"])
	}

	// Small prompt passes.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(bigPrompt(40))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
}

func TestContextGuard_SkipsToLargerWindow(t *testing.T) {
	small := &fakeProvider{nonStream: true, body: `{"choices":[]}`}
	large := &fakeProvider{nonStream: true, body: `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`}
	smallP := &prioProvider{fakeProvider: small, name: "small", prio: 1}
	largeP := &prioProvider{fakeProvider: large, name: "large", prio: 2}
	h := ctxSetup(t, map[string]usage.Price{
		"m-small": {ContextTokens: 100},
		"m-large": {ContextTokens: 100000},
	},
		router.Candidate{Provider: smallP, Model: "m-small"},
		router.Candidate{Provider: largeP, Model: "m-large"},
	)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(bigPrompt(1000))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	if d := rec.Header().Get("X-TokenRoute-Decision"); !strings.Contains(d, "provider=large") {
		t.Fatalf("decision = %q, want provider=large", d)
	}
}

// prioProvider gives the fake a name/priority for multi-candidate routes.
type prioProvider struct {
	*fakeProvider
	name string
	prio int
}

func (p *prioProvider) Name() string  { return p.name }
func (p *prioProvider) Priority() int { return p.prio }
