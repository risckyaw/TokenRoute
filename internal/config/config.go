// Package config loads and validates gateway YAML configuration.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Jarvisagentic/tokenroute/internal/pricing"
	"github.com/Jarvisagentic/tokenroute/internal/router"
)

type CircuitConfig struct {
	FailureThreshold int `yaml:"failure_threshold"` // default 3
	CooldownMs       int `yaml:"cooldown_ms"`       // default 30000
	// AutoDisableAfter: after N open-transitions the breaker stays disabled
	// until manually re-enabled (default 3; 0 = never auto-disable).
	AutoDisableAfter int `yaml:"auto_disable_after"`
	// Mode "percent" trips on a failure RATIO in the current minute
	// (LiteLLM DEFAULT_FAILURE_THRESHOLD_PERCENT); default "consecutive".
	Mode           string  `yaml:"mode"`
	FailurePercent float64 `yaml:"failure_percent"` // default 0.5
	MinRequests    int     `yaml:"min_requests"`    // default 5
	// AllowedFails (LiteLLM per-exception allowed_fails): consecutive failures
	// tolerated per failure kind before opening; absent kinds use the global
	// threshold, auth/permission stay instant-open unless listed.
	// Names: auth, permission, rate_limit, quota_exhausted, timeout, server,
	// invalid_request, model_unavailable, network, unknown.
	AllowedFails map[string]int `yaml:"allowed_fails"`
}

// HealthCheckConfig controls per-provider background probes (LiteLLM
// background health checks): keeps circuit state and latency EMA warm.
type HealthCheckConfig struct {
	Enabled    bool   `yaml:"enabled"`     // default false
	IntervalMs int    `yaml:"interval_ms"` // default 60000
	Model      string `yaml:"model"`       // upstream model for the probe
}

type ProviderConfig struct {
	Name      string   `yaml:"name"`
	Type      string   `yaml:"type"`
	BaseURL   string   `yaml:"base_url"`
	APIKey    string   `yaml:"api_key"`
	APIKeys   []string `yaml:"api_keys"` // pool; api_key still works (pool of 1)
	Priority  int      `yaml:"priority"`
	TimeoutMs int      `yaml:"timeout_ms"`
	// ResponseHeaderTimeoutMs bounds the wait for upstream response headers
	// (0 = disabled; main.go applies the 900000 default when unset).
	// Streaming bodies unaffected.
	ResponseHeaderTimeoutMs int `yaml:"response_header_timeout_ms"`
	// StreamIdleTimeoutMs aborts a streaming relay after this long without
	// upstream bytes (0 = disabled; main.go applies the 300000 default).
	StreamIdleTimeoutMs int `yaml:"stream_idle_timeout_ms"`
	// ModelMapping rewrites candidate model names to upstream models,
	// applied after route resolution (alias -> upstream model).
	ModelMapping map[string]string `yaml:"model_mapping"`
	Circuit      *CircuitConfig    `yaml:"circuit"`
	// QuotaTokenLimit + QuotaWindowSeconds enable pre-request budget
	// awareness for this provider (per candidate model): strategies
	// reset_aware/fill_first/auto read the ledger; 0 = untracked.
	QuotaTokenLimit    int64 `yaml:"quota_token_limit"`
	QuotaWindowSeconds int   `yaml:"quota_window_seconds"` // default 60
	// HealthCheck enables a background probe goroutine (max_tokens=1 "hi"
	// chat through the normal provider path); results feed circuit/EMA only,
	// never the quota ledger or usage log.
	HealthCheck *HealthCheckConfig `yaml:"health_check"`
	// ParamOverride/ParamDelete/HeaderOverride/HeaderPass (severely reduced
	// new-api override port): set-only JSON body ops applied after model
	// rewrite (candidate-level param_override wins on key conflict);
	// header_override sets upstream request headers; header_pass forwards
	// client headers (glob, case-insensitive) bypassing filterHeaders.
	ParamOverride  map[string]any   `yaml:"param_override"`
	ParamDelete    []string         `yaml:"param_delete"`
	HeaderOverride map[string]string `yaml:"header_override"`
	HeaderPass     []string         `yaml:"header_pass"`
}

type CandidateConfig struct {
	Provider string   `yaml:"provider"`
	Model    string   `yaml:"model"`
	Weight   int      `yaml:"weight"` // weighted strategy; default 1
	Groups   []string `yaml:"groups"` // empty = all key groups allowed
	Tags     []string `yaml:"tags"`   // tag-routing labels (X-Route-Tags header); empty = matches all
	// ParamOverride is applied after the provider-level param ops and wins
	// on key conflict (set-only).
	ParamOverride map[string]any `yaml:"param_override"`
}

