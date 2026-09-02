package router

import (
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
)

func TestInflightIncDec(t *testing.T) {
	r := New(nil, nil)
	r.IncInflight("p")
	r.IncInflight("p")
	if n := r.Inflight("p"); n != 2 {
		t.Fatalf("inflight = %d, want 2", n)
	}
	r.DecInflight("p")
	if n := r.Inflight("p"); n != 1 {
		t.Fatalf("inflight = %d, want 1", n)
	}
	// Floor at 0 (double-dec safe).
	r.DecInflight("p")
	r.DecInflight("p")
	if n := r.Inflight("p"); n != 0 {
		t.Fatalf("inflight = %d, want 0 (floored)", n)
	}
}

func TestLeastConnOrdering(t *testing.T) {
	a, b, c, provs := threeProviders()
	rt := &Route{Model: "m", Strategy: "least_connections", Candidates: []Candidate{
		{Provider: a, Model: "am"}, {Provider: b, Model: "bm"}, {Provider: c, Model: "cm"},
	}}
	r := New(provs, []*Route{rt})
	// All zero -> priority order (a first).
	if got := names(r.OrderCandidates(rt)); got[0] != "a" {
		t.Fatalf("cold: first = %v, want a", got)
	}
	// a busy -> b (next priority, still idle) leads.
	r.IncInflight("a")
	r.IncInflight("a")
	if got := names(r.OrderCandidates(rt)); got[0] != "b" {
		t.Fatalf("first = %v, want b", got)
	}
	// b busier than c -> c leads.
	r.IncInflight("b")
	r.IncInflight("b")
	r.IncInflight("b")
	if got := names(r.OrderCandidates(rt)); got[0] != "c" {
		t.Fatalf("first = %v, want c", got)
	}
	// All equal -> priority order again.
	r.IncInflight("c")
	r.IncInflight("c")
	if got := names(r.OrderCandidates(rt)); got[0] != "a" {
		t.Fatalf("equal: first = %v, want a (priority tie-break)", got)
	}
}

func TestLeastConnWeights(t *testing.T) {
	small := &fakeProvider{name: "small", priority: 1} // weight 1
	big := &fakeProvider{name: "big", priority: 2}     // weight 4
	rt := &Route{Model: "m", Strategy: "least_connections", Candidates: []Candidate{
		{Provider: small, Model: "m"}, {Provider: big, Model: "m", Weight: 4},
	}}
	r := New([]provider.Provider{small, big}, []*Route{rt})
	// big has 1 inflight: (1+1)/4 = 0.5 vs small (0+1)/1 = 1 -> big leads.
	r.IncInflight("big")
	if got := names(r.OrderCandidates(rt)); got[0] != "big" {
		t.Fatalf("first = %v, want big (0.5 < 1)", got)
	}
	// big at 3: (3+1)/4 = 1 ties small at 1 -> priority: small first.
	r.IncInflight("big")
	r.IncInflight("big")
	if got := names(r.OrderCandidates(rt)); got[0] != "small" {
		t.Fatalf("first = %v, want small (tie -> priority)", got)
	}
}

func TestAutoUsesInflight(t *testing.T) {
	a := &fakeProvider{name: "a", priority: 1}
	b := &fakeProvider{name: "b", priority: 2}
	rt := &Route{Model: "m", Strategy: "auto", Candidates: []Candidate{
		{Provider: a, Model: "m"}, {Provider: b, Model: "m"},
	}}
	r := New([]provider.Provider{a, b}, []*Route{rt})
	// Identical signals otherwise: the loaded provider must rank second.
	r.IncInflight("a")
	r.IncInflight("a")
	r.IncInflight("a")
	if got := names(r.OrderCandidates(rt)); got[0] != "b" {
		t.Fatalf("first = %v, want b (a loaded)", got)
	}
}
