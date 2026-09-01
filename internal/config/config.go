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
}

type ProviderConfig struct {
	Name      string         `yaml:"name"`
	Type      string         `yaml:"type"`
	BaseURL   string         `yaml:"base_url"`
	APIKey    string         `yaml:"api_key"`
	Priority  int            `yaml:"priority"`
	TimeoutMs int            `yaml:"timeout_ms"`
	Circuit   *CircuitConfig `yaml:"circuit"`
}

type CandidateConfig struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	Weight   int    `yaml:"weight"` // weighted strategy; default 1
}

type RouteConfig struct {
	Model      string            `yaml:"model"`
	Strategy   string            `yaml:"strategy"` // priority|round_robin|least_latency|weighted|cost
	Candidates []CandidateConfig `yaml:"candidates"`
}

type PriceConfig struct {
	PromptPer1M     float64 `yaml:"prompt_per_1m"`
	CompletionPer1M float64 `yaml:"completion_per_1m"`
}

type Config struct {
	Listen    string                 `yaml:"listen"`
	UsageDB   string                 `yaml:"usage_db"`
	AdminKey  string                 `yaml:"admin_key"`
	Providers []ProviderConfig       `yaml:"providers"`
	Routes    []RouteConfig          `yaml:"routes"`
	Prices    map[string]PriceConfig `yaml:"prices"`
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
	}
	cfg.AdminKey = expandIfPresent(cfg.AdminKey)
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
	}
	return nil
}

// validStrategy mirrors router strategy names; kept local to avoid an import.
func validStrategy(s string) bool {
	switch s {
	case "priority", "round_robin", "least_latency", "weighted", "cost":
		return true
	}
	return false
}