type RouteConfig struct {
	Model      string            `yaml:"model"`
	Strategy   string            `yaml:"strategy"`   // priority|round_robin|least_latency|weighted|cost|lkgp|headroom
	Multiplier float64           `yaml:"multiplier"` // cost multiplier; default 1.0
	Candidates []CandidateConfig `yaml:"candidates"`
	// FallbackRoutes (LiteLLM fallbacks): other virtual models tried, in
	// order, when every candidate of this route fails retryably. Max 3 route
	// hops; cycles skipped. Client errors (400/401/403) never trigger it.
	FallbackRoutes []string `yaml:"fallback_routes"`
	// PromptCacheAffinity pins cacheable prompt prefixes to the provider that
	// served them (provider-side prompt cache hits); overrides global default.
	// Shorthand for Affinity{Enabled: true} with prefix hashing.
	PromptCacheAffinity bool `yaml:"prompt_cache_affinity"`
	// Affinity (new-api channel affinity port): extended pin config. Wins
	// over PromptCacheAffinity when enabled is set.
	Affinity *AffinityConfig `yaml:"affinity"`
	// HashOn (consistent_hash strategy only): request value to hash.
	// "header:X-Session-Id" (any header name) or "key" (virtual API key).
	// Missing value -> priority order fallback.
	HashOn string `yaml:"hash_on"`
}

// AffinityConfig extends prompt-cache affinity: pin key source (header value
// hash vs prompt-prefix hash), TTL, and skip_retry_on_failure (a pinned
// request that fails must NOT fail over — state lives on that channel).
type AffinityConfig struct {
	Enabled   bool   `yaml:"enabled"`
	KeyHeader string `yaml:"key_header"` // e.g. "X-Session-Id"; empty = prefix hashing
	TTLMs     int    `yaml:"ttl_ms"`     // default 3600000
	// SkipRetryOnFailure: on an affinity HIT, relay upstream failures to the
	// client instead of failing over (stateful channel; new-api port).
	SkipRetryOnFailure bool `yaml:"skip_retry_on_failure"`
}

// FreeTierConfig is one free-tier budget entry: provider+model gets
// monthly_tokens per 30-day window (0 = skip).
type FreeTierConfig struct {
	Provider      string `yaml:"provider"`
	Model         string `yaml:"model"`
	MonthlyTokens int64  `yaml:"monthly_tokens"`
}

type PriceConfig struct {
	PromptPer1M     float64 `yaml:"prompt_per_1m"`
	CompletionPer1M float64 `yaml:"completion_per_1m"`
	EmbedPer1M      float64 `yaml:"embed_per_1m"`   // optional; falls back to prompt_per_1m
	ContextTokens   int     `yaml:"context_tokens"` // optional; 0 = no context guard
	// Expr (new-api billingexpr port): one expression evaluated per request
	// with token variables p,c,len,cr,cc,cc1h,img,ai,ao; coefficients are USD
	// per 1M tokens and the result is auto-divided by 1e6. Wins over the flat
	// fields for chat cost. Invalid expressions fail config load.
	Expr string `yaml:"expr"`
}

// CacheConfig controls the in-memory semantic response cache.
type CacheConfig struct {
	Enabled    bool `yaml:"enabled"`
	TTLSeconds int  `yaml:"ttl_seconds"` // default 300
}

// SearchConfig wires one web-search backend for POST /v1/search.
type SearchConfig struct {
	Backend string   `yaml:"backend"` // tavily | brave | exa
	APIKey  string   `yaml:"api_key"`
	APIKeys []string `yaml:"api_keys"` // pool; api_key appended when set
}

type Config struct {
	Listen      string      `yaml:"listen"`
	AdminListen string      `yaml:"admin_listen"` // optional dedicated admin listener
	UsageDB     string      `yaml:"usage_db"`
	AdminKey    string      `yaml:"admin_key"`
	MaxBodyMB   int         `yaml:"max_body_mb"` // request body cap; default 10
	Cache       CacheConfig `yaml:"cache"`
	// ModelCatalog controls daily models.dev capability sync: "off"
	// disables; empty/other enables with the cache beside usage_db.
	ModelCatalog string `yaml:"model_catalog"`
	// PricingSync controls the LiteLLM pricing sync: "off" disables;
	// empty/other enables with a 24h refresh. Synced prices fill gaps only —
	// config `prices:` entries always win (OmniRoute resolution order).
	PricingSync string `yaml:"pricing_sync"`
	// Search lists web-search backends for POST /v1/search, tried in order.
	Search    []SearchConfig         `yaml:"search"`
	Providers []ProviderConfig       `yaml:"providers"`
	Routes    []RouteConfig          `yaml:"routes"`
	Prices    map[string]PriceConfig `yaml:"prices"`
	// FreeTier catalogs per (provider, model) monthly free-token budgets
	// (OmniRoute freeModelCatalog): seeds the quota ledger with monthly
	// windows so fill_first/reset_aware/auto prefer live free tiers.
	FreeTier []FreeTierConfig `yaml:"free_tier"`
	// Aliases maps client-facing model names to route (virtual) models,
	// resolved BEFORE route lookup (OmniRoute modelAliasResolver):
	// "deepseek-chat" -> "ds/deepseek-v4-flash". Per-provider model_mapping
	// still applies after route resolution.
	Aliases map[string]string `yaml:"aliases"`
	// PromptCacheAffinity (global default, routes may override): pin
	// cacheable prompt prefixes to the serving provider+model for 1h.
	PromptCacheAffinity bool `yaml:"prompt_cache_affinity"`
	// HealthCheck is the global background-probe default; a provider's own
	// health_check block wins entirely.
	HealthCheck *HealthCheckConfig `yaml:"health_check"`
	// RetryPolicy overrides failover/disable classification (new-api port).
	// Unset = current hardcoded behavior exactly.
	RetryPolicy *RetryPolicyConfig `yaml:"retry_policy"`
	// GroupRatio (new-api group_ratio port): group name -> cost multiplier.
	// Effective multiplier = route.multiplier × product of ratios for groups
	// in key∩candidate intersection; empty intersection = 1.0. Unset =
	// current behavior. Cost-only; routing/filtering unaffected.
	GroupRatio map[string]float64 `yaml:"group_ratio"`
}

