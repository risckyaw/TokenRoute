package router

import (
	"testing"
	"time"
)

func newTestBreaker() *CircuitBreaker {
	cb := NewCircuitBreaker(CircuitConfig{FailureThreshold: 3, CooldownMs: 30000})
	now := time.Now()
	cb.now = func() time.Time { return now }
	return cb
}

func TestCircuitThresholdTrip(t *testing.T) {
	cb := newTestBreaker()
	for i := 0; i < 2; i++ {
		cb.OnFailure()
		if cb.State() != "closed" {
			t.Fatalf("state after %d failures = %s, want closed", i+1, cb.State())
		}
	}
	cb.OnFailure()
	if cb.State() != "open" {
		t.Fatalf("state after threshold = %s, want open", cb.State())
	}
	if cb.Allow() {
		t.Fatal("open circuit must not allow requests")
	}
}

func TestCircuitCooldownSkipThenHalfOpen(t *testing.T) {
	cb := newTestBreaker()
	for i := 0; i < 3; i++ {
		cb.OnFailure()
	}
	if cb.Allow() {
		t.Fatal("open circuit must not allow before cooldown")
	}
	base := cb.now()
	cb.now = func() time.Time { return base.Add(31 * time.Second) }
	if !cb.Allow() {
		t.Fatal("after cooldown circuit must allow one probe (half-open)")
	}
	if cb.State() != "half-open" {
		t.Fatalf("state = %s, want half-open", cb.State())
	}
	if cb.Allow() {
		t.Fatal("half-open must allow exactly one probe")
	}
}

func TestCircuitHalfOpenProbeFailureReopens(t *testing.T) {
	cb := newTestBreaker()
	for i := 0; i < 3; i++ {
		cb.OnFailure()
	}
	base := cb.now()
	cb.now = func() time.Time { return base.Add(31 * time.Second) }
	cb.Allow() // -> half-open
	cb.OnFailure()
	if cb.State() != "open" {
		t.Fatalf("state = %s, want open", cb.State())
	}
	// Cooldown restarts: not allowed immediately after reopen.
	if cb.Allow() {
		t.Fatal("cooldown must restart after failed probe")
	}
}

func TestCircuitRecovery(t *testing.T) {
	cb := newTestBreaker()
	for i := 0; i < 3; i++ {
		cb.OnFailure()
	}
	base := cb.now()
	cb.now = func() time.Time { return base.Add(31 * time.Second) }
	cb.Allow() // -> half-open
	cb.OnSuccess()
	if cb.State() != "closed" {
		t.Fatalf("state = %s, want closed", cb.State())
	}
	// Failure counter reset: threshold applies afresh.
	cb.OnFailure()
	if cb.State() != "closed" {
		t.Fatalf("state = %s, want closed after reset", cb.State())
	}
	if !cb.Allow() {
		t.Fatal("closed circuit must allow requests")
	}
}

func newPercentBreaker() *CircuitBreaker {
	cb := NewCircuitBreaker(CircuitConfig{Mode: "percent", FailurePercent: 0.5, MinRequests: 5, CooldownMs: 30000})
	now := time.Now()
	cb.now = func() time.Time { return now }
	return cb
}

func TestPercentTrip(t *testing.T) {
	cb := newPercentBreaker()
	// 5 failures / 5 total = 100% >= max(5, 0.5*5) -> trips at 5th failure.
	for i := 0; i < 4; i++ {
		cb.OnFailure()
		if cb.State() != "closed" {
			t.Fatalf("state after %d failures = %s, want closed", i+1, cb.State())
		}
	}
	cb.OnFailure()
	if cb.State() != "open" {
		t.Fatalf("state = %s, want open at 5/5 failures", cb.State())
	}
}

func TestPercentBelowMinimumNoTrip(t *testing.T) {
	cb := newPercentBreaker()
	// 4 failures out of 4: 100% ratio but below min_requests=5.
	for i := 0; i < 4; i++ {
		cb.OnFailure()
	}
	if cb.State() != "closed" {
		t.Fatalf("state = %s, want closed below min_requests", cb.State())
	}
	// High volume, low ratio: 6 failures in 20 total = 30% < 50% -> closed.
	cb2 := newPercentBreaker()
	for i := 0; i < 14; i++ {
		cb2.OnSuccess()
	}
	for i := 0; i < 6; i++ {
		cb2.OnFailure()
	}
	if cb2.State() != "closed" {
		t.Fatalf("state = %s, want closed at 30 percent ratio", cb2.State())
	}
}

func TestPercentMinuteRollover(t *testing.T) {
	cb := newPercentBreaker()
	for i := 0; i < 4; i++ {
		cb.OnFailure()
	}
	// Window resets after 60s: counters restart, ratio no longer met.
	base := cb.now()
	cb.now = func() time.Time { return base.Add(61 * time.Second) }
	cb.OnFailure() // 1/1 in the new window, below min_requests
	if cb.State() != "closed" {
		t.Fatalf("state = %s, want closed after minute rollover", cb.State())
	}
}

