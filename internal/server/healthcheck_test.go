package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/provider/openai"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

func TestHealthCheck_FeedsRouterNoUsageRows(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"hc","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(up.Close)

	p := openai.New(openai.Config{Name: "p1", BaseURL: up.URL, Priority: 1, TimeoutMs: 5000})
	rt := router.New([]provider.Provider{p}, []*router.Route{{
		Model:      "auto",
		Candidates: []router.Candidate{{Provider: p, Model: "m1"}},
	}})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	RunHealthChecks(ctx, rt, []HealthTarget{{Provider: p, Model: "m1", Interval: 20 * time.Millisecond}})

	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() < 2 {
		t.Fatalf("expected >=2 probes, got %d", calls.Load())
	}
	if rt.LatencyMs("p1") == 0 {
		t.Fatal("latency EMA must be fed by probes")
	}
	if _, _, known := rt.Quota().Remaining("p1", "m1"); known {
		t.Fatal("health checks must not seed/track the quota ledger")
	}
	entries, err := ul.QueryRecent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("health checks must not write usage rows, got %d", len(entries))
	}
}

func TestHealthCheck_Shutdown(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"id":"hc"}`))
	}))
	t.Cleanup(up.Close)

	p := openai.New(openai.Config{Name: "p1", BaseURL: up.URL, Priority: 1, TimeoutMs: 5000})
	rt := router.New([]provider.Provider{p}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	RunHealthChecks(ctx, rt, []HealthTarget{{Provider: p, Model: "m1", Interval: 15 * time.Millisecond}})
	time.Sleep(60 * time.Millisecond)
	cancel()
	n := calls.Load()
	time.Sleep(60 * time.Millisecond)
	if got := calls.Load(); got > n+1 {
		t.Fatalf("probes continued after cancel: %d -> %d", n, got)
	}
}

func TestHealthCheck_FailureFeedsCircuit(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	t.Cleanup(up.Close)

	p := openai.New(openai.Config{Name: "p1", BaseURL: up.URL, Priority: 1, TimeoutMs: 5000})
	rt := router.New([]provider.Provider{p}, nil)
	rt.SetCircuit("p1", router.CircuitConfig{FailureThreshold: 2, CooldownMs: 60000})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	RunHealthChecks(ctx, rt, []HealthTarget{{Provider: p, Model: "m1", Interval: 15 * time.Millisecond}})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rt.CircuitState("p1") == "open" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("circuit state = %s, want open after failing probes", rt.CircuitState("p1"))
}
