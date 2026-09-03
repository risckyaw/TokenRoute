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

func quotaTestSrv(t *testing.T) *srv {
	t.Helper()
	p1 := openai.New(openai.Config{Name: "p1", BaseURL: "http://x", Priority: 1})
	rt := router.New([]provider.Provider{p1}, nil)
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	return &srv{router: rt, usage: ul}
}

func TestParseOIDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"45":    45 * time.Second,
		"1s":    time.Second,
		"1m30s": 90 * time.Second,
		"6h":    6 * time.Hour,
		"2m30s": 150 * time.Second,
		"":      0,
		"abc":   0,
		"-5":    0,
		"0":     0,
	}
	for in, want := range cases {
		if got := parseOIDuration(in); got != want {
			t.Errorf("parseOIDuration(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestObserveUpstreamQuotaHeaders(t *testing.T) {
	s := quotaTestSrv(t)
	// [OI]-style
	h := http.Header{
		"X-Ratelimit-Remaining-Tokens": []string{"12345"},
		"X-Ratelimit-Reset-Tokens":     []string{"1m30s"},
	}
	s.observeUpstreamQuota("p1", "m1", h)
	rem, _, known := s.router.Quota().Remaining("p1", "m1")
	if !known || rem != 12345 {
		t.Fatalf("remaining = %d known=%v, want 12345 true", rem, known)
	}
	if reset := s.router.Quota().WindowReset("p1", "m1"); reset.Before(time.Now().Add(89 * time.Second)) {
		t.Fatalf("reset = %v, want ~90s out", reset)
	}

	// Anthropic-style prefix
	h2 := http.Header{
		"Anthropic-Ratelimit-Remaining-Tokens": []string{"777"},
		"Anthropic-Ratelimit-Reset-Tokens":     []string{"45s"},
	}
	s.observeUpstreamQuota("p2", "m2", h2)
	if rem, _, known := s.router.Quota().Remaining("p2", "m2"); !known || rem != 777 {
		t.Fatalf("anthropic remaining = %d known=%v, want 777 true", rem, known)
	}
}

func TestObserveUpstreamQuotaAbsentNoop(t *testing.T) {
	s := quotaTestSrv(t)
	s.observeUpstreamQuota("p", "m", http.Header{})
	if _, _, known := s.router.Quota().Remaining("p", "m"); known {
		t.Fatal("absent headers must not create an observation")
	}
	// garbage values = no-op
	s.observeUpstreamQuota("p", "m", http.Header{"X-Ratelimit-Remaining-Tokens": []string{"junk"}})
	if _, _, known := s.router.Quota().Remaining("p", "m"); known {
		t.Fatal("invalid remaining must not create an observation")
	}
}

// End-to-end: a 200 response's rate-limit headers land in the ledger.
func TestObserveUpstreamQuotaEndToEnd(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Ratelimit-Remaining-Tokens", "999")
		w.Header().Set("X-Ratelimit-Reset-Tokens", "30s")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(up.Close)
	p1 := openai.New(openai.Config{Name: "p1", BaseURL: up.URL, Priority: 1})
	rt := router.New([]provider.Provider{p1}, []*router.Route{{
		Model:      "auto",
		Candidates: []router.Candidate{{Provider: p1, Model: "m1"}},
	}})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	h := New(rt, ul, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"auto","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	rem, _, known := rt.Quota().Remaining("p1", "m1")
	if !known || rem != 999 {
		t.Fatalf("ledger remaining = %d known=%v, want 999 true", rem, known)
	}
}