// RetryPolicyConfig (new-api status_code_ranges + AutomaticDisableKeywords):
// retry_status_ranges decides failover (never_retry wins), disable ranges +
// keywords classify a failure as auth/quota so the circuit opens fast.
type RetryPolicyConfig struct {
	RetryStatusRanges   string   `yaml:"retry_status_ranges"`   // e.g. "100-199,300-399,401-407,409-499,500-503,505-599"
	NeverRetry          []int    `yaml:"never_retry"`           // e.g. [504, 524]
	DisableStatusRanges string   `yaml:"disable_status_ranges"` // e.g. "401"
	DisableKeywords     []string `yaml:"disable_keywords"`      // case-insensitive body substrings
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	for i := range cfg.Providers {
		cfg.Providers[i].BaseURL = expandIfPresent(cfg.Providers[i].BaseURL)
		cfg.Providers[i].APIKey = expandIfPresent(cfg.Providers[i].APIKey)
		for j := range cfg.Providers[i].APIKeys {
			cfg.Providers[i].APIKeys[j] = expandIfPresent(cfg.Providers[i].APIKeys[j])
		}
	}
	cfg.AdminKey = expandIfPresent(cfg.AdminKey)
	for i := range cfg.Search {
		cfg.Search[i].APIKey = expandIfPresent(cfg.Search[i].APIKey)
		for j := range cfg.Search[i].APIKeys {
			cfg.Search[i].APIKeys[j] = expandIfPresent(cfg.Search[i].APIKeys[j])
		}
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8400"
	}
	if cfg.UsageDB == "" {
		cfg.UsageDB = "data/usage.db"
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// expandIfPresent expands $VAR / ${VAR}; empty expansion stays empty
// (api_key may legitimately be empty, e.g. Ollama).
func expandIfPresent(s string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	return os.ExpandEnv(s)
}

func (c *Config) Validate() error {
	seen := map[string]bool{}
	for _, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("provider with empty name")
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate provider name: %s", p.Name)
		}
		seen[p.Name] = true
	}
	routeModels := map[string]bool{}
	for _, r := range c.Routes {
		routeModels[r.Model] = true
	}
	for _, r := range c.Routes {
		if r.Model == "" {
			return fmt.Errorf("route with empty model")
		}
		if r.Strategy != "" && !validStrategy(r.Strategy) {
			return fmt.Errorf("route %q has unknown strategy %q", r.Model, r.Strategy)
		}
		if r.HashOn != "" && r.HashOn != "key" && !strings.HasPrefix(r.HashOn, "header:") {
			return fmt.Errorf("route %q has invalid hash_on %q (want \"key\" or \"header:Name\")", r.Model, r.HashOn)
		}
		if r.Strategy == "consistent_hash" && r.HashOn == "" {
			return fmt.Errorf("route %q uses consistent_hash without hash_on", r.Model)
		}
		for _, cand := range r.Candidates {
			if !seen[cand.Provider] {
				return fmt.Errorf("route %q references unknown provider %q", r.Model, cand.Provider)
			}
			if cand.Model == "" {
				return fmt.Errorf("route %q has candidate with empty model", r.Model)
			}
			if cand.Weight < 0 {
				return fmt.Errorf("route %q has candidate with negative weight", r.Model)
			}
		}
		for _, fb := range r.FallbackRoutes {
			if !routeModels[fb] {
				return fmt.Errorf("route %q fallback_routes references unknown model %q", r.Model, fb)
			}
		}
	}
	for m, p := range c.Prices {
		if p.Expr != "" {
			if _, _, err := pricing.Compile(p.Expr); err != nil {
				return fmt.Errorf("prices %q: %w", m, err)
			}
		}
	}
	if rp := c.RetryPolicy; rp != nil {
		if _, err := router.NewRetryPolicy(rp.RetryStatusRanges, rp.DisableStatusRanges, rp.NeverRetry, rp.DisableKeywords); err != nil {
			return fmt.Errorf("retry_policy: %w", err)
		}
	}
	return nil
}

// validStrategy mirrors router strategy names; kept local to avoid an import.
func validStrategy(s string) bool {
	switch s {
	case "priority", "round_robin", "least_latency", "weighted", "cost", "lkgp", "headroom", "fusion",
		"p2c", "reset_aware", "fill_first", "auto", "lowest_usage", "peak_ewma", "least_connections",
		"consistent_hash":
		return true
	}
	return false
}
