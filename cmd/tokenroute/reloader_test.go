package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/auth"
	"github.com/Jarvisagentic/tokenroute/internal/config"
	"github.com/Jarvisagentic/tokenroute/internal/metrics"
	"github.com/Jarvisagentic/tokenroute/internal/ratelimit"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// reloaderFixture builds a runtimeReloader with fake build/startWorkers hooks
// and a pre-populated current state carrying process-lifetime services.
type reloaderFixture struct {
	current *atomic.Pointer[serverState]
	active  *atomic.Pointer[config.Config]
	rel     *runtimeReloader

	// captured build/startWorkers args
	mu             sync.Mutex
	buildCfg       *config.Config
	buildShared    *usage.PriceStore
	started        []string // worker names started per call
	startedState   *serverState
	disposed       bool
	disposedState  *serverState
	cancelledPrev  bool
	prevCancelUsed bool

	// process-lifetime services on the old state
	usageStore *usage.Logger
	keyStore   *auth.Store
	limiter    *ratelimit.Registry
	mreg       *metrics.Registry
}

func newReloaderFixture(t *testing.T, buildErr error) *reloaderFixture {
	t.Helper()
	f := &reloaderFixture{}

	f.usageStore = usage.NewLogger(nil)
	f.keyStore = nil // auth.Store requires a DB; identity check uses pointer compare
	f.limiter = ratelimit.NewRegistry()
	f.mreg = metrics.New()

	prev := &serverState{
		router:  router.New(nil, nil),
		usage:   f.usageStore,
		prices:  usage.NewPriceStore(map[string]usage.Price{"old-model": {PromptPer1M: 1}}),
		keys:    f.keyStore,
		limiter: f.limiter,
		metrics: f.mreg,
	}
	prevCancel := func() { f.mu.Lock(); f.cancelledPrev = true; f.mu.Unlock() }
	prev.workersCancel = prevCancel

	f.current = &atomic.Pointer[serverState]{}
	f.current.Store(prev)
	f.active = &atomic.Pointer[config.Config]{}
	f.active.Store(&config.Config{
		Listen: ":1", AdminListen: ":2", UsageDB: "a.db", AdminKey: "k",
		Prices: map[string]config.PriceConfig{},
	})

	f.rel = &runtimeReloader{
		current: f.current,
		active:  f.active,
		metrics: f.mreg,
		build: func(cfg *config.Config, shared *usage.PriceStore) (*serverState, error) {
			f.mu.Lock()
			f.buildCfg = cfg
			f.buildShared = shared
			f.mu.Unlock()
			if buildErr != nil {
				return nil, buildErr
			}
			prices := shared
			if prices == nil {
				prices = usage.NewPriceStore(nil)
			}
			for m, p := range cfg.Prices {
				prices.Set(m, usage.Price{PromptPer1M: p.PromptPer1M})
			}
			return &serverState{router: router.New(nil, nil), prices: prices}, nil
		},
		startWorkers: func(ctx context.Context, cancel context.CancelFunc, cfg *config.Config, st *serverState) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.startedState = st
			names := []string{}
			if !isOff(cfg.ModelCatalog) {
				names = append(names, "catalog")
				st.modalities = func(string) ([]string, bool) { return nil, false }
			}
			if !isOff(cfg.PricingSync) {
				names = append(names, "pricing")
			}
			if cfg.HealthCheck != nil && cfg.HealthCheck.Enabled {
				names = append(names, "health")
			}
			for _, p := range cfg.Providers {
				if p.BalanceProbe != nil {
					names = append(names, "balance")
					break
				}
			}
			f.started = append(f.started, names...)
			st.workersCancel = cancel
		},
		dispose: func(st *serverState) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.disposed = true
			f.disposedState = st
		},
	}
	return f
}

func isOff(s string) bool { return s == "off" || s == "OFF" || s == "Off" }

func (f *reloaderFixture) gotStarted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...)
}

func (f *reloaderFixture) wasCancelled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancelledPrev
}

func (f *reloaderFixture) gotBuildCfg() *config.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buildCfg
}

func basePersisted() *config.Config {
	return &config.Config{
		Listen: ":9", AdminListen: ":8", UsageDB: "other.db", AdminKey: "newk",
		Prices: map[string]config.PriceConfig{"m1": {PromptPer1M: 2}},
	}
}

