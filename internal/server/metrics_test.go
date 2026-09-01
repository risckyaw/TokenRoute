package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/metrics"
	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

func TestMetrics_Scrape(t *testing.T) {
	fp := &fakeProvider{nonStream: true, body: `{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`}
	rt := router.New([]provider.Provider{fp}, []*router.Route{{
		Model:      "auto",
		Candidates: []router.Candidate{{Provider: fp, Model: "up-model"}},
	}})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	m := metrics.New()
	m.Providers = func() []string { return []string{"fake"} }
	m.CircuitOpen = func(string) bool { return false }
	h := NewWithOptions(Options{Router: rt, Usage: ul, Metrics: m})

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"auto"}`)))
		if rec.Code != 200 {
			t.Fatalf("req %d status %d", i, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("metrics status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `tokenroute_requests_total{key="",provider="fake",model="up-model",status_class="2xx"} 2`) {
		t.Fatalf("missing requests counter:\n%s", body)
	}
	if !strings.Contains(body, `tokenroute_tokens_total{key="",provider="fake",kind="prompt"} 6`) {
		t.Fatalf("missing prompt tokens:\n%s", body)
	}
	if !strings.Contains(body, `tokenroute_tokens_total{key="",provider="fake",kind="completion"} 8`) {
		t.Fatalf("missing completion tokens:\n%s", body)
	}
	if !strings.Contains(body, `tokenroute_latency_seconds_bucket{provider="fake",le="0.05"}`) {
		t.Fatalf("missing histogram:\n%s", body)
	}
	if !strings.Contains(body, `tokenroute_circuit_open{provider="fake"} 0`) {
		t.Fatalf("missing gauge:\n%s", body)
	}
	// No duplicated counter lines.
	if strings.Count(body, "tokenroute_requests_total{") != 1 {
		t.Fatalf("duplicated request series:\n%s", body)
	}
}
