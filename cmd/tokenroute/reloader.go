package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Jarvisagentic/tokenroute/internal/catalog"
	"github.com/Jarvisagentic/tokenroute/internal/config"
	"github.com/Jarvisagentic/tokenroute/internal/metrics"
	"github.com/Jarvisagentic/tokenroute/internal/pricing"
	"github.com/Jarvisagentic/tokenroute/internal/server"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// runtimeReloader coordinates hot config swaps shared by the SIGHUP handler
// and the admin config API. Each Apply builds a fresh serverState (with a
// fresh configured price map so removed prices disappear), starts candidate
// workers only after successful construction, swaps the atomic pointers, then
// cancels the old state's workers.
type runtimeReloader struct {
	mu           sync.Mutex
	current      *atomic.Pointer[serverState]
	active       *atomic.Pointer[config.Config]
	metrics      *metrics.Registry
	build        func(*config.Config, *usage.PriceStore) (*serverState, error)
	startWorkers func(context.Context, context.CancelFunc, *config.Config, *serverState)
	dispose      func(*serverState)
}

// Apply swaps the runtime to persisted. restartPaths lists bootstrap fields
// (listen, admin_listen, usage_db, admin_key) that an admin apply must retain
// from the active config — the process cannot rebind listeners or reopen the
// DB hot, so those four always come from the running config. nil (SIGHUP
// path) means "no retention": the complete persisted config is applied
// verbatim, and main() handles a listen-address change by rebinding the HTTP
// server afterwards. A non-nil slice, including an empty one, identifies the
// admin API path and retains all four active bootstrap fields so a deferred
// value cannot become active during a later unrelated hot apply.
func (r *runtimeReloader) Apply(ctx context.Context, persisted *config.Config, restartPaths []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Admin PUT passes a non-nil slice (possibly empty), so bootstrap fields
	// always stay at their active values until restart. SIGHUP passes nil and
	// applies the persisted document verbatim.
	effective := persisted
	if restartPaths != nil {
		effective = overlayRestartFields(persisted, r.active.Load())
	}

	// Fresh price map: buildState(effective, nil) seeds only the new explicit
	// prices: entries, so removed config prices actually disappear.
	nstate, err := r.build(effective, nil)
	if err != nil {
		return err
	}

	prev := r.current.Load()
	// Carry over process-lifetime services.
	nstate.usage = prev.usage
	nstate.keys = prev.keys
	nstate.limiter = prev.limiter
	nstate.metrics = prev.metrics
	bindMetrics(prev.metrics, nstate.router)
	nstate.adminKey = effective.AdminKey
	// Keep the warm response cache unless the reload toggled it off.
	nstate.cache = prev.cache
	if !effective.Cache.Enabled {
		nstate.cache = nil
	} else if nstate.cache == nil {
		nstate.cache = server.NewCache(true, effective.Cache.TTLSeconds)
	}

	// Candidate worker context; cancelled on any failure before the swap.
	wctx, wcancel := context.WithCancel(context.Background())
	ok := false
	defer func() {
		if !ok {
			wcancel()
		}
	}()
	if r.startWorkers != nil {
		r.startWorkers(wctx, wcancel, effective, nstate)
	}
	if nstate.workersCancel == nil {
		nstate.workersCancel = wcancel
	}

	r.current.Store(nstate)
	r.active.Store(effective)
	ok = true
	if prev.workersCancel != nil {
		prev.workersCancel()
	}
	return nil
}

// Validate builds the effective runtime state without applying it or starting
// workers. buildState currently owns no long-lived resources until workers run.
func (r *runtimeReloader) Validate(_ context.Context, persisted *config.Config, restartPaths []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Admin PUT passes a non-nil slice (possibly empty), so bootstrap fields
	// always stay at their active values until restart. SIGHUP passes nil and
	// applies the persisted document verbatim.
	effective := persisted
	if restartPaths != nil {
		effective = overlayRestartFields(persisted, r.active.Load())
	}
	nstate, err := r.build(effective, nil)
	if err != nil {
		return err
	}
	dispose := r.dispose
	if dispose == nil {
		dispose = disposeRuntimeState
	}
	dispose(nstate)
	return nil
}

// disposeRuntimeState releases resources allocated during buildState. Workers
// are never started by Validate, so provider transports are its only resources.
func disposeRuntimeState(st *serverState) {
	if st == nil || st.router == nil {
		return
	}
	for _, p := range st.router.Providers() {
		if closer, ok := p.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
	}
}

// overlayRestartFields returns a copy of candidate with bootstrap fields
// (listen, admin_listen, usage_db, admin_key) retained from active.
func overlayRestartFields(candidate, active *config.Config) *config.Config {
	out := *candidate
	out.Listen = active.Listen
	out.AdminListen = active.AdminListen
	out.UsageDB = active.UsageDB
	out.AdminKey = active.AdminKey
	return &out
}

// startStateWorkers starts the background workers owned by a serverState:
// model-catalog sync (cache loaded before the state is exposed, refresh in
// background), LiteLLM pricing sync, health checks, and balance probes. Any
// worker whose config is "off"/disabled is not started.
func startStateWorkers(ctx context.Context, cancel context.CancelFunc, cfg *config.Config, st *serverState) {
	st.workersCancel = cancel

	// Daily model-capability sync (models.dev) — strictly additive below the
	// hand-written price table; "off" disables. Load the persisted cache
	// synchronously so the swapped-in router already has the lookup.
	if !strings.EqualFold(cfg.ModelCatalog, "off") {
		syncer := catalog.NewSyncer(
			filepath.Join(filepath.Dir(cfg.UsageDB), "model-catalog.json"),
			"", 0, st.prices,
		)
		syncer.LoadCache()
		st.modalities = syncer.Modalities
		st.router.SetModalityLookup(syncer.Modalities)
		go syncer.Run(ctx)
	}

	// LiteLLM pricing sync — fills price gaps, config always wins.
	if !strings.EqualFold(cfg.PricingSync, "off") {
		psync := pricing.NewSyncer(st.prices)
		go psync.Run(ctx, 0)
	}

	// Background health checks and account-balance probes.
	server.RunHealthChecks(ctx, st.router, healthTargets(cfg, st.router))
	server.RunBalanceProbes(ctx, st.router, balanceTargets(cfg))
}
