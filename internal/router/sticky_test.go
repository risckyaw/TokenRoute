package router

import (
	"testing"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
)

// stickyRoute builds a 3-candidate round_robin route with the given sticky N.
func stickyRoute(sticky int) (*Router, *Route) {
	a, b, c, provs := threeProviders()
	rt := &Route{Model: "m", Strategy: StrategyRoundRobin, Sticky: sticky, Candidates: []Candidate{
		{Provider: a, Model: "am"}, {Provider: b, Model: "bm"}, {Provider: c, Model: "cm"},
	}}
	return New(provs, []*Route{rt}), rt
}

// firstNames returns the leading provider name of n consecutive orderings.
func firstNames(r *Router, rt *Route, n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = r.OrderCandidates(rt)[0].Provider.Name()
	}
	return out
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// sticky: 3 keeps three consecutive requests on a candidate, then advances.
func TestStickyRoundRobinAdvancesAfterN(t *testing.T) {
	r, rt := stickyRoute(3)
	eq(t, firstNames(r, rt, 9), []string{"a", "a", "a", "b", "b", "b", "c", "c", "c"})
}

// The cursor wraps back to the first candidate after the last one.
func TestStickyRoundRobinWraps(t *testing.T) {
	r, rt := stickyRoute(2)
	eq(t, firstNames(r, rt, 8), []string{"a", "a", "b", "b", "c", "c", "a", "a"})
}

// sticky: 1 (and 0/unset) must behave exactly like the legacy rotation:
// advance every request.
func TestStickyOneUnchanged(t *testing.T) {
	r, rt := stickyRoute(1)
	eq(t, firstNames(r, rt, 4), []string{"a", "b", "c", "a"})

	r0, rt0 := stickyRoute(0)
	eq(t, firstNames(r0, rt0, 4), []string{"a", "b", "c", "a"})
}

// A filtered-out candidate (circuit open) is skipped forward in order without
// shifting the cursor: the sticky index tracks the ORIGINAL list.
func TestStickyRoundRobinSkipsFiltered(t *testing.T) {
	r, rt := stickyRoute(2)
	r.SetCircuit("b", CircuitConfig{FailureThreshold: 1, CooldownMs: 600000, AutoDisableAfter: 99})
	r.RecordResult("b", time.Millisecond, false) // opens b's circuit

	// Cursor: a,a (0,0) -> b,b (1,1: filtered, falls to c) -> c,c (2,2) -> a,a.
	eq(t, firstNames(r, rt, 8), []string{"a", "a", "c", "c", "c", "c", "a", "a"})
	// b never appears at all.
	for _, c := range r.OrderCandidates(rt) {
		if c.Provider.Name() == "b" {
			t.Fatal("circuit-open candidate must not be returned")
		}
	}
}

// A cursor past every survivor wraps to the first survivor.
func TestStickyRoundRobinWrapsPastFilteredTail(t *testing.T) {
	r, rt := stickyRoute(1)
	r.SetCircuit("c", CircuitConfig{FailureThreshold: 1, CooldownMs: 600000, AutoDisableAfter: 99})
	r.RecordResult("c", time.Millisecond, false)
	rt.Sticky = 2
	// Cursor 0 -> a,a; 1 -> b,b; 2 -> c filtered, no survivor at/after -> a,a.
	eq(t, firstNames(r, rt, 6), []string{"a", "a", "b", "b", "a", "a"})
}

// Sticky state is per-route and rebuilt on config reload (fresh Route value).
func TestStickyStateResetsOnFreshRoute(t *testing.T) {
	r, rt := stickyRoute(2)
	_ = firstNames(r, rt, 3) // advances to candidate b
	_, fresh := stickyRoute(2)
	if got := firstNames(New([]provider.Provider(nil), []*Route{fresh}), fresh, 1)[0]; got != "a" {
		t.Fatalf("fresh route starts at %s, want a", got)
	}
}
