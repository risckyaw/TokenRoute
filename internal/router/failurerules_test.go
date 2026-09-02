package router

import (
	"testing"
	"time"
)

func TestFailureRulesUnsetIsNil(t *testing.T) {
	fr, err := NewFailureRules(nil)
	if err != nil || fr != nil {
		t.Fatalf("unset rules = %v, %v; want nil, nil (legacy behavior)", fr, err)
	}
	// A nil rule set never prescribes a cooldown.
	if _, ok := fr.Cooldown("p", 429, "rate limit exceeded"); ok {
		t.Fatal("nil rules must not match")
	}
	fr.OnSuccess("p") // nil-safe
	if fr.BackoffLevel("p") != 0 {
		t.Fatal("nil rules must report level 0")
	}
}

// Text rules are evaluated before status rules, and in config order.
func TestFailureRulesTextFirstThenStatus(t *testing.T) {
	fr, err := NewFailureRules([]FailureRule{
		{Match: "overloaded", CooldownMs: 4000},
		{Match: "rate limit", CooldownMs: 9000},
		{Status: 429, CooldownMs: 1000},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A 429 whose body says "overloaded" hits the first text rule, not the
	// status rule that also matches.
	if d, ok := fr.Cooldown("p", 429, "Model is Overloaded, try later"); !ok || d != 4*time.Second {
		t.Fatalf("got %v ok=%v, want 4s from the first text rule", d, ok)
	}
	// Config order decides between two matching text rules.
	if d, ok := fr.Cooldown("p", 500, "rate limit hit"); !ok || d != 9*time.Second {
		t.Fatalf("got %v ok=%v, want 9s", d, ok)
	}
	// No text match: the status rule fires.
	if d, ok := fr.Cooldown("p", 429, "something else"); !ok || d != time.Second {
		t.Fatalf("got %v ok=%v, want 1s from the status rule", d, ok)
	}
	// Neither: no override.
	if _, ok := fr.Cooldown("p", 503, "gateway down"); ok {
		t.Fatal("unmatched failure must not prescribe a cooldown")
	}
}

// backoff: true escalates 2s, 4s, 8s... per provider, caps at 5min, and resets
// on that provider's next success.
func TestFailureRulesBackoffEscalatesAndResets(t *testing.T) {
	fr, err := NewFailureRules([]FailureRule{{Match: "boom", Backoff: true}})
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	for i, w := range want {
		d, ok := fr.Cooldown("p", 500, "boom")
		if !ok || d != w {
			t.Fatalf("hit %d: got %v ok=%v, want %v", i+1, d, ok, w)
		}
	}
	// Escalation is per provider: a fresh provider starts at the base.
	if d, _ := fr.Cooldown("other", 500, "boom"); d != 2*time.Second {
		t.Fatalf("other provider = %v, want 2s", d)
	}
	// Cap at 5 minutes no matter how many failures pile up.
	for i := 0; i < 20; i++ {
		fr.Cooldown("p", 500, "boom")
	}
	if d, _ := fr.Cooldown("p", 500, "boom"); d != backoffCap {
		t.Fatalf("capped = %v, want %v", d, backoffCap)
	}
	// Success resets that provider only.
	fr.OnSuccess("p")
	if d, _ := fr.Cooldown("p", 500, "boom"); d != 2*time.Second {
		t.Fatalf("after success = %v, want 2s", d)
	}
	if lvl := fr.BackoffLevel("other"); lvl != 1 {
		t.Fatalf("other level = %d, want 1 (untouched by p's success)", lvl)
	}
}

// RecordResult on a success clears the router-held backoff level.
func TestRouterResetsBackoffOnSuccess(t *testing.T) {
	fr, err := NewFailureRules([]FailureRule{{Status: 500, Backoff: true}})
	if err != nil {
		t.Fatal(err)
	}
	r := New(nil, nil)
	r.SetFailureRules(fr)
	if d, ok := r.FailureCooldown("p", 500, ""); !ok || d != 2*time.Second {
		t.Fatalf("first = %v ok=%v", d, ok)
	}
	if d, _ := r.FailureCooldown("p", 500, ""); d != 4*time.Second {
		t.Fatalf("second = %v, want 4s", d)
	}
	r.RecordResult("p", time.Millisecond, true)
	if d, _ := r.FailureCooldown("p", 500, ""); d != 2*time.Second {
		t.Fatalf("after success = %v, want 2s", d)
	}
	// A router with no rules never prescribes anything.
	if _, ok := New(nil, nil).FailureCooldown("p", 500, "boom"); ok {
		t.Fatal("unconfigured router must not prescribe cooldowns")
	}
}

func TestFailureRulesInvalid(t *testing.T) {
	cases := []struct {
		name string
		rule FailureRule
	}{
		{"no selector", FailureRule{CooldownMs: 1000}},
		{"both selectors", FailureRule{Match: "x", Status: 429, CooldownMs: 1000}},
		{"no cooldown", FailureRule{Match: "x"}},
		{"cooldown and backoff", FailureRule{Match: "x", CooldownMs: 1000, Backoff: true}},
		{"bad status", FailureRule{Status: 42, CooldownMs: 1000}},
		{"negative cooldown", FailureRule{Status: 429, CooldownMs: -1}},
		{"blank match", FailureRule{Match: "   ", CooldownMs: 1000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewFailureRules([]FailureRule{tc.rule}); err == nil {
				t.Fatalf("rule %+v must be rejected", tc.rule)
			}
		})
	}
}
