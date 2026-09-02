package pricing

import (
	"math"
	"testing"
)

func eval(t *testing.T, expression string, e *Env, anthropic bool) (float64, string) {
	t.Helper()
	prog, used, err := Compile(expression)
	if err != nil {
		t.Fatalf("compile %q: %v", expression, err)
	}
	Normalize(e, used, anthropic)
	cost, tier, err := Eval(prog, e)
	if err != nil {
		t.Fatalf("eval %q: %v", expression, err)
	}
	return cost, tier
}

const claudeExpr = `p <= 200000 ? tier("standard", p*1.5 + c*7.5) : tier("long_context", p*3.0 + c*11.25)`

func TestClaudeTierBoundary(t *testing.T) {
	// Exactly at the boundary: standard tier.
	cost, tier := eval(t, claudeExpr, &Env{P: 200000, C: 100}, false)
	if tier != "standard" {
		t.Fatalf("tier = %q, want standard", tier)
	}
	want := (200000*1.5 + 100*7.5) / 1e6
	if math.Abs(cost-want) > 1e-12 {
		t.Fatalf("cost = %v, want %v", cost, want)
	}
	// One over: long_context tier.
	cost, tier = eval(t, claudeExpr, &Env{P: 200001, C: 100}, false)
	if tier != "long_context" {
		t.Fatalf("tier = %q, want long_context", tier)
	}
	want = (200001*3.0 + 100*11.25) / 1e6
	if math.Abs(cost-want) > 1e-12 {
		t.Fatalf("cost = %v, want %v", cost, want)
	}
}

func TestLenDrivesTierCondition(t *testing.T) {
	// new-api style: tier decided on total input (len), cost on billable p.
	expr := `len <= 1000 ? tier("short", p*1.0) : tier("long", p*2.0)`
	_, tier := eval(t, expr, &Env{P: 500, CR: 600}, true) // anthropic: len = 500+600
	if tier != "long" {
		t.Fatalf("tier = %q, want long (len includes cache)", tier)
	}
}

func TestCacheReadSubtraction(t *testing.T) {
	// [OI] semantics: prompt_tokens includes cached tokens; an expr pricing
	// cr separately must see p reduced by cr.
	expr := `p*2.0 + c*8.0 + cr*0.2`
	cost, _ := eval(t, expr, &Env{P: 1000, C: 100, CR: 400}, false)
	want := (600*2.0 + 100*8.0 + 400*0.2) / 1e6
	if math.Abs(cost-want) > 1e-12 {
		t.Fatalf("cost = %v, want %v", cost, want)
	}
}

func TestNoSubtractionWhenVarUnused(t *testing.T) {
	// cr present but not referenced: p untouched.
	expr := `p*2.0 + c*8.0`
	cost, _ := eval(t, expr, &Env{P: 1000, C: 100, CR: 400}, false)
	want := (1000*2.0 + 100*8.0) / 1e6
	if math.Abs(cost-want) > 1e-12 {
		t.Fatalf("cost = %v, want %v", cost, want)
	}
}

func TestAnthropicNoSubtraction(t *testing.T) {
	// Anthropic: input_tokens is text-only; cr/cc reported separately and
	// must NOT be subtracted from p — only len grows.
	expr := `p*3.0 + cr*0.3 + cc*3.75`
	cost, _ := eval(t, expr, &Env{P: 1000, C: 0, CR: 400, CC: 200}, true)
	want := (1000*3.0 + 400*0.3 + 200*3.75) / 1e6
	if math.Abs(cost-want) > 1e-12 {
		t.Fatalf("cost = %v, want %v", cost, want)
	}
}

func TestClampAtZero(t *testing.T) {
	// cr > p must not produce negative billable tokens.
	expr := `p*2.0 + cr*0.2`
	cost, _ := eval(t, expr, &Env{P: 100, CR: 500}, false)
	want := (0.0 + 500*0.2) / 1e6
	if math.Abs(cost-want) > 1e-12 {
		t.Fatalf("cost = %v, want %v", cost, want)
	}
}

func TestNoTierCall(t *testing.T) {
	cost, tier := eval(t, `p*1.0 + c*2.0`, &Env{P: 100, C: 50}, false)
	if tier != "" {
		t.Fatalf("tier = %q, want empty", tier)
	}
	if math.Abs(cost-(100*1.0+50*2.0)/1e6) > 1e-12 {
		t.Fatalf("cost = %v", cost)
	}
}

func TestCompileInvalid(t *testing.T) {
	if _, _, err := Compile(`p * * 2`); err == nil {
		t.Fatal("expected compile error")
	}
	if _, _, err := Compile(`nonsense_var * 2`); err == nil {
		t.Fatal("expected unknown-name error")
	}
}

func TestCompileCaches(t *testing.T) {
	p1, _, err := Compile(`p*1.0`)
	if err != nil {
		t.Fatal(err)
	}
	p2, _, err := Compile(`p*1.0`)
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Fatal("same expression must return the cached program")
	}
}