func TestPercentHalfOpenProbeFailureReopens(t *testing.T) {
	cb := newPercentBreaker()
	for i := 0; i < 5; i++ {
		cb.OnFailure()
	}
	base := cb.now()
	cb.now = func() time.Time { return base.Add(31 * time.Second) }
	cb.Allow() // -> half-open
	cb.OnFailure()
	if cb.State() != "open" {
		t.Fatalf("state = %s, want open after failed probe", cb.State())
	}
}

func TestAllowedFailsPerKind(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{
		FailureThreshold: 3, CooldownMs: 30000,
		AllowedFails: map[FailureKind]int{FailureTimeout: 5, FailureRateLimit: 1},
	})
	// timeout tolerated 5 times: 5 failures still closed, 6th opens.
	for i := 0; i < 5; i++ {
		cb.OnFailureKind(FailureTimeout, true)
		if cb.State() != "closed" {
			t.Fatalf("timeout %d: state = %s, want closed", i+1, cb.State())
		}
	}
	cb.OnFailureKind(FailureTimeout, true)
	if cb.State() != "open" {
		t.Fatalf("state = %s, want open after allowed_fails+1 timeouts", cb.State())
	}
}

func TestAllowedFailsRateLimitTight(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{
		FailureThreshold: 3, CooldownMs: 30000,
		AllowedFails: map[FailureKind]int{FailureRateLimit: 1},
	})
	cb.OnFailureKind(FailureRateLimit, true)
	if cb.State() != "closed" {
		t.Fatalf("state = %s, want closed at 1 rate_limit", cb.State())
	}
	cb.OnFailureKind(FailureRateLimit, true)
	if cb.State() != "open" {
		t.Fatalf("state = %s, want open at allowed_fails+1 rate_limits", cb.State())
	}
}

func TestAllowedFailsUnmatchedKindUsesThreshold(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{
		FailureThreshold: 3, CooldownMs: 30000,
		AllowedFails: map[FailureKind]int{FailureTimeout: 10},
	})
	// network has no entry: global threshold 3 applies.
	for i := 0; i < 2; i++ {
		cb.OnFailureKind(FailureNetwork, true)
		if cb.State() != "closed" {
			t.Fatalf("network %d: state = %s, want closed", i+1, cb.State())
		}
	}
	cb.OnFailureKind(FailureNetwork, true)
	if cb.State() != "open" {
		t.Fatalf("state = %s, want open at threshold for unmatched kind", cb.State())
	}
}

func TestAllowedFailsAuthDefaultInstantOpen(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{
		FailureThreshold: 3, CooldownMs: 30000,
		AllowedFails: map[FailureKind]int{FailureTimeout: 10},
	})
	cb.OnFailureKind(FailureAuth, true)
	if cb.State() != "open" {
		t.Fatalf("state = %s, want instant open for auth without override", cb.State())
	}
}

func TestAllowedFailsAuthOverride(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{
		FailureThreshold: 3, CooldownMs: 30000,
		AllowedFails: map[FailureKind]int{FailureAuth: 2},
	})
	cb.OnFailureKind(FailureAuth, true)
	cb.OnFailureKind(FailureAuth, true)
	if cb.State() != "closed" {
		t.Fatalf("state = %s, want closed within explicit auth budget", cb.State())
	}
	cb.OnFailureKind(FailureAuth, true)
	if cb.State() != "open" {
		t.Fatalf("state = %s, want open past explicit auth budget", cb.State())
	}
}

func TestAllowedFailsSuccessResetsKindCounters(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{
		FailureThreshold: 3, CooldownMs: 30000,
		AllowedFails: map[FailureKind]int{FailureRateLimit: 1},
	})
	cb.OnFailureKind(FailureRateLimit, true)
	cb.OnSuccess()
	cb.OnFailureKind(FailureRateLimit, true)
	if cb.State() != "closed" {
		t.Fatalf("state = %s, want closed: success resets kind counters", cb.State())
	}
}

func TestParseAllowedFails(t *testing.T) {
	got := ParseAllowedFails(map[string]int{"timeout": 10, "rate_limit": 3, "bogus": 1})
	if got[FailureTimeout] != 10 || got[FailureRateLimit] != 3 {
		t.Fatalf("ParseAllowedFails = %v", got)
	}
	if _, ok := got[FailureAuth]; ok {
		t.Fatal("bogus name must be skipped")
	}
	if ParseAllowedFails(map[string]int{"bogus": 1}) != nil {
		t.Fatal("all-unknown map must return nil (single-counter behavior)")
	}
}
