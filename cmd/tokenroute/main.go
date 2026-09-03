// Command gateway runs the AI gateway HTTP server.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/auth"
	"github.com/Jarvisagentic/tokenroute/internal/config"
	"github.com/Jarvisagentic/tokenroute/internal/metrics"
	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/provider/anthropic"
	"github.com/Jarvisagentic/tokenroute/internal/provider/gemini"
	"github.com/Jarvisagentic/tokenroute/internal/provider/openai"
	"github.com/Jarvisagentic/tokenroute/internal/ratelimit"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/search"
	"github.com/Jarvisagentic/tokenroute/internal/server"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// serverState is swapped atomically on hot reload; in-flight requests
// keep the pointers they started with.
type serverState struct {
	router   *router.Router
	usage    *usage.Logger
	prices   *usage.PriceStore // shared, goroutine-safe price store
	keys     *auth.Store
	limiter  *ratelimit.Registry
	adminKey string
	cache    *server.RespCache
	metrics  *metrics.Registry
	// streamIdleMs is the per-provider stream idle timeout (provider name -> ms).
	streamIdleMs map[string]int
	// providerTypes: provider name -> configured type (expr pricing semantics).
	providerTypes map[string]string
	// retryPolicy: configured failover/disable overrides (nil = built-in).
	retryPolicy *router.RetryPolicy
	// groupRatio: group name -> cost multiplier (nil = unset).
	groupRatio map[string]float64
	maxBodyMB  int
	// searchBackends: ordered web-search upstreams for /v1/search.
	searchBackends []search.Backend
	// modalities: model -> synced input modalities (catalog sync); nil when the
	// catalog is off. Survives reloads so the swapped router keeps the lookup.
	modalities func(string) ([]string, bool)
	// workersCancel cancels this state's background workers (health checks,
	// balance probes, model-catalog sync, pricing sync). Owned per state.
	workersCancel context.CancelFunc
}

