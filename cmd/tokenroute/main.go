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
	"github.com/Jarvisagentic/tokenroute/internal/server"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// serverState is swapped atomically on hot reload; in-flight requests
// keep the pointers they started with.
type serverState struct {
	router   *router.Router
	usage    *usage.Logger
	prices   map[string]usage.Price
	keys     *auth.Store
	limiter  *ratelimit.Registry
	adminKey string
	cache    *server.RespCache
	metrics  *metrics.Registry
}

func buildState(cfg *config.Config) (*serverState, error) {
	provs := make([]provider.Provider, 0, len(cfg.Providers))
	byName := map[string]provider.Provider{}
	for _, pc := range cfg.Providers {
		var p provider.Provider
		switch pc.Type {
		case "openai", "":
			p = openai.New(openai.Config{
				Name:      pc.Name,
				BaseURL:   pc.BaseURL,
				APIKey:    pc.APIKey,
				APIKeys:   pc.APIKeys,
				Priority:  pc.Priority,
				TimeoutMs: pc.TimeoutMs,
			})
		case "anthropic":
			p = anthropic.New(anthropic.Config{
				Name:      pc.Name,
				BaseURL:   pc.BaseURL,
				APIKey:    pc.APIKey,
				APIKeys:   pc.APIKeys,
				Priority:  pc.Priority,
				TimeoutMs: pc.TimeoutMs,
			})
		case "gemini":
			p = gemini.New(gemini.Config{
				Name:      pc.Name,
				BaseURL:   pc.BaseURL,
				APIKey:    pc.APIKey,
				APIKeys:   pc.APIKeys,
				Priority:  pc.Priority,
				TimeoutMs: pc.TimeoutMs,
			})
		default:
			return nil, fmt.Errorf("unknown provider type %q for %q", pc.Type, pc.Name)
		}
		provs = append(provs, p)
		byName[pc.Name] = p
	}
	routes := make([]*router.Route, 0, len(cfg.Routes))
	for _, rc := range cfg.Routes {
		rt := &router.Route{Model: rc.Model, Strategy: rc.Strategy}
		for _, cc := range rc.Candidates {
			rt.Candidates = append(rt.Candidates, router.Candidate{
				Provider: byName[cc.Provider],
				Model:    cc.Model,
				Weight:   cc.Weight,
			})
		}
		routes = append(routes, rt)
	}
	prices := make(map[string]usage.Price, len(cfg.Prices))
	for m, pc := range cfg.Prices {
		prices[m] = usage.Price{PromptPer1M: pc.PromptPer1M, CompletionPer1M: pc.CompletionPer1M, EmbedPer1M: pc.EmbedPer1M, ContextTokens: pc.ContextTokens}
	}
	rt := router.New(provs, routes)
	rt.SetPrices(prices)
	for _, pc := range cfg.Providers {
		if pc.Circuit != nil {
			rt.SetCircuit(pc.Name, router.CircuitConfig{
				FailureThreshold: pc.Circuit.FailureThreshold,
				CooldownMs:       pc.Circuit.CooldownMs,
			})
		}
	}
	return &serverState{router: rt, prices: prices}, nil
}

// openDB opens the shared usage/auth DB; fatal at startup, skipped on reload failure.
func openDB(path string) (*sql.DB, error) {
	return usage.OpenDB(path)
}

// bindMetrics points the registry's circuit gauge at the live router.
// Called again on reload so the gauge follows the swapped router.
func bindMetrics(m *metrics.Registry, rt *router.Router) {
	m.Providers = func() []string {
		names := []string{}
		for _, p := range rt.Providers() {
			names = append(names, p.Name())
		}
		return names
	}
	m.CircuitOpen = func(name string) bool { return rt.CircuitState(name) == "open" }
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
	state, err := buildState(cfg)
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
	var current atomic.Pointer[serverState]
	current.Store(state)

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
			Cache:         st.cache,
			Metrics:       st.metrics,
			SeparateAdmin: cfg.AdminListen != "",
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
				Metrics: st.metrics,
			}).ServeHTTP(w, r)
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
				nstate, err := buildState(ncfg)
				if err != nil {
					log.Error("reload build state", "err", err)
					continue
				}
				prev := current.Load()
				nstate.usage = prev.usage
				nstate.keys = prev.keys
				nstate.limiter = prev.limiter
				nstate.adminKey = ncfg.AdminKey
				nstate.metrics = prev.metrics
				bindMetrics(prev.metrics, nstate.router)
				// Keep the warm cache unless reload toggled it off or changed TTL.
				nstate.cache = prev.cache
				if !ncfg.Cache.Enabled {
					nstate.cache = nil
				} else if nstate.cache == nil {
					nstate.cache = server.NewCache(true, ncfg.Cache.TTLSeconds)
				}
				current.Store(nstate)
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
