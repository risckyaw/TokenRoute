package router

import (
	"testing"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

func TestQuotaLedgerWindowRollover(t *testing.T) {
	l := NewQuotaLedger()
	now := time.Now()
	l.now = func() time.Time { return now }
	l.SetLimit("p", "m", 1000, time.Minute)
	l.Record("p", "m", 400)
	rem, ratio, known := l.Remaining("p", "m")
	if !known || rem != 600 || ratio < 0.59 || ratio > 0.61 {
		t.Fatalf("remaining=%d ratio=%v known=%v, want 600/0.6/true", rem, ratio, known)
	}
	// Window elapsed: reset to full.
	l.now = func() time.Time { return now.Add(61 * time.Second) }
	rem, _, _ = l.Remaining("p", "m")
	if rem != 1000 {
		t.Fatalf("remaining after rollover = %d, want 1000", rem)
	}
}

func TestQuotaLedgerFailOpen(t *testing.T) {
	l := NewQuotaLedger()
	if !l.Affordable("unknown", "m", 1<<60) {
		t.Fatal("untracked provider must be affordable (fail-open)")
	}
	if _, _, known := l.Remaining("unknown", "m"); known {
		t.Fatal("untracked provider must report known=false")
	}
}

func TestFillFirstSinksExhausted(t *testing.T) {
	r := New(nil, nil)
	r.quota.SetLimit("a", "m", 100, time.Minute)
	r.quota.SetLimit("b", "m", 100, time.Minute)
	r.quota.Record("a", "m", 100) // exhaust a
	rt := &Route{Model: "x", Strategy: StrategyFillFirst, Candidates: []Candidate{
		{Provider: &fakeProvider{name: "a", priority: 1}, Model: "m"},
		{Provider: &fakeProvider{name: "b", priority: 2}, Model: "m"},
	}}
	got := r.OrderCandidates(rt)
	if got[0].Provider.Name() != "b" {
		t.Fatalf("fill_first first = %s, want b (a exhausted)", got[0].Provider.Name())
	}
}

func TestResetAwarePrefersSoonestReset(t *testing.T) {
	r := New(nil, nil)
	r.quota.SetLimit("a", "m", 100, time.Hour)
	r.quota.SetLimit("b", "m", 100, time.Minute) // resets sooner
	rt := &Route{Model: "x", Strategy: StrategyResetAware, Candidates: []Candidate{
		{Provider: &fakeProvider{name: "a", priority: 1}, Model: "m"},
		{Provider: &fakeProvider{name: "b", priority: 2}, Model: "m"},
	}}
	got := r.OrderCandidates(rt)
	if got[0].Provider.Name() != "b" {
		t.Fatalf("reset_aware first = %s, want b (minute window)", got[0].Provider.Name())
	}
}

func TestP2CPicksLeastLoadedOfTwo(t *testing.T) {
	r := New(nil, nil)
	// Preload b with heavy window traffic so a always wins the p2c draw.
	for i := 0; i < 50; i++ {
		r.RecordResult("b", time.Millisecond, true)
	}
	rt := &Route{Model: "x", Strategy: StrategyP2C, Candidates: []Candidate{
		{Provider: &fakeProvider{name: "a", priority: 1}, Model: "m"},
		{Provider: &fakeProvider{name: "b", priority: 2}, Model: "m"},
	}}
	for i := 0; i < 20; i++ {
		got := r.OrderCandidates(rt)
		if got[0].Provider.Name() != "a" {
			t.Fatalf("p2c first = %s, want a (least loaded)", got[0].Provider.Name())
		}
	}
}

func TestAutoPrefersCheaperHealthy(t *testing.T) {
	r := New(nil, nil)
	r.SetPrices(map[string]usage.Price{
		"cheap":     {PromptPer1M: 0.1, CompletionPer1M: 0.1},
		"expensive": {PromptPer1M: 9, CompletionPer1M: 9},
	})
	// Poison b's health so the expensive AND unhealthy candidate clearly loses.
	for i := 0; i < 10; i++ {
		r.RecordResult("b", time.Millisecond, false)
	}
	rt := &Route{Model: "x", Strategy: StrategyAuto, Candidates: []Candidate{
		{Provider: &fakeProvider{name: "a", priority: 1}, Model: "expensive"},
		{Provider: &fakeProvider{name: "b", priority: 2}, Model: "cheap"},
	}}
	got := r.OrderCandidates(rt)
	if got[0].Provider.Name() != "b" {
		t.Fatalf("auto first = %s, want b (cheap beats unhealthy-expensive ordering)", got[0].Provider.Name())
	}
}
