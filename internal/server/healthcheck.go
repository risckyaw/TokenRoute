package server

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/router"
)

// HealthTarget is one provider to probe in the background.
type HealthTarget struct {
	Provider provider.Provider
	Model    string // upstream model for the probe
	Interval time.Duration
}

// healthBody is the minimal probe request (max_tokens=1).
var healthBody = []byte(`{"model":"","messages":[{"role":"user","content":"hi"}],"max_tokens":1}`)

// RunHealthChecks probes each target on its interval until ctx is cancelled
// (LiteLLM background health checks): a minimal chat completion through the
// normal provider path feeding Router.RecordResultKind with measured
// latency, so circuit state, percent-window counters and latency EMA stay
// warm before user traffic. Never touches the quota ledger or usage log.
func RunHealthChecks(ctx context.Context, rt *router.Router, targets []HealthTarget) {
	for _, t := range targets {
		t := t
		if t.Interval <= 0 {
			t.Interval = time.Minute
		}
		go func() {
			ticker := time.NewTicker(t.Interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					probeOnce(ctx, rt, t)
				}
			}
		}()
	}
}

// probeOnce sends one minimal completion and records the classified result.
func probeOnce(ctx context.Context, rt *router.Router, t HealthTarget) {
	start := time.Now()
	req := &provider.Request{Model: t.Model, Body: healthBody, Header: nil}
	resp, err := t.Provider.ChatComplete(ctx, req)
	lat := time.Since(start)
	if err != nil {
		f := router.ClassifyFailure(0, "", err)
		rt.RecordResultKind(t.Provider.Name(), lat, false, f.Kind, f.Kind != router.FailureUnknown)
		slog.Debug("health check", "provider", t.Provider.Name(), "err", err)
		return
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		rt.RecordResultKind(t.Provider.Name(), lat, true, router.FailureUnknown, true)
		return
	}
	f := router.ClassifyFailure(resp.StatusCode, string(snippet), nil)
	rt.RecordResultKind(t.Provider.Name(), lat, false, f.Kind, true)
	slog.Debug("health check", "provider", t.Provider.Name(), "status", resp.StatusCode)
}