// buildState builds a fresh server state. sharedPrices is always nil in
// production (startup and reload both build a fresh store seeded only from
// cfg.Prices, so removed config prices disappear); the parameter exists so
// tests can inject a pre-seeded store.
func buildState(cfg *config.Config, sharedPrices *usage.PriceStore) (*serverState, error) {
	provs := make([]provider.Provider, 0, len(cfg.Providers))
	byName := map[string]provider.Provider{}
	sit := map[string]int{} // stream idle timeout ms per provider name
	ptypes := map[string]string{}
	mappings := map[string]map[string]string{}
	for _, pc := range cfg.Providers {
		// ponytail: 0 means "unset" here, so an explicit 0 in YAML also gets
		// the default; use a huge value to effectively disable, or switch to
		// *int when a real disable knob is needed.
		rhtMs := pc.ResponseHeaderTimeoutMs
		if rhtMs == 0 {
			rhtMs = 900000 // 15min default
		}
		sitMs := pc.StreamIdleTimeoutMs
		if sitMs == 0 {
			sitMs = 300000 // 5min default
		}
		sit[pc.Name], mappings[pc.Name] = sitMs, pc.ModelMapping
		ptypes[pc.Name] = pc.Type
		if ptypes[pc.Name] == "" {
			ptypes[pc.Name] = "openai"
		}
		var p provider.Provider
		switch pc.Type {
		case "openai", "":
			p = openai.New(openai.Config{
				Name:                    pc.Name,
				BaseURL:                 pc.BaseURL,
				APIKey:                  pc.APIKey,
				APIKeys:                 pc.APIKeys,
				Priority:                pc.Priority,
				TimeoutMs:               pc.TimeoutMs,
				ResponseHeaderTimeoutMs: rhtMs,
			})
		case "anthropic":
			p = anthropic.New(anthropic.Config{
				Name:                    pc.Name,
				BaseURL:                 pc.BaseURL,
				APIKey:                  pc.APIKey,
				APIKeys:                 pc.APIKeys,
				Priority:                pc.Priority,
				TimeoutMs:               pc.TimeoutMs,
				ResponseHeaderTimeoutMs: rhtMs,
			})
		case "gemini":
			p = gemini.New(gemini.Config{
				Name:                    pc.Name,
				BaseURL:                 pc.BaseURL,
				APIKey:                  pc.APIKey,
				APIKeys:                 pc.APIKeys,
				Priority:                pc.Priority,
				TimeoutMs:               pc.TimeoutMs,
				ResponseHeaderTimeoutMs: rhtMs,
			})
		default:
			return nil, fmt.Errorf("unknown provider type %q for %q", pc.Type, pc.Name)
		}
		provs = append(provs, p)
		byName[pc.Name] = p
	}
	routes := make([]*router.Route, 0, len(cfg.Routes))
	for _, rc := range cfg.Routes {
		rt := &router.Route{Model: rc.Model, Strategy: rc.Strategy, Multiplier: rc.Multiplier, FallbackRoutes: rc.FallbackRoutes,
			PromptCacheAffinity: rc.PromptCacheAffinity, HashOn: rc.HashOn, Sticky: rc.Sticky}
		if rc.Affinity != nil && rc.Affinity.Enabled {
			rt.PromptCacheAffinity = true
			rt.AffinityKeyHeader = rc.Affinity.KeyHeader
			rt.AffinitySkipRetry = rc.Affinity.SkipRetryOnFailure
			if rc.Affinity.TTLMs > 0 {
				rt.AffinityTTL = time.Duration(rc.Affinity.TTLMs) * time.Millisecond
			}
		}
		if rc.FusionJudge != nil {
			rt.FusionJudge = router.FusionJudgeConfig{
				Judge:     rc.FusionJudge.Judge,
				MinPanel:  rc.FusionJudge.MinPanel,
				GraceMs:   rc.FusionJudge.GraceMs,
				TimeoutMs: rc.FusionJudge.TimeoutMs,
			}
		}
		for _, cc := range rc.Candidates {
			rt.Candidates = append(rt.Candidates, router.Candidate{
				Provider:      byName[cc.Provider],
				Model:         cc.Model,
				Weight:        cc.Weight,
				Groups:        cc.Groups,
				Tags:          cc.Tags,
				ParamOverride: cc.ParamOverride,
			})
		}
		routes = append(routes, rt)
	}
	prices := sharedPrices
	if prices == nil {
		prices = usage.NewPriceStore(nil)
	}
	// Seed config prices into the fresh store: config always wins over
	// synced entries (OmniRoute resolution order). Reload builds a new
	// store, so prices removed from YAML actually disappear.
	for m, pc := range cfg.Prices {
		prices.Set(m, usage.Price{PromptPer1M: pc.PromptPer1M, CompletionPer1M: pc.CompletionPer1M, EmbedPer1M: pc.EmbedPer1M, ContextTokens: pc.ContextTokens, Expr: pc.Expr})
	}
	rt := router.New(provs, routes)
	rt.SetPrices(prices)
	// Prompt-cache affinity: one shared pin cache when any route opts in or
	// the global default enables it (per-route flag gates use at request time).
	rt.AffinityDefault = cfg.PromptCacheAffinity
	if cfg.PromptCacheAffinity {
		rt.SetAffinity(router.NewAffinityCache(time.Hour))
	} else {
		for _, rc := range cfg.Routes {
			if rc.PromptCacheAffinity || (rc.Affinity != nil && rc.Affinity.Enabled) {
				rt.SetAffinity(router.NewAffinityCache(time.Hour))
				break
			}
		}
	}
	if len(cfg.Aliases) > 0 {
		rt.SetAliases(cfg.Aliases)
	}
	for name, m := range mappings {
		rt.SetModelMapping(name, m)
	}
	for _, pc := range cfg.Providers {
		if pc.ParamOverride != nil || pc.ParamDelete != nil || pc.HeaderOverride != nil || pc.HeaderPass != nil {
			rt.SetProviderOverride(pc.Name, router.ProviderOverride{
				ParamSet: pc.ParamOverride, ParamDel: pc.ParamDelete,
				HeaderSet: pc.HeaderOverride, HeaderPass: pc.HeaderPass,
			})
		}
	}
	for _, pc := range cfg.Providers {
		if pc.Circuit != nil {
			rt.SetCircuit(pc.Name, router.CircuitConfig{
				FailureThreshold: pc.Circuit.FailureThreshold,
				CooldownMs:       pc.Circuit.CooldownMs,
				AutoDisableAfter: pc.Circuit.AutoDisableAfter,
				Mode:             pc.Circuit.Mode,
				FailurePercent:   pc.Circuit.FailurePercent,
				MinRequests:      pc.Circuit.MinRequests,
				AllowedFails:     router.ParseAllowedFails(pc.Circuit.AllowedFails),
			})
		}
		if pc.QuotaTokenLimit > 0 {
			win := time.Duration(pc.QuotaWindowSeconds) * time.Second
			if win <= 0 {
				win = time.Minute
			}
			// One ledger entry per candidate model routed to this provider.
			for _, rc := range cfg.Routes {
				for _, cc := range rc.Candidates {
					if cc.Provider == pc.Name {
						rt.Quota().SetLimit(pc.Name, cc.Model, pc.QuotaTokenLimit, win)
					}
				}
			}
		}
	}
	// Free-tier catalog: seed the quota ledger with 30-day windows so
	// quota-aware strategies prefer candidates with live free budget.
	for _, ft := range cfg.FreeTier {
		if ft.MonthlyTokens > 0 && ft.Provider != "" && ft.Model != "" {
			rt.Quota().SetLimit(ft.Provider, ft.Model, ft.MonthlyTokens, 30*24*time.Hour)
		}
	}
	backends := make([]search.Backend, 0, len(cfg.Search))
	for _, sc := range cfg.Search {
		keys := append([]string(nil), sc.APIKeys...)
		if sc.APIKey != "" {
			keys = append(keys, sc.APIKey)
		}
		pool := provider.NewKeyPool(keys...)
		switch strings.ToLower(sc.Backend) {
		case "tavily":
			backends = append(backends, &search.Tavily{Pool: pool})
		case "brave":
			backends = append(backends, &search.Brave{Pool: pool})
		case "exa":
			backends = append(backends, &search.Exa{Pool: pool})
		default:
			return nil, fmt.Errorf("unknown search backend %q", sc.Backend)
		}
	}
	var rp *router.RetryPolicy
	if cfg.RetryPolicy != nil {
		var err error
		rp, err = router.NewRetryPolicy(cfg.RetryPolicy.RetryStatusRanges, cfg.RetryPolicy.DisableStatusRanges,
			cfg.RetryPolicy.NeverRetry, cfg.RetryPolicy.DisableKeywords)
		if err != nil {
			return nil, fmt.Errorf("retry_policy: %w", err)
		}
	}
	// failure_rules (9router ERROR_RULES): ordered text/status cooldown
	// overrides; nil when unconfigured (built-in circuit cooldowns).
	fr, err := cfg.FailureRulesPolicy()
	if err != nil {
		return nil, err
	}
	rt.SetFailureRules(fr)
	return &serverState{router: rt, prices: prices, streamIdleMs: sit, providerTypes: ptypes, retryPolicy: rp, groupRatio: cfg.GroupRatio, maxBodyMB: cfg.MaxBodyMB, searchBackends: backends}, nil
}

