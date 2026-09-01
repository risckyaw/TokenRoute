package router

import (
	"testing"
	"time"
)

func TestStrategyLKGP(t *testing.T) {
	a, b, c, provs := threeProviders()
	rt := &Route{Model: "m", Strategy: "lkgp", Candidates: []Candidate{
		{Provider: a, Model: "am"}, {Provider: b, Model: "bm"}, {Provider: c, Model: "cm"},
	}}
	r := New(provs, []*Route{rt})

	// Initial: priority order.
	got := names(r.OrderCandidates(rt))
	if got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("initial order %v, want [a b c]", got)
	}
	// Success on b: next order starts with b, rest in priority order.
	r.RecordResult("b", time.Millisecond, true)
	got = names(r.OrderCandidates(rt))
	if got[0] != "b" || got[1] != "a" || got[2] != "c" {
		t.Fatalf("after b success: %v, want [b a c]", got)
	}
	// b fails: lastGood cleared, back to priority.
	r.RecordResult("b", time.Millisecond, false)
	got = names(r.OrderCandidates(rt))
	if got[0] != "a" {
		t.Fatalf("after b failure: %v, want [a b c]", got)
	}
	// Failure of a non-lastGood provider does not clear lastGood.
	r.RecordResult("c", time.Millisecond, true)
	r.RecordResult("a", time.Millisecond, false)
	got = names(r.OrderCandidates(rt))
	if got[0] != "c" {
		t.Fatalf("lastGood c cleared by a failure: %v", got)
	}
}

func TestModelLockout(t *testing.T) {
	a, b, _, provs := threeProviders()
	rt := &Route{Model: "m", Candidates: []Candidate{
		{Provider: a, Model: "x"}, {Provider: a, Model: "y"}, {Provider: b, Model: "x"},
	}}
	r := New(provs, []*Route{rt})

	r.LockModel("a", "x", time.Hour)
	got := r.OrderCandidates(rt)
	// a|x locked; a|y still served (same provider, other model).
	if len(got) != 2 || got[0].Model != "y" || got[1].Provider.Name() != "b" {
		t.Fatalf("got %v, want a|y then b|x", names(got))
	}
	if !r.IsModelLocked("a", "x") || r.IsModelLocked("a", "y") || r.IsModelLocked("b", "x") {
		t.Fatal("IsModelLocked mismatch")
	}
	// Expired lock is lifted.
	r.LockModel("a", "y", -time.Second)
	if r.IsModelLocked("a", "y") {
		t.Fatal("expired lock still held")
	}
	got = r.OrderCandidates(rt)
	if len(got) != 2 || got[0].Model != "y" {
		t.Fatalf("after expiry: got %v", names(got))
	}
}

func TestCircuitOpenFor(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{FailureThreshold: 1, CooldownMs: 10})
	cb.OpenFor(120 * time.Second)
	if cb.State() != "open" {
		t.Fatalf("state %s, want open", cb.State())
	}
	if cb.Allow() {
		t.Fatal("open circuit allowed request")
	}
	rem := time.Until(cb.OpenUntil())
	if rem < 60*time.Second {
		t.Fatalf("remaining %v, want >60s", rem)
	}
	// Plain failure after expiry uses the configured (short) cooldown.
	cb.now = func() time.Time { return time.Now().Add(121 * time.Second) }
	if !cb.Allow() {
		t.Fatal("half-open probe not allowed after custom cooldown")
	}
	cb.OnFailure()
	if rem := cb.OpenUntil().Sub(cb.now()); rem > time.Second {
		t.Fatalf("after failure cooldown %v, want ~10ms", rem)
	}
}
