package server

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/provider/openai"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// setupRetry builds a 2-candidate gateway with a retry policy installed.
func setupRetry(t *testing.T, base1, base2 string, rp *router.RetryPolicy) http.Handler {
	t.Helper()
	p1 := openai.New(openai.Config{Name: "p1", BaseURL: base1, Priority: 1, TimeoutMs: 5000})
	p2 := openai.New(openai.Config{Name: "p2", BaseURL: base2, Priority: 2, TimeoutMs: 5000})
	rt := router.New([]provider.Provider{p1, p2}, []*router.Route{{
		Model: "auto",
		Candidates: []router.Candidate{
			{Provider: p1, Model: "m1"},
			{Provider: p2, Model: "m2"},
		},
	}})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	return NewWithOptions(Options{Router: rt, Usage: ul, RetryPolicy: rp})
}

// Policy makes 400 retryable: a 400 from p1 must fail over to p2.
func TestRetryPolicy_CustomRetryableStatus(t *testing.T) {
	u1 := upstream(t, 400, `{"error":"weird"}`)
	u2 := upstream(t, 200, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	rp, err := router.NewRetryPolicy("400,429,500-503", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := postModel(t, setupRetry(t, u1.URL, u2.URL, rp), "auto")
	if rec.Code != 200 {
		t.Fatalf("status %d, want 200 (failover on configured 400)", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("X-TokenRoute-Decision"), "provider=p2") {
		t.Fatalf("decision = %q, want p2", rec.Header().Get("X-TokenRoute-Decision"))
	}
}

// never_retry 503: p1's 503 is relayed as-is (no failover to p2).
func TestRetryPolicy_NeverRetry(t *testing.T) {
	u1 := upstream(t, 503, `{"error":"down"}`)
	u2 := upstream(t, 200, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	rp, err := router.NewRetryPolicy("429,500-504", "", []int{503}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := postModel(t, setupRetry(t, u1.URL, u2.URL, rp), "auto")
	if rec.Code != 503 {
		t.Fatalf("status %d, want 503 (503 excluded from retry)", rec.Code)
	}
}

// Disable keyword in a retryable 429 body reclassifies to quota_exhausted:
// failure kind feeds the 15-minute quota lock instead of the 30s one.
func TestRetryPolicy_DisableKeywordQuotaLock(t *testing.T) {
	u1 := upstream(t, 429, `{"error":"insufficient balance"}`)
	u2 := upstream(t, 200, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	p1 := openai.New(openai.Config{Name: "p1", BaseURL: u1.URL, Priority: 1, TimeoutMs: 5000})
	p2 := openai.New(openai.Config{Name: "p2", BaseURL: u2.URL, Priority: 2, TimeoutMs: 5000})
	rt := router.New([]provider.Provider{p1, p2}, []*router.Route{{
		Model: "auto",
		Candidates: []router.Candidate{
			{Provider: p1, Model: "m1"},
			{Provider: p2, Model: "m2"},
		},
	}})
	rp, err := router.NewRetryPolicy("429,500-503", "", nil, []string{"insufficient balance"})
	if err != nil {
		t.Fatal(err)
	}
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	h := NewWithOptions(Options{Router: rt, Usage: ul, RetryPolicy: rp})
	rec := postModel(t, h, "auto")
	if rec.Code != 200 {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	until := rt.ModelLockUntil("p1", "m1")
	if until.IsZero() {
		t.Fatal("p1/m1 must be model-locked after the 429")
	}
	// Quota lock is 15 minutes; a plain rate-limit lock would be 30s.
	if d := time.Until(until); d < 10*time.Minute {
		t.Fatalf("lock %v too short for quota exhaustion", d)
	}
}

// No policy: built-in behavior unchanged (400 relayed, no failover).
func TestRetryPolicy_NilKeepsBuiltins(t *testing.T) {
	u1 := upstream(t, 400, `{"error":"bad"}`)
	u2 := upstream(t, 200, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	rec := postModel(t, setupRetry(t, u1.URL, u2.URL, nil), "auto")
	if rec.Code != 400 {
		t.Fatalf("status %d, want 400 relayed (no policy)", rec.Code)
	}
}
