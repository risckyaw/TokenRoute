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
