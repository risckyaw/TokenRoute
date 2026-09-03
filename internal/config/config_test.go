package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const baseCfg = `
providers:
  - name: p1
    type: openai
    base_url: http://x
routes:
  - model: auto
    candidates:
      - provider: p1
        model: up
prices:
  up:
    expr: "EXPR_HERE"
`

func TestExprPriceValid(t *testing.T) {
	cfg := baseCfg[:0] // silence unused
	_ = cfg
	good := `'p <= 200000 ? tier("standard", p*1.5) : tier("long", p*3.0)'`
	p := writeCfg(t, replaceExpr(baseCfg, good))
	if _, err := Load(p); err != nil {
		t.Fatalf("valid expr rejected: %v", err)
	}
}

func TestExprPriceInvalidFailsLoad(t *testing.T) {
	p := writeCfg(t, replaceExpr(baseCfg, "'p * * 2'"))
	if _, err := Load(p); err == nil {
		t.Fatal("invalid expr must fail config load")
	}
}

func TestExprPriceUnknownVarFailsLoad(t *testing.T) {
	p := writeCfg(t, replaceExpr(baseCfg, "'bogus * 2'"))
	if _, err := Load(p); err == nil {
		t.Fatal("unknown variable must fail config load")
	}
}

func TestRetryPolicyValid(t *testing.T) {
	cfg := `
providers:
  - name: p1
    type: openai
    base_url: http://x
routes:
  - model: auto
    candidates:
      - {provider: p1, model: up}
retry_policy:
  retry_status_ranges: "100-199,300-399,401-407,409-499,500-503,505-599"
  never_retry: [504, 524]
  disable_status_ranges: "401"
  disable_keywords: ["insufficient balance", "account suspended"]
`
	if _, err := Load(writeCfg(t, cfg)); err != nil {
		t.Fatalf("valid retry_policy rejected: %v", err)
	}
}

func TestRetryPolicyInvalidFailsLoad(t *testing.T) {
	cfg := `
providers:
  - {name: p1, type: openai, base_url: http://x}
routes:
  - {model: auto, candidates: [{provider: p1, model: up}]}
retry_policy:
  retry_status_ranges: "abc"
`
	if _, err := Load(writeCfg(t, cfg)); err == nil {
		t.Fatal("bad range must fail config load")
	}
}

