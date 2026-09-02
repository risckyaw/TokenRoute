package router

import (
	"testing"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
)

func TestObserveUpstreamFreshWins(t *testing.T) {
	l := NewQuotaLedger()
	l.SetLimit("p", "m", 1000, time.Minute)
	l.Record("p", "m", 900) // local says 100 left
	l.ObserveUpstream("p", "m", 500, 30*time.Second)
	rem, ratio, known := l.Remaining("p", "m")
	if !known || rem != 500 {
		t.Fatalf("remaining = %d known=%v, want 500 true (observed wins over local)", rem, known)
	}
	if ratio != 0.5 {
		t.Fatalf("ratio = %v, want 0.5 (observed/configured limit)", ratio)
	}
	if reset := l.WindowReset("p", "m"); reset.Before(time.Now().Add(29*time.Second)) || reset.After(time.Now().Add(31*time.Second)) {
		t.Fatalf("reset = %v, want ~30s from now", reset)
	}
}

func TestObserveUpstreamStaleIgnored(t *testing.T) {
	l := NewQuotaLedger()
	l.now = func() time.Time { return baseT }
	l.SetLimit("p", "m", 1000, time.Minute)
	l.ObserveUpstream("p", "m", 500, 30*time.Second)
	l.now = func() time.Time { return baseT.Add(61 * time.Second) } // past freshness window
	l.Record("p", "m", 900)
	rem, _, known := l.Remaining("p", "m")
	if !known || rem != 100 {
		t.Fatalf("remaining = %d, want 100 (stale observation ignored, local used)", rem)
	}
}

var baseT = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestObserveUpstreamNoLimit(t *testing.T) {
	l := NewQuotaLedger()
	l.ObserveUpstream("p", "m", 42, 10*time.Second)
	rem, ratio, known := l.Remaining("p", "m")
	if !known || rem != 42 || ratio != 1 {
		t.Fatalf("got rem=%d ratio=%v known=%v, want 42/1/true (unlimited denominator)", rem, ratio, known)
	}
	l.ObserveUpstream("p", "m", 0, 10*time.Second)
	_, ratio, _ = l.Remaining("p", "m")
	if ratio != 0 {
		t.Fatalf("ratio = %v, want 0 for exhausted observed budget", ratio)
	}
}

func TestObserveUpstreamUnknownRow(t *testing.T) {
	l := NewQuotaLedger()
	if _, _, known := l.Remaining("x", "y"); known {
		t.Fatal("unobserved row must stay unknown")
	}
}

func TestFillFirstPrefersObserved(t *testing.T) {
	p1 := &fakeProvider{name: "p1", priority: 1}
	p2 := &fakeProvider{name: "p2", priority: 2}
	rt := &Route{Model: "m", Strategy: StrategyFillFirst, Candidates: []Candidate{
		{Provider: p1, Model: "m"}, {Provider: p2, Model: "m"},
	}}
	r := New([]provider.Provider{p1, p2}, []*Route{rt})
	// p1 exhausted upstream (observed), p2 untouched: p2 should lead now.
	r.quota.ObserveUpstream("p1", "m", 0, time.Minute)
	got := r.OrderCandidates(rt)
	if got[0].Provider.Name() != "p2" {
		t.Fatalf("first = %s, want p2 (p1 observed exhausted)", got[0].Provider.Name())
	}
}
