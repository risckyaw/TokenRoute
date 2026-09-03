package router

import (
	"testing"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

func names(cands []Candidate) []string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.Provider.Name()
	}
	return out
}

func threeProviders() (*fakeProvider, *fakeProvider, *fakeProvider, []provider.Provider) {
	a := &fakeProvider{name: "a", priority: 1}
	b := &fakeProvider{name: "b", priority: 5}
	c := &fakeProvider{name: "c", priority: 10}
	return a, b, c, []provider.Provider{a, b, c}
}

func TestStrategyPriority(t *testing.T) {
	a, b, c, provs := threeProviders()
	rt := &Route{Model: "m", Strategy: "priority", Candidates: []Candidate{
		{Provider: c, Model: "cm"}, {Provider: a, Model: "am"}, {Provider: b, Model: "bm"},
	}}
	r := New(provs, []*Route{rt})
	got := names(r.OrderCandidates(rt))
	want := []string{"a", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestStrategyRoundRobinRotates(t *testing.T) {
	a, b, c, provs := threeProviders()
	rt := &Route{Model: "m", Strategy: "round_robin", Candidates: []Candidate{
		{Provider: a, Model: "am"}, {Provider: b, Model: "bm"}, {Provider: c, Model: "cm"},
	}}
	r := New(provs, []*Route{rt})
	firsts := map[string]int{}
	for i := 0; i < 6; i++ {
		got := r.OrderCandidates(rt)
		firsts[got[0].Provider.Name()]++
	}
	for _, n := range []string{"a", "b", "c"} {
		if firsts[n] != 2 {
			t.Fatalf("provider %s first %d times, want 2 (%v)", n, firsts[n], firsts)
		}
	}
}

func TestStrategyLeastLatency(t *testing.T) {
	a, b, c, provs := threeProviders()
	rt := &Route{Model: "m", Strategy: "least_latency", Candidates: []Candidate{
		{Provider: a, Model: "am"}, {Provider: b, Model: "bm"}, {Provider: c, Model: "cm"},
	}}
	r := New(provs, []*Route{rt})
	// Unseen providers (EMA=0) stay in priority order first.
	got := names(r.OrderCandidates(rt))
	if got[0] != "a" {
		t.Fatalf("unseen: first = %v, want a", got)
	}
	// a slow, b fast, c unseen (slow-start: seeded with mean 255 -> middle).
	r.RecordResult("a", 500*time.Millisecond, true)
	r.RecordResult("b", 10*time.Millisecond, true)
	got = names(r.OrderCandidates(rt))
	want := []string{"b", "c", "a"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	// EMA moves toward new samples: make c very slow.
	r.RecordResult("c", 900*time.Millisecond, true)
	got = names(r.OrderCandidates(rt))
	if got[0] != "b" {
		t.Fatalf("after c slow: got %v, want b first", got)
	}
}

func TestStrategyWeighted(t *testing.T) {
	a, b, c, provs := threeProviders()
	rt := &Route{Model: "m", Strategy: "weighted", Candidates: []Candidate{
		{Provider: a, Model: "am", Weight: 80},
		{Provider: b, Model: "bm", Weight: 20},
		{Provider: c, Model: "cm", Weight: 0}, // defaults to 1
	}}
	r := New(provs, []*Route{rt})
	counts := map[string]int{}
	n := 10000
	for i := 0; i < n; i++ {
		got := r.OrderCandidates(rt)
		counts[got[0].Provider.Name()]++
		// Remaining candidates must keep priority order after the pick.
		for i := 1; i < len(got)-1; i++ {
			if got[i].Provider.Priority() > got[i+1].Provider.Priority() {
				t.Fatalf("tail not in priority order: %v", names(got))
			}
		}
	}
	if counts["a"] < 7000 || counts["a"] > 9000 {
		t.Fatalf("a picked %d/%d times, want ~79%%", counts["a"], n)
	}
	if counts["b"] < 1000 || counts["b"] > 3000 {
		t.Fatalf("b picked %d/%d times, want ~20%%", counts["b"], n)
	}
	if counts["c"] > 300 {
		t.Fatalf("c (weight 1) picked %d/%d times, want ~1%%", counts["c"], n)
	}
}

func TestStrategyCost(t *testing.T) {
	a, b, c, provs := threeProviders()
	rt := &Route{Model: "m", Strategy: "cost", Candidates: []Candidate{
		{Provider: a, Model: "expensive"}, {Provider: b, Model: "unknown"}, {Provider: c, Model: "cheap"},
	}}
	r := New(provs, []*Route{rt})
	r.SetPrices(usage.NewPriceStore(map[string]usage.Price{
		"expensive": {PromptPer1M: 10, CompletionPer1M: 30},
		"cheap":     {PromptPer1M: 0.1, CompletionPer1M: 0.2},
	}))
	got := names(r.OrderCandidates(rt))
	want := []string{"c", "a", "b"} // unknown price = last
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestOrderCandidatesSkipsOpenCircuit(t *testing.T) {
	a, b, _, provs := threeProviders()
	rt := &Route{Model: "m", Candidates: []Candidate{
		{Provider: a, Model: "am"}, {Provider: b, Model: "bm"},
	}}
	r := New(provs, []*Route{rt})
	r.SetCircuit("a", CircuitConfig{FailureThreshold: 1, CooldownMs: 60000})
	r.RecordResult("a", time.Millisecond, false) // trips circuit
	got := names(r.OrderCandidates(rt))
	if len(got) != 1 || got[0] != "b" {
		t.Fatalf("got %v, want [b] (a circuit open)", got)
	}
}

func TestLowestUsageOrdering(t *testing.T) {
	hi := &fakeProvider{name: "hi", priority: 1}
	mid := &fakeProvider{name: "mid", priority: 5}
	lo := &fakeProvider{name: "lo", priority: 10}
	rt := &Route{Model: "m", Strategy: StrategyLowestUsage, Candidates: []Candidate{
		{Provider: hi, Model: "a"}, {Provider: mid, Model: "b"}, {Provider: lo, Model: "c"},
	}}
	r := New([]provider.Provider{hi, mid, lo}, []*Route{rt})

	// Unseen candidates first (all zero): priority order hi, mid, lo.
	got := r.OrderCandidates(rt)
	if got[0].Provider.Name() != "hi" || got[1].Provider.Name() != "mid" || got[2].Provider.Name() != "lo" {
		t.Fatalf("unseen order: %v %v %v", got[0].Provider.Name(), got[1].Provider.Name(), got[2].Provider.Name())
	}
	// Load hi with tokens: it sinks behind the zero-usage others.
	r.RecordTokens("hi", 1000)
	got = r.OrderCandidates(rt)
	if got[0].Provider.Name() == "hi" {
		t.Fatalf("loaded provider must sink: %v", got[0].Provider.Name())
	}
	if got[2].Provider.Name() != "hi" {
		t.Fatalf("hi must be last: %v %v %v", got[0].Provider.Name(), got[1].Provider.Name(), got[2].Provider.Name())
	}
	// Tie between mid and lo (both 0): priority wins (mid before lo).
	if got[0].Provider.Name() != "mid" || got[1].Provider.Name() != "lo" {
		t.Fatalf("tie must keep priority: %v %v", got[0].Provider.Name(), got[1].Provider.Name())
	}
	// mid gets fewer tokens than lo: mid first, lo second, hi last.
	r.RecordTokens("mid", 100)
	r.RecordTokens("lo", 500)
	got = r.OrderCandidates(rt)
	want := []string{"mid", "lo", "hi"}
	for i, name := range want {
		if got[i].Provider.Name() != name {
			t.Fatalf("order[%d] = %s, want %s", i, got[i].Provider.Name(), name)
		}
	}
}

func TestLowestUsageWindowRollover(t *testing.T) {
	hi := &fakeProvider{name: "hi", priority: 1}
	lo := &fakeProvider{name: "lo", priority: 10}
	rt := &Route{Model: "m", Strategy: StrategyLowestUsage, Candidates: []Candidate{
		{Provider: hi, Model: "a"}, {Provider: lo, Model: "b"},
	}}
	r := New([]provider.Provider{hi, lo}, []*Route{rt})
	r.RecordTokens("hi", 5000)
	// Simulate minute rollover by resetting the window start.
	r.latMu.Lock()
	r.windows["hi"].start = time.Now().Add(-61 * time.Second)
	r.latMu.Unlock()
	// Next RecordResult rolls the window (toks reset); hi becomes unseen-fresh.
	r.RecordResult("hi", time.Millisecond, true)
	got := r.OrderCandidates(rt)
	if got[0].Provider.Name() != "hi" {
		t.Fatalf("after rollover hi must be first (0 toks): %v", got[0].Provider.Name())
	}
}

func TestValidStrategyLowestUsage(t *testing.T) {
	if !ValidStrategy(StrategyLowestUsage) {
		t.Fatal("lowest_usage must be a valid strategy")
	}
}
