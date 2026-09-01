package router

import (
	"testing"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
)

func TestHeadroom_LeastUsedFirst(t *testing.T) {
	a := &fakeProvider{name: "a", priority: 1}
	b := &fakeProvider{name: "b", priority: 2}
	rt := New([]provider.Provider{a, b}, []*Route{{
		Model:    "m",
		Strategy: StrategyHeadroom,
		Candidates: []Candidate{
			{Provider: a, Model: "ma"},
			{Provider: b, Model: "mb"},
		},
	}})

	route := rt.Resolve("m")
	// No usage yet: priority order (a first).
	if got := rt.OrderCandidates(route); got[0].Provider.Name() != "a" {
		t.Fatalf("initial first = %s, want a", got[0].Provider.Name())
	}

	// 3 results on a, 0 on b -> b first.
	for range 3 {
		rt.RecordResult("a", 10*time.Millisecond, true)
	}
	got := rt.OrderCandidates(route)
	if got[0].Provider.Name() != "b" {
		t.Fatalf("after load first = %s, want b", got[0].Provider.Name())
	}
	if got[1].Provider.Name() != "a" {
		t.Fatalf("second = %s, want a", got[1].Provider.Name())
	}

	// Equal load -> priority order again.
	rt.RecordResult("b", 10*time.Millisecond, true)
	rt.RecordResult("b", 10*time.Millisecond, true)
	rt.RecordResult("b", 10*time.Millisecond, true)
	if got := rt.OrderCandidates(route); got[0].Provider.Name() != "a" {
		t.Fatalf("tie first = %s, want a (priority tiebreak)", got[0].Provider.Name())
	}
}

func TestHeadroom_WindowReset(t *testing.T) {
	a := &fakeProvider{name: "a", priority: 1}
	b := &fakeProvider{name: "b", priority: 2}
	rt := New([]provider.Provider{a, b}, []*Route{{
		Model:    "m",
		Strategy: StrategyHeadroom,
		Candidates: []Candidate{
			{Provider: a, Model: "ma"},
			{Provider: b, Model: "mb"},
		},
	}})
	rt.RecordResult("a", 10*time.Millisecond, true)
	// Force a's window into the past: next RecordResult resets the counter.
	rt.latMu.Lock()
	rt.windows["a"].start = time.Now().Add(-61 * time.Second)
	rt.latMu.Unlock()

	rt.RecordResult("a", 10*time.Millisecond, true) // resets to 1
	rt.RecordResult("b", 10*time.Millisecond, true) // b = 1
	// Tie at 1 each -> priority: a first.
	if got := rt.OrderCandidates(rt.Resolve("m")); got[0].Provider.Name() != "a" {
		t.Fatalf("after reset first = %s, want a", got[0].Provider.Name())
	}
}
