// Package config loads and validates gateway YAML configuration.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
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
}

type CandidateConfig struct {
	Provider string   `yaml:"provider"`
	Model    string   `yaml:"model"`
	Weight   int      `yaml:"weight"` // weighted strategy; default 1
	Groups   []string `yaml:"groups"` // empty = all key groups allowed
	Tags     []string `yaml:"tags"`   // tag-routing labels (X-Route-Tags header); empty = matches all
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
	return nil
}

// validStrategy mirrors router strategy names; kept local to avoid an import.
func validStrategy(s string) bool {
	switch s {
	case "priority", "round_robin", "least_latency", "weighted", "cost", "lkgp", "headroom", "fusion",
		"p2c", "reset_aware", "fill_first", "auto":
		return true
	}
	return false
}
