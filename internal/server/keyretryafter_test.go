package server

import (
	"strconv"
	"strings"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/auth"
)

// Key RPM/TPM 429s carry a bucket-derived Retry-After (Kong writes
// Retry-After on its own 429s); daily quota keeps the midnight hint.

func TestKey429RetryAfterRPM(t *testing.T) {
	h, _, k := authSetup(t, func(k *auth.Key) { k.RPM = 1 })
	if rec := postChat(t, h, k.Key, "auto"); rec.Code != 200 {
		t.Fatalf("first: %d", rec.Code)
	}
	rec := postChat(t, h, k.Key, "auto")
	if rec.Code != 429 {
		t.Fatalf("status %d, want 429", rec.Code)
	}
	ra, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || ra <= 0 || ra > 60 {
		t.Fatalf("Retry-After = %q, want 1..60", rec.Header().Get("Retry-After"))
	}
	if rr := rec.Header().Get("RateLimit-Reset"); rr != rec.Header().Get("Retry-After") {
		t.Fatalf("RateLimit-Reset = %q, want = Retry-After %q", rr, rec.Header().Get("Retry-After"))
	}
}

func TestKey429RetryAfterTPM(t *testing.T) {
	h, _, k := authSetup(t, func(k *auth.Key) { k.TPM = 1 }) // 1 token/min
	_ = postChat(t, h, k.Key, "auto")                        // first request: bucket starts with 1 token, post-request deduct drains it
	rec := postChat(t, h, k.Key, "auto")
	if rec.Code != 429 {
		t.Fatalf("status %d, want 429 (TPM=1 exhausted)", rec.Code)
	}
	ra, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || ra <= 0 || ra > 60 {
		t.Fatalf("Retry-After = %q, want 1..60", rec.Header().Get("Retry-After"))
	}
	if !strings.Contains(rec.Body.String(), "rate_limit_exceeded") {
		t.Fatalf("body %q", rec.Body.String())
	}
}

func TestDailyQuotaRetryAfterUnchanged(t *testing.T) {
	h, store, k := authSetup(t, func(k *auth.Key) { k.DailyQuota = 1 })
	if rec := postChat(t, h, k.Key, "auto"); rec.Code != 200 {
		t.Fatalf("first: %d: %s", rec.Code, rec.Body.String())
	}
	_ = store
	rec := postChat(t, h, k.Key, "auto")
	if rec.Code != 429 {
		t.Fatalf("status %d, want 429", rec.Code)
	}
	ra, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || ra <= 0 || ra > 86400 {
		t.Fatalf("daily Retry-After = %q, want seconds-until-midnight (1..86400)", rec.Header().Get("Retry-After"))
	}
	if !strings.Contains(rec.Body.String(), "daily_quota_exceeded") {
		t.Fatalf("body %q", rec.Body.String())
	}
}
