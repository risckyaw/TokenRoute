package router

import (
	"errors"
	"testing"
	"time"
)

func TestClassifyFailureQuotaVsRateLimit(t *testing.T) {
	f := ClassifyFailure(429, `{"error":"rate limit reached"}`, nil)
	if f.Kind != FailureRateLimit || !f.Retryable {
		t.Fatalf("plain 429 = %v retryable=%v, want rate_limit retryable", f.Kind, f.Retryable)
	}
	f = ClassifyFailure(429, `{"error":{"message":"You exceeded your current quota, insufficient balance"}}`, nil)
	if f.Kind != FailureQuotaExhausted || f.Retryable {
		t.Fatalf("quota 429 = %v retryable=%v, want quota_exhausted non-retryable", f.Kind, f.Retryable)
	}
}

func TestClassifyFailureAuth(t *testing.T) {
	if f := ClassifyFailure(401, `invalid api key`, nil); f.Kind != FailureAuth {
		t.Fatalf("401 = %v, want auth", f.Kind)
	}
	if f := ClassifyFailure(403, `permission denied for model`, nil); f.Kind != FailurePermission {
		t.Fatalf("403 permission = %v, want permission", f.Kind)
	}
}

func TestClassifyFailureClientAbortNotProvider(t *testing.T) {
	f := ClassifyFailure(0, "", errors.New("context canceled"))
	if f.Retryable || f.Kind != FailureUnknown {
		t.Fatalf("client abort = %v retryable=%v, want unknown non-retryable", f.Kind, f.Retryable)
	}
	f = ClassifyFailure(0, "", errors.New("read tcp: connection reset by peer"))
	if f.Kind != FailureUnknown {
		t.Fatalf("client reset counted as %v", f.Kind)
	}
}

func TestCircuitDegradedBand(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{FailureThreshold: 5, CooldownMs: 30000})
	for i := 0; i < 3; i++ { // degradation = 60% of 5 = 3
		cb.OnFailure()
	}
	if cb.State() != "degraded" {
		t.Fatalf("state at 3/5 = %s, want degraded", cb.State())
	}
	if !cb.Allow() {
		t.Fatal("degraded circuit must still allow traffic")
	}
	cb.OnFailure()
	cb.OnFailure()
	if cb.State() != "open" {
		t.Fatalf("state at 5/5 = %s, want open", cb.State())
	}
}

func TestCircuitEscalatingBackoff(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{FailureThreshold: 3, CooldownMs: 30000, AutoDisableAfter: 1000})
	now := time.Now()
	cb.now = func() time.Time { return now }
	// Cycle: open (3 failures) -> cooldown -> probe -> probe fails (openCycles++).
	for cycle := 1; cycle <= 4; cycle++ {
		for i := 0; i < 3; i++ {
			cb.OnFailure()
		}
		cb.now = func() time.Time { return now.Add(time.Duration(cycle) * time.Hour) }
		cb.Allow() // half-open probe
		cb.OnFailure()
		cb.now = func() time.Time { return now }
	}
	// openCycles == 4 > escalateAfter (3) -> 2^1 = 2x base cooldown.
	if cd := cb.effectiveCooldown(); cd != 60*time.Second {
		t.Fatalf("effectiveCooldown after 4 cycles = %v (openCycles=%d state=%s), want 60s (2x)", cd, cb.openCycles, cb.State())
	}
}

func TestCircuitAuthOpensImmediately(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{FailureThreshold: 5, CooldownMs: 30000})
	cb.OnFailureKind(FailureAuth, true)
	if cb.State() != "open" {
		t.Fatalf("auth failure = %s, want immediate open", cb.State())
	}
}

func TestCircuitClientAbortIgnored(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{FailureThreshold: 3, CooldownMs: 30000})
	for i := 0; i < 10; i++ {
		cb.OnFailureKind(FailureUnknown, false) // filtered client abort
	}
	if cb.State() != "closed" {
		t.Fatalf("client aborts tripped breaker: %s", cb.State())
	}
}