// healthTargets resolves per-provider probe targets: a provider's own
// health_check block wins; otherwise the global block applies when enabled.
func healthTargets(cfg *config.Config, rt *router.Router) []server.HealthTarget {
	byName := map[string]provider.Provider{}
	for _, p := range rt.Providers() {
		byName[p.Name()] = p
	}
	var out []server.HealthTarget
	for _, pc := range cfg.Providers {
		hc := pc.HealthCheck
		if hc == nil && cfg.HealthCheck != nil && cfg.HealthCheck.Enabled {
			cp := *cfg.HealthCheck
			hc = &cp
		}
		if hc == nil || !hc.Enabled {
			continue
		}
		model := hc.Model
		if model == "" {
			model = rt.FirstModelFor(pc.Name)
		}
		if model == "" {
			continue // no candidate configured; nothing to probe
		}
		out = append(out, server.HealthTarget{
			Provider: byName[pc.Name],
			Model:    model,
			Interval: time.Duration(hc.IntervalMs) * time.Millisecond,
		})
	}
	return out
}

// balanceTargets resolves the opt-in per-provider balance probes.
func balanceTargets(cfg *config.Config) []server.BalanceTarget {
	var out []server.BalanceTarget
	for _, pc := range cfg.Providers {
		bp := pc.BalanceProbe
		if bp == nil || bp.URL == "" {
			continue
		}
		minUSD := bp.MinUSD
		if minUSD <= 0 {
			minUSD = 0.01
		}
		interval := time.Duration(bp.IntervalMs) * time.Millisecond
		if interval <= 0 {
			interval = 5 * time.Minute
		}
		key := pc.APIKey
		if key == "" && len(pc.APIKeys) > 0 {
			key = pc.APIKeys[0] // pool: any key sees the same account balance
		}
		out = append(out, server.BalanceTarget{
			Provider: pc.Name, URL: bp.URL, APIKey: key,
			Interval: interval, MinUSD: minUSD,
		})
	}
	return out
}

