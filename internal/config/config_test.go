package config

import (
	"os"
	"path/filepath"
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
