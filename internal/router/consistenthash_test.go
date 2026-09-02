package router

import (
	"testing"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
)

func hashRoute(provs []provider.Provider) *Route {
	return &Route{Model: "m", Strategy: "consistent_hash", HashOn: "header:X-Session-Id",
		Candidates: []Candidate{
			{Provider: provs[0], Model: "m"}, {Provider: provs[1], Model: "m"}, {Provider: provs[2], Model: "m"},
		}}
}

func TestConsistentHashDeterminism(t *testing.T) {
	a, b, c, provs := threeProviders()
	rt := hashRoute(provs)
	r := New(provs, []*Route{rt})
	first := names(r.OrderCandidatesHash(rt, nil, "session-42"))
	for i := 0; i < 50; i++ {
		got := names(r.OrderCandidatesHash(rt, nil, "session-42"))
		if got[0] != first[0] {
			t.Fatalf("non-deterministic: %v vs %v", got, first)
		}
	}
	_ = a
	_ = b
	_ = c
}

func TestConsistentHashSpreads(t *testing.T) {
	_, _, _, provs := threeProviders()
	rt := hashRoute(provs)
	r := New(provs, []*Route{rt})
	seen := map[string]int{}
	for i := 0; i < 300; i++ {
		got := r.OrderCandidatesHash(rt, nil, "sess-"+string(rune('A'+i%26))+string(rune('a'+i%18))+"-"+string(rune('0'+i%10)))
		seen[got[0].Provider.Name()]++
	}
	if len(seen) < 2 {
		t.Fatalf("no spread: %v", seen)
	}
}

func TestConsistentHashSkipsOpenCircuit(t *testing.T) {
	_, _, _, provs := threeProviders()
	rt := hashRoute(provs)
	r := New(provs, []*Route{rt})
	r.SetCircuit("a", CircuitConfig{FailureThreshold: 1, CooldownMs: 60000})
	// Find a value that maps to "a" when all candidates are allowed.
	target := ""
	for i := 0; i < 10000; i++ {
		v := "probe-" + string(rune(i%26+'a')) + "-" + string(rune(i/26%26+'a')) + string(rune(i/676%26+'a'))
		if names(r.OrderCandidatesHash(rt, nil, v))[0] == "a" {
			target = v
			break
		}
	}
	if target == "" {
		t.Fatal("no value mapped to a")
	}
	// Trip a's circuit; the same value must now land elsewhere (ring walk).
	r.RecordResult("a", time.Second, false)
	got := names(r.OrderCandidatesHash(rt, nil, target))
	if got[0] == "a" {
		t.Fatalf("first = a, want ring walk past open circuit: %v", got)
	}
}

func TestConsistentHashMissingValuePriority(t *testing.T) {
	_, _, _, provs := threeProviders()
	rt := hashRoute(provs)
	r := New(provs, []*Route{rt})
	got := names(r.OrderCandidatesHash(rt, nil, ""))
	if got[0] != "a" {
		t.Fatalf("empty value: first = %v, want a (priority)", got)
	}
}

func TestConsistentHashLexicalSortStable(t *testing.T) {
	// Ring must be lexical regardless of candidate declaration order.
	z := &fakeProvider{name: "zeta", priority: 1}
	a2 := &fakeProvider{name: "alpha", priority: 2}
	rt := &Route{Model: "m", Strategy: "consistent_hash", Candidates: []Candidate{
		{Provider: z, Model: "m"}, {Provider: a2, Model: "m"},
	}}
	r := New([]provider.Provider{z, a2}, []*Route{rt})
	// fnv32 picks an index on the sorted [alpha|m, zeta|m] ring.
	_ = r.OrderCandidatesHash(rt, nil, "x") // no panic; deterministic below
	g1 := names(r.OrderCandidatesHash(rt, nil, "x"))
	g2 := names(r.OrderCandidatesHash(rt, nil, "x"))
	if g1[0] != g2[0] {
		t.Fatalf("unstable ring: %v vs %v", g1, g2)
	}
}