// openDB opens the shared usage/auth DB; fatal at startup, skipped on reload failure.
func openDB(path string) (*sql.DB, error) {
	return usage.OpenDB(path)
}

// bindMetrics points the registry's circuit gauge at the live router.
// Called again on reload so the gauge follows the swapped router.
func bindMetrics(m *metrics.Registry, rt *router.Router) {
	m.BindGauges(func() []string {
		names := []string{}
		for _, p := range rt.Providers() {
			names = append(names, p.Name())
		}
		return names
	},
		func(name string) bool { return rt.CircuitState(name) == "open" },
		func(name string) int { return rt.Inflight(name) },
	)
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	fs := flag.NewFlagSet("gateway", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "path to config file")
	_ = fs.Parse(args)

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}
	state, err := buildState(cfg, nil)
	if err != nil {
		log.Error("build state", "err", err)
		os.Exit(1)
	}
	db, err := openDB(cfg.UsageDB)
	if err != nil {
		log.Error("open usage db", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	state.usage = usage.NewLogger(db)
	keys, err := auth.NewStore(db)
	if err != nil {
		log.Error("open auth store", "err", err)
		os.Exit(1)
	}
	state.keys = keys
	state.limiter = ratelimit.NewRegistry()
	state.adminKey = cfg.AdminKey
	state.cache = server.NewCache(cfg.Cache.Enabled, cfg.Cache.TTLSeconds)
	mreg := metrics.New()
	state.metrics = mreg
	bindMetrics(mreg, state.router)

	// Background workers owned by the initial state (model catalog, pricing
	// sync, health checks, balance probes). Cancelled on shutdown.
	wctx, wcancel := context.WithCancel(context.Background())
	defer wcancel()
	startStateWorkers(wctx, wcancel, cfg, state)
	var current atomic.Pointer[serverState]
	var activeConfig atomic.Pointer[config.Config]
	current.Store(state)
	activeConfig.Store(cfg)
	reloader := &runtimeReloader{
		current: &current, active: &activeConfig, metrics: mreg,
		build:        buildState,
		startWorkers: startStateWorkers,
		dispose:      disposeRuntimeState,
	}
	configStore := config.NewStore(*configPath, 5)

	log.Info("starting gateway",
		"listen", cfg.Listen,
		"providers", len(cfg.Providers),
		"routes", len(cfg.Routes),
	)

	// Handler resolves state per-request so reloads take effect.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := current.Load()
		server.NewWithOptions(server.Options{
			Router: st.router, Usage: st.usage, Prices: st.prices,
			Keys: st.keys, Limiter: st.limiter, AdminKey: st.adminKey,
			Cache:          st.cache,
			Metrics:        st.metrics,
			SeparateAdmin:  cfg.AdminListen != "",
			StreamIdleMs:   st.streamIdleMs,
			ProviderTypes:  st.providerTypes,
			RetryPolicy:    st.retryPolicy,
			GroupRatio:     st.groupRatio,
			MaxBodyMB:      st.maxBodyMB,
			SearchBackends: st.searchBackends,
			ConfigStore:    configStore,
			ApplyConfig:    reloader.Apply,
			ValidateConfig: reloader.Validate,
		}).ServeHTTP(w, r)
	})
	srv := &http.Server{Addr: cfg.Listen, Handler: handler}
	listenAddr := cfg.Listen

	// Optional dedicated admin listener (public mux has no /admin routes).
	var adminSrv *http.Server
	adminAddr := cfg.AdminListen
	if adminAddr != "" {
		adminHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			st := current.Load()
			server.NewAdminOnly(server.Options{
				Router: st.router, Usage: st.usage, Prices: st.prices,
				Keys: st.keys, Limiter: st.limiter, AdminKey: st.adminKey,
				Metrics:        st.metrics,
				ConfigStore:    configStore,
				ApplyConfig:    reloader.Apply,
				ValidateConfig: reloader.Validate,
			}).ServeHTTP(w, r)
			// admin-only mux has no proxied endpoints; body cap unused
		})
		adminSrv = &http.Server{Addr: adminAddr, Handler: adminHandler}
	}

	errCh := make(chan error, 2)
	serve := func(s *http.Server) {
		errCh <- s.ListenAndServe()
	}
	go serve(srv)
	if adminSrv != nil {
		go serve(adminSrv)
		log.Info("admin listener", "addr", adminAddr)
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	for {
		select {
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				ncfg, err := config.Load(*configPath)
				if err != nil {
					log.Error("reload config", "err", err)
					continue
				}
				// nil restart paths: explicit operator reload applies the
				// complete persisted config (including listen address).
				if err := reloader.Apply(context.Background(), ncfg, nil); err != nil {
					log.Error("reload build state", "err", err)
					continue
				}
				log.Info("config reloaded",
					"providers", len(ncfg.Providers),
					"routes", len(ncfg.Routes),
				)
				if ncfg.Listen != listenAddr {
					old := srv
					srv = &http.Server{Addr: ncfg.Listen, Handler: handler}
					listenAddr = ncfg.Listen
					go serve(srv)
					log.Info("listen address changed",
						"old", old.Addr, "new", srv.Addr)
					go func() {
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						if err := old.Shutdown(ctx); err != nil {
							log.Error("old server shutdown", "err", err)
						}
					}()
				}
				continue
			}
			log.Info("shutting down", "signal", sig.String())
			if st := current.Load(); st != nil && st.workersCancel != nil {
				st.workersCancel()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if adminSrv != nil {
				if err := adminSrv.Shutdown(ctx); err != nil {
					log.Error("admin shutdown", "err", err)
				}
			}
			if err := srv.Shutdown(ctx); err != nil {
				log.Error("shutdown", "err", err)
				os.Exit(1)
			}
			return
		case err := <-errCh:
			if !errors.Is(err, http.ErrServerClosed) {
				log.Error("serve", "err", err)
				os.Exit(1)
			}
			// Old server drained after a listen change; keep running.
		}
	}
}