func TestRuntimeReloaderRoutePriceEditsAppearInNewState(t *testing.T) {
	f := newReloaderFixture(t, nil)
	p := basePersisted()
	if err := f.rel.Apply(context.Background(), p, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	st := f.current.Load()
	if st == nil || st.router == nil {
		t.Fatal("no new state stored")
	}
	if _, ok := st.prices.Get("m1"); !ok {
		t.Errorf("new price m1 missing from new state: %v", st.prices.Snapshot())
	}
	if got := f.active.Load(); got != p {
		t.Errorf("active config not swapped: %p want %p", got, p)
	}
}

func TestRuntimeReloaderRemovedPriceDisappears(t *testing.T) {
	f := newReloaderFixture(t, nil)
	p := basePersisted()
	p.Prices = map[string]config.PriceConfig{} // all removed
	if err := f.rel.Apply(context.Background(), p, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	st := f.current.Load()
	if _, ok := st.prices.Get("old-model"); ok {
		t.Errorf("removed price old-model survived reload: %v", st.prices.Snapshot())
	}
}

func TestRuntimeReloaderRestartFieldsRetainedFromActiveOnAdminApply(t *testing.T) {
	f := newReloaderFixture(t, nil)
	p := basePersisted()
	restart := []string{"listen", "admin_listen", "usage_db", "admin_key"}
	if err := f.rel.Apply(context.Background(), p, restart); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	built := f.gotBuildCfg()
	if built == nil {
		t.Fatal("build never called")
	}
	if built.Listen != ":1" || built.AdminListen != ":2" || built.UsageDB != "a.db" || built.AdminKey != "k" {
		t.Errorf("restart fields not retained from active: %+v", built)
	}
	// active config pointer still swapped to the effective config
	if f.active.Load().Listen != ":1" {
		t.Errorf("active config listen = %q, want retained :1", f.active.Load().Listen)
	}
}

func TestRuntimeReloaderSIGHUPAppliesAllFields(t *testing.T) {
	f := newReloaderFixture(t, nil)
	p := basePersisted()
	if err := f.rel.Apply(context.Background(), p, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	built := f.gotBuildCfg()
	if built.Listen != ":9" || built.AdminListen != ":8" || built.UsageDB != "other.db" || built.AdminKey != "newk" {
		t.Errorf("SIGHUP did not apply persisted bootstrap fields: %+v", built)
	}
}

func TestRuntimeReloaderValidateBuildsWithoutSideEffects(t *testing.T) {
	f := newReloaderFixture(t, nil)
	prevState := f.current.Load()
	prevCfg := f.active.Load()
	persisted := basePersisted()
	restart := []string{"listen", "admin_listen", "usage_db", "admin_key"}

	if err := f.rel.Validate(context.Background(), persisted, restart); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	built := f.gotBuildCfg()
	if built == nil || built.Listen != ":1" || built.AdminListen != ":2" || built.UsageDB != "a.db" || built.AdminKey != "k" {
		t.Fatalf("restart fields not retained for dry run: %+v", built)
	}
	if f.current.Load() != prevState || f.active.Load() != prevCfg {
		t.Fatal("dry run swapped runtime pointers")
	}
	if f.wasCancelled() {
		t.Fatal("dry run cancelled active workers")
	}
	if len(f.gotStarted()) != 0 {
		t.Fatalf("dry run started workers: %v", f.gotStarted())
	}
	f.mu.Lock()
	disposed, disposedState := f.disposed, f.disposedState
	f.mu.Unlock()
	if !disposed || disposedState == nil || disposedState == prevState {
		t.Fatalf("candidate state not disposed: disposed=%v state=%p", disposed, disposedState)
	}
}

func TestRuntimeReloaderValidateBuildFailureLeavesRuntimeUntouched(t *testing.T) {
	f := newReloaderFixture(t, errors.New("boom"))
	prevState := f.current.Load()
	prevCfg := f.active.Load()

	if err := f.rel.Validate(context.Background(), basePersisted(), nil); err == nil {
		t.Fatal("expected error")
	}
	if f.current.Load() != prevState || f.active.Load() != prevCfg {
		t.Fatal("failed dry run swapped runtime pointers")
	}
	if f.wasCancelled() || len(f.gotStarted()) != 0 {
		t.Fatal("failed dry run changed worker lifecycle")
	}
	f.mu.Lock()
	disposed := f.disposed
	f.mu.Unlock()
	if disposed {
		t.Fatal("failed build reported a candidate state to dispose")
	}
}

func TestRuntimeReloaderBuildFailureLeavesPointersUnchanged(t *testing.T) {
	f := newReloaderFixture(t, errors.New("boom"))
	prevState := f.current.Load()
	prevCfg := f.active.Load()
	err := f.rel.Apply(context.Background(), basePersisted(), nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if f.current.Load() != prevState {
		t.Error("current pointer changed on build failure")
	}
	if f.active.Load() != prevCfg {
		t.Error("active pointer changed on build failure")
	}
	if f.wasCancelled() {
		t.Error("old workers cancelled on failed apply")
	}
	if len(f.gotStarted()) != 0 {
		t.Errorf("workers started on failed apply: %v", f.gotStarted())
	}
}

func TestRuntimeReloaderReusesProcessLifetimeServices(t *testing.T) {
	f := newReloaderFixture(t, nil)
	if err := f.rel.Apply(context.Background(), basePersisted(), nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	st := f.current.Load()
	if st.usage != f.usageStore {
		t.Error("usage store not reused")
	}
	if st.keys != f.keyStore {
		t.Error("key store not reused")
	}
	if st.limiter != f.limiter {
		t.Error("limiter not reused")
	}
	if st.metrics != f.mreg {
		t.Error("metrics not reused")
	}
}

func TestRuntimeReloaderStartsNewWorkersThenCancelsOld(t *testing.T) {
	f := newReloaderFixture(t, nil)
	if err := f.rel.Apply(context.Background(), basePersisted(), nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	st := f.current.Load()
	if st.workersCancel == nil {
		t.Error("new state has no workersCancel")
	}
	if !f.wasCancelled() {
		t.Error("old workers not cancelled after swap")
	}
}

func TestRuntimeReloaderWorkerSetFollowsConfig(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*config.Config)
		want []string
	}{
		{"all on by default", func(c *config.Config) {}, []string{"catalog", "pricing"}},
		{"catalog off", func(c *config.Config) { c.ModelCatalog = "off" }, []string{"pricing"}},
		{"pricing off", func(c *config.Config) { c.PricingSync = "off" }, []string{"catalog"}},
		{"both off", func(c *config.Config) { c.ModelCatalog = "off"; c.PricingSync = "off" }, nil},
		{"health on", func(c *config.Config) {
			c.ModelCatalog = "off"
			c.PricingSync = "off"
			c.HealthCheck = &config.HealthCheckConfig{Enabled: true}
		}, []string{"health"}},
		{"balance on", func(c *config.Config) {
			c.ModelCatalog = "off"
			c.PricingSync = "off"
			c.Providers = []config.ProviderConfig{{Name: "p1", BalanceProbe: &config.BalanceProbeConfig{URL: "http://x"}}}
		}, []string{"balance"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReloaderFixture(t, nil)
			p := basePersisted()
			tc.mut(p)
			if err := f.rel.Apply(context.Background(), p, nil); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			got := f.gotStarted()
			if len(got) != len(tc.want) {
				t.Fatalf("started workers %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("started workers %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestOverlayRestartFields(t *testing.T) {
	cand := &config.Config{Listen: ":9", AdminListen: ":8", UsageDB: "b.db", AdminKey: "nk", ModelCatalog: "off"}
	active := &config.Config{Listen: ":1", AdminListen: ":2", UsageDB: "a.db", AdminKey: "k", ModelCatalog: "on"}
	out := overlayRestartFields(cand, active)
	if out == cand {
		t.Error("overlayRestartFields mutated candidate in place; want copy")
	}
	if out.Listen != ":1" || out.AdminListen != ":2" || out.UsageDB != "a.db" || out.AdminKey != "k" {
		t.Errorf("restart fields not overlaid: %+v", out)
	}
	if out.ModelCatalog != "off" {
		t.Errorf("non-restart field changed: %q", out.ModelCatalog)
	}
	// originals untouched
	if cand.Listen != ":9" || active.Listen != ":1" {
		t.Error("inputs mutated")
	}
}

// ensure context import used
var _ = time.Second
