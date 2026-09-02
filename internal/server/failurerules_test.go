package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/provider/openai"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// rulesSetup routes "auto" to a failing p1 (bad) then a healthy p2, with the
// given failure rules and a long-cooldown breaker on p1 so an override is
// visible as a SHORTER open window.
func rulesSetup(t *testing.T, bad, good string, rules []router.FailureRule) (http.Handler, *router.Router) {
	t.Helper()
	p1 := openai.New(openai.Config{Name: "p1", BaseURL: bad, Priority: 1, TimeoutMs: 5000})
	p2 := openai.New(openai.Config{Name: "p2", BaseURL: good, Priority: 2, TimeoutMs: 5000})
	rt := router.New([]provider.Provider{p1, p2}, []*router.Route{{
		Model: "auto",
		Candidates: []router.Candidate{
			{Provider: p1, Model: "m1"},
			{Provider: p2, Model: "m2"},
		},
	}})
	// Threshold 1 so one failure opens; 10min cooldown so any rule is shorter.
	rt.SetCircuit("p1", router.CircuitConfig{FailureThreshold: 1, CooldownMs: 600000, AutoDisableAfter: 99})
	fr, err := router.NewFailureRules(rules)
	if err != nil {
		t.Fatal(err)
	}
	rt.SetFailureRules(fr)
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	return New(rt, ul, nil), rt
}

// openWindow is how long p1's circuit stays open from now.
func openWindow(t *testing.T, rt *router.Router) time.Duration {
	t.Helper()
	until := rt.CircuitOpenUntil("p1")
	if until.IsZero() {
		t.Fatalf("p1 circuit not open (state %s)", rt.CircuitState("p1"))
	}
	return time.Until(until)
}

// A matching text rule replaces the breaker's configured cooldown.
func TestFailureRuleTextCooldownOverrides(t *testing.T) {
	bad := upstream(t, 503, `{"error":{"message":"Model is overloaded"}}`)
	good := upstream(t, 200, `{"id":"ok"}`)
	h, rt := rulesSetup(t, bad.URL, good.URL, []router.FailureRule{
		{Match: "overloaded", CooldownMs: 4000},
	})

	if rec := post(t, h); rec.Code != 200 {
		t.Fatalf("status %d, want failover to 200", rec.Code)
	}
	if d := openWindow(t, rt); d > 5*time.Second {
		t.Fatalf("open window %v, want ~4s from the rule (not the 10min cooldown)", d)
	}
}

// A status rule fires when no text rule matches — here on a 429, whose
// Retry-After honoring would otherwise set a 5min window.
func TestFailureRuleStatusOverridesRetryAfter(t *testing.T) {
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "300")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	t.Cleanup(limited.Close)
	good := upstream(t, 200, `{"id":"ok"}`)
	h, rt := rulesSetup(t, limited.URL, good.URL, []router.FailureRule{
		{Match: "overloaded", CooldownMs: 90000}, // must not match "slow down"
		{Status: 429, CooldownMs: 5000},
	})

	if rec := post(t, h); rec.Code != 200 {
		t.Fatalf("status %d, want failover to 200", rec.Code)
	}
	if d := openWindow(t, rt); d > 6*time.Second {
		t.Fatalf("open window %v, want ~5s from the status rule (not Retry-After 300)", d)
	}
}

// A terminal (non-retryable) status still gets its rule's cooldown, even
// though the response relays as-is with no failover.
func TestFailureRuleTerminalStatus(t *testing.T) {
	denied := upstream(t, 401, `{"error":{"message":"bad key"}}`)
	good := upstream(t, 200, `{"id":"ok"}`)
	h, rt := rulesSetup(t, denied.URL, good.URL, []router.FailureRule{
		{Status: 401, CooldownMs: 120000},
	})

	if rec := post(t, h); rec.Code != 401 {
		t.Fatalf("status %d, want the 401 relayed as-is", rec.Code)
	}
	d := openWindow(t, rt)
	if d < 100*time.Second || d > 121*time.Second {
		t.Fatalf("open window %v, want ~120s from the 401 rule", d)
	}
}

// With no failure_rules configured, cooldowns are exactly the legacy ones.
func TestFailureRulesUnsetLegacyCooldown(t *testing.T) {
	bad := upstream(t, 503, `{"error":{"message":"Model is overloaded"}}`)
	good := upstream(t, 200, `{"id":"ok"}`)
	h, rt := rulesSetup(t, bad.URL, good.URL, nil)

	if rec := post(t, h); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if d := openWindow(t, rt); d < 9*time.Minute {
		t.Fatalf("open window %v, want the configured 10min cooldown", d)
	}
	// A terminal 401 with no rules must not open the circuit at all.
	denied := upstream(t, 401, `{"error":{"message":"bad key"}}`)
	h2, rt2 := rulesSetup(t, denied.URL, good.URL, nil)
	if rec := post(t, h2); rec.Code != 401 {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	if st := rt2.CircuitState("p1"); st != "closed" {
		t.Fatalf("circuit %s after a relayed 401 with no rules, want closed", st)
	}
}

// backoff: true escalates across successive failures and resets once the
// provider answers successfully.
func TestFailureRuleBackoffEscalatesEndToEnd(t *testing.T) {
	status := 503
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	t.Cleanup(flaky.Close)
	good := upstream(t, 200, `{"id":"ok"}`)
	// Breaker with an immediate cooldown so p1 is retried each request.
	p1 := openai.New(openai.Config{Name: "p1", BaseURL: flaky.URL, Priority: 1, TimeoutMs: 5000})
	p2 := openai.New(openai.Config{Name: "p2", BaseURL: good.URL, Priority: 2, TimeoutMs: 5000})
	rt := router.New([]provider.Provider{p1, p2}, []*router.Route{{
		Model:      "auto",
		Candidates: []router.Candidate{{Provider: p1, Model: "m1"}, {Provider: p2, Model: "m2"}},
	}})
	rt.SetCircuit("p1", router.CircuitConfig{FailureThreshold: 1, CooldownMs: 1, AutoDisableAfter: 99})
	fr, err := router.NewFailureRules([]router.FailureRule{{Match: "boom", Backoff: true}})
	if err != nil {
		t.Fatal(err)
	}
	rt.SetFailureRules(fr)
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	h := New(rt, ul, nil)

	for i, want := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second} {
		if rec := post(t, h); rec.Code != 200 {
			t.Fatalf("request %d: status %d", i+1, rec.Code)
		}
		d := openWindow(t, rt)
		if d > want || d < want-time.Second {
			t.Fatalf("request %d: open window %v, want ~%v", i+1, d, want)
		}
		// Let the 1ms cooldown lapse so p1 is probed again next request.
		rt.ResetCircuit("p1")
	}
	// p1 recovers: the next failure starts over at the base cooldown.
	status = 200
	if rec := post(t, h); rec.Code != 200 {
		t.Fatalf("recovery: status %d", rec.Code)
	}
	status = 503
	if rec := post(t, h); rec.Code != 200 {
		t.Fatalf("post-recovery: status %d", rec.Code)
	}
	if d := openWindow(t, rt); d > 2*time.Second {
		t.Fatalf("after recovery open window %v, want ~2s (level reset)", d)
	}
}