func replaceExpr(cfg, expr string) string {
	out := ""
	for _, line := range splitLines(cfg) {
		if line == `    expr: "EXPR_HERE"` {
			line = `    expr: ` + expr
		}
		out += line + "\n"
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func TestDecodeRejectsUnknownField(t *testing.T) {
	_, err := Decode([]byte("unknown_option: true\n"), false)
	if err == nil || !strings.Contains(err.Error(), "field unknown_option not found") {
		t.Fatalf("error = %v", err)
	}
}

func TestDecodeRejectsDuplicateKey(t *testing.T) {
	_, err := Decode([]byte("listen: :8400\nlisten: :9400\n"), false)
	if err == nil || !strings.Contains(err.Error(), "already defined") {
		t.Fatalf("error = %v", err)
	}
}

func TestDecodeRejectsTrailingYAMLDocuments(t *testing.T) {
	for _, trailing := range []string{"", "{}", "value", "null"} {
		t.Run(trailing, func(t *testing.T) {
			_, err := Decode([]byte("listen: :8400\n---\n"+trailing+"\n"), false)
			if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestLoadRejectsTrailingYAMLDocuments(t *testing.T) {
	for _, trailing := range []string{"", "{}", "value", "null"} {
		t.Run(trailing, func(t *testing.T) {
			_, err := Load(writeCfg(t, "listen: :8400\n---\n"+trailing+"\n"))
			if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestDecodeCanPreserveEnvironmentReferences(t *testing.T) {
	cfg, err := Decode([]byte("admin_key: ${GATEWAY_ADMIN_KEY}\n"), false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminKey != "${GATEWAY_ADMIN_KEY}" {
		t.Fatalf("admin_key = %q", cfg.AdminKey)
	}
}

// sticky is round_robin-only and must be non-negative.
func TestStickyConfig(t *testing.T) {
	tmpl := `
providers:
  - name: p1
    type: openai
    base_url: http://x
routes:
  - model: auto
    strategy: %s
    sticky: %d
    candidates:
      - provider: p1
        model: up
`
	if _, err := Load(writeCfg(t, fmt.Sprintf(tmpl, "round_robin", 5))); err != nil {
		t.Fatalf("valid sticky rejected: %v", err)
	}
	if _, err := Load(writeCfg(t, fmt.Sprintf(tmpl, "priority", 5))); err == nil {
		t.Fatal("sticky with a non-round_robin strategy must fail load")
	}
	if _, err := Load(writeCfg(t, fmt.Sprintf(tmpl, "round_robin", -1))); err == nil {
		t.Fatal("negative sticky must fail load")
	}
	// sticky: 1 on any strategy is the default and stays legal.
	if _, err := Load(writeCfg(t, fmt.Sprintf(tmpl, "priority", 1))); err != nil {
		t.Fatalf("sticky 1 rejected: %v", err)
	}
}

// failure_rules must be validated at load: a rule needs exactly one selector
// and exactly one cooldown source.
func TestFailureRulesConfig(t *testing.T) {
	tmpl := `
providers:
  - name: p1
    type: openai
    base_url: http://x
routes:
  - model: auto
    candidates:
      - provider: p1
        model: up
failure_rules:
%s
`
	good := `  - {match: "overloaded", cooldown_ms: 4000}
  - {match: "rate limit", backoff: true}
  - {status: 401, cooldown_ms: 120000}`
	cfg, err := Load(writeCfg(t, fmt.Sprintf(tmpl, good)))
	if err != nil {
		t.Fatalf("valid failure_rules rejected: %v", err)
	}
	fr, err := cfg.FailureRulesPolicy()
	if err != nil || fr == nil {
		t.Fatalf("policy = %v, %v; want a non-nil rule set", fr, err)
	}

	for name, rule := range map[string]string{
		"no selector":          `  - {cooldown_ms: 1000}`,
		"both selectors":       `  - {match: "x", status: 429, cooldown_ms: 1000}`,
		"no cooldown":          `  - {match: "x"}`,
		"cooldown and backoff": `  - {status: 429, cooldown_ms: 1000, backoff: true}`,
	} {
		if _, err := Load(writeCfg(t, fmt.Sprintf(tmpl, rule))); err == nil {
			t.Errorf("%s: must fail config load", name)
		}
	}

	// Unset failure_rules yields a nil policy (legacy behavior).
	bare, err := Load(writeCfg(t, `
providers:
  - name: p1
    type: openai
    base_url: http://x
routes:
  - model: auto
    candidates:
      - provider: p1
        model: up
`))
	if err != nil {
		t.Fatal(err)
	}
	if fr, err := bare.FailureRulesPolicy(); err != nil || fr != nil {
		t.Fatalf("unset policy = %v, %v; want nil, nil", fr, err)
	}
}

// fusion_judge is strategy-gated, needs >=2 candidates, and its judge must
// name a known provider as "provider/model".
func TestFusionJudgeConfigValidation(t *testing.T) {
	tmpl := `
providers:
  - name: p1
    type: openai
    base_url: http://x
  - name: p2
    type: openai
    base_url: http://y
routes:
  - model: panel
    strategy: %s
    fusion_judge: {judge: "%s", min_panel: 2, grace_ms: 1500, timeout_ms: 60000}
    candidates:
      - provider: p1
        model: a
      - provider: p2
        model: b
`
	if _, err := Load(writeCfg(t, fmt.Sprintf(tmpl, "fusion_judge", "p1/a"))); err != nil {
		t.Fatalf("valid fusion_judge rejected: %v", err)
	}
	// Judge may be a model not listed as a candidate, as long as the provider exists.
	if _, err := Load(writeCfg(t, fmt.Sprintf(tmpl, "fusion_judge", "p2/other"))); err != nil {
		t.Fatalf("off-panel judge rejected: %v", err)
	}
	if _, err := Load(writeCfg(t, fmt.Sprintf(tmpl, "priority", "p1/a"))); err == nil {
		t.Fatal("fusion_judge block on another strategy must fail load")
	}
	if _, err := Load(writeCfg(t, fmt.Sprintf(tmpl, "fusion_judge", "nope/a"))); err == nil {
		t.Fatal("judge naming an unknown provider must fail load")
	}
	if _, err := Load(writeCfg(t, fmt.Sprintf(tmpl, "fusion_judge", "bare-model"))); err == nil {
		t.Fatal("judge without provider/model form must fail load")
	}
	// Fewer than 2 candidates leaves nothing to fuse.
	single := `
providers:
  - name: p1
    type: openai
    base_url: http://x
routes:
  - model: panel
    strategy: fusion_judge
    candidates:
      - provider: p1
        model: a
`
	if _, err := Load(writeCfg(t, single)); err == nil {
		t.Fatal("fusion_judge with 1 candidate must fail load")
	}
	// The block is optional: defaults apply.
	noBlock := `
providers:
  - name: p1
    type: openai
    base_url: http://x
  - name: p2
    type: openai
    base_url: http://y
routes:
  - model: panel
    strategy: fusion_judge
    candidates:
      - provider: p1
        model: a
      - provider: p2
        model: b
`
	if _, err := Load(writeCfg(t, noBlock)); err != nil {
		t.Fatalf("fusion_judge without a config block rejected: %v", err)
	}
}

// balance_probe is opt-in and needs a url; negative knobs fail load.
func TestBalanceProbeConfig(t *testing.T) {
	tmpl := `
providers:
  - name: p1
    type: openai
    base_url: http://x
    balance_probe: %s
routes:
  - model: auto
    candidates:
      - provider: p1
        model: up
`
	cfg, err := Load(writeCfg(t, fmt.Sprintf(tmpl, `{url: "https://api.deepseek.com/user/balance", interval_ms: 300000, min_usd: 0.10}`)))
	if err != nil {
		t.Fatalf("valid balance_probe rejected: %v", err)
	}
	if bp := cfg.Providers[0].BalanceProbe; bp == nil || bp.MinUSD != 0.10 {
		t.Fatalf("balance_probe not parsed: %+v", cfg.Providers[0].BalanceProbe)
	}
	if _, err := Load(writeCfg(t, fmt.Sprintf(tmpl, `{interval_ms: 300000}`))); err == nil {
		t.Fatal("balance_probe without url must fail load")
	}
	if _, err := Load(writeCfg(t, fmt.Sprintf(tmpl, `{url: "http://x", min_usd: -1}`))); err == nil {
		t.Fatal("negative min_usd must fail load")
	}
	// Absent block = disabled (the default).
	bare, err := Load(writeCfg(t, `
providers:
  - name: p1
    type: openai
    base_url: http://x
routes:
  - model: auto
    candidates:
      - provider: p1
        model: up
`))
	if err != nil {
		t.Fatal(err)
	}
	if bare.Providers[0].BalanceProbe != nil {
		t.Fatal("balance_probe must be nil (disabled) by default")
	}
}
