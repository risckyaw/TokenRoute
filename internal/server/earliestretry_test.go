package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/provider/openai"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// limitedUpstream answers every request with 429 + the given Retry-After.
func limitedUpstream(t *testing.T, retryAfter string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
	}))
	t.Cleanup(s.Close)
	return s
}

// setupN wires one candidate per base URL onto route "auto" (priority order).
func setupN(t *testing.T, bases ...string) (http.Handler, *router.Router) {
	t.Helper()
	provs := make([]provider.Provider, 0, len(bases))
	cands := make([]router.Candidate, 0, len(bases))
	for i, b := range bases {
		p := openai.New(openai.Config{Name: "p" + strconv.Itoa(i+1), BaseURL: b, Priority: i + 1, TimeoutMs: 5000})
		provs = append(provs, p)
		cands = append(cands, router.Candidate{Provider: p, Model: "m" + strconv.Itoa(i+1)})
	}
	rt := router.New(provs, []*router.Route{{Model: "auto", Candidates: cands}})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	return New(rt, ul, nil), rt
}

// Three candidates 429 with distinct Retry-After hints; the client must get
// the EARLIEST (30s from p2), not the last-relayed 600s.
func TestEarliestRetryAfter_PicksSoonest(t *testing.T) {
	a := limitedUpstream(t, "300")
	b := limitedUpstream(t, "30")
	c := limitedUpstream(t, "600")
	h, _ := setupN(t, a.URL, b.URL, c.URL)

	rec := post(t, h)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429: %s", rec.Code, rec.Body.String())
	}
	ra, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
	if ra < 25 || ra > 35 {
		t.Fatalf("Retry-After = %d, want ~30 (earliest of 300/30/600)", ra)
	}
	if src := rec.Header().Get("X-TokenRoute-Retry-After-Source"); src != "upstream" {
		t.Fatalf("source = %q, want upstream", src)
	}
	// The upstream body is relayed verbatim (never rewritten).
	if !strings.Contains(rec.Body.String(), "rate limited") {
		t.Fatalf("body %q, want raw upstream body", rec.Body.String())
	}
}

// The relayed candidate already has the shortest hint: leave it untouched and
// do not stamp a source marker.
func TestEarliestRetryAfter_ShorterRelayedStays(t *testing.T) {
	a := limitedUpstream(t, "600")
	b := limitedUpstream(t, "20")
	h, _ := setupN(t, a.URL, b.URL)

	rec := post(t, h)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", rec.Code)
	}
	ra, _ := strconv.Atoi(rec.Header().Get("Retry-After"))
	if ra != 20 {
		t.Fatalf("Retry-After = %d, want the relayed 20 (already earliest)", ra)
	}
	if src := rec.Header().Get("X-TokenRoute-Retry-After-Source"); src != "" {
		t.Fatalf("source = %q, want empty (relayed value kept)", src)
	}
}

// A non-429 terminal failure keeps whatever the upstream said.
func TestEarliestRetryAfter_Non429Untouched(t *testing.T) {
	bad1 := upstream(t, 503, `{"error":"down1"}`)
	bad2 := upstream(t, 500, `{"error":"down2"}`)
	h, rt := setupN(t, bad1.URL, bad2.URL)
	// A live lock on another candidate must not leak into a 500 relay.
	rt.LockModel("p1", "m1", 45*time.Second)

	rec := post(t, h)
	if rec.Code != 500 {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "" {
		t.Fatalf("Retry-After = %q on a 500, want empty", ra)
	}
	if src := rec.Header().Get("X-TokenRoute-Retry-After-Source"); src != "" {
		t.Fatalf("source = %q, want empty", src)
	}
}

// Quota-ledger exhaustion of a candidate that never got called still supplies
// the reset hint when it is sooner than the relayed 429's. The exhausted
// candidate goes first so its transport error does not discard the 429 relay.
func TestEarliestRetryAfter_QuotaSource(t *testing.T) {
	dead := deadURL(t) // candidate 1 never answers; its quota is exhausted
	a := limitedUpstream(t, "600")
	h, rt := setupN(t, dead, a.URL)
	rt.Quota().SetLimit("p1", "m1", 100, 40*time.Second)
	rt.Quota().Record("p1", "m1", 100) // exhausted -> window reset is the hint

	rec := post(t, h)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429 relayed from p2: %s", rec.Code, rec.Body.String())
	}
	ra, _ := strconv.Atoi(rec.Header().Get("Retry-After"))
	if ra < 30 || ra > 45 {
		t.Fatalf("Retry-After = %d, want ~40 (p1 quota window)", ra)
	}
	if src := rec.Header().Get("X-TokenRoute-Retry-After-Source"); src != "quota" {
		t.Fatalf("source = %q, want quota", src)
	}
}

// A circuit-open candidate's probe time is the hint when it is soonest. The
// open candidate is filtered out of the ordered list entirely, proving the
// aggregation scans the whole route, not just what failover touched.
func TestEarliestRetryAfter_CircuitSource(t *testing.T) {
	a := limitedUpstream(t, "600")
	dead := deadURL(t)
	h, rt := setupN(t, a.URL, dead)
	rt.SetCircuit("p2", router.CircuitConfig{FailureThreshold: 1, CooldownMs: 25000, AutoDisableAfter: 99})
	rt.RecordResult("p2", time.Millisecond, false) // opens p2 for 25s

	rec := post(t, h)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", rec.Code)
	}
	ra, _ := strconv.Atoi(rec.Header().Get("Retry-After"))
	if ra < 20 || ra > 30 {
		t.Fatalf("Retry-After = %d, want ~25 (p2 circuit cooldown)", ra)
	}
	if src := rec.Header().Get("X-TokenRoute-Retry-After-Source"); src != "circuit" {
		t.Fatalf("source = %q, want circuit", src)
	}
}

func TestResetHintObserveEarliestWins(t *testing.T) {
	var h resetHint
	now := time.Now()
	h.observe(now.Add(time.Minute), "upstream")
	h.observe(now.Add(10*time.Second), "quota")
	h.observe(now.Add(2*time.Minute), "circuit")
	h.observe(now.Add(-time.Minute), "upstream") // past: ignored
	h.observe(time.Time{}, "circuit")            // zero: ignored
	if h.source != "quota" {
		t.Fatalf("source = %q, want quota", h.source)
	}
	if secs := h.seconds(); secs < 9 || secs > 12 {
		t.Fatalf("seconds = %d, want ~10", secs)
	}
	var empty resetHint
	if empty.seconds() != 0 {
		t.Fatalf("unset hint seconds = %d, want 0", empty.seconds())
	}
}
