// Package pricing — expression-based per-model billing (port of new-api
// pkg/billingexpr). One expression string per model, evaluated per request
// with token-detail variables; result is total USD cost.
//
// Variables (all float64 token counts):
//
//	p    billable prompt tokens (after normalization, see Normalize)
//	c    billable completion tokens
//	len  total input length for tier conditions (text + cache tokens)
//	cr   cached-read tokens (prompt_tokens_details.cached_tokens /
//	     anthropic cache_read_input_tokens)
//	cc   cache-creation tokens (anthropic cache_creation_input_tokens;
//	     [OI] cache_creation_tokens)
//	cc1h cache-creation tokens with 1h TTL (subset of cc when reported)
//	img  image input tokens
//	ai   audio input tokens, ao audio output tokens
//
// Coefficients are real USD per 1M tokens applied to raw token counts; the
// expression result is divided by 1e6 automatically so expressions read
// naturally, e.g. Claude long-context tiering:
//
//	p <= 200000 ? tier("standard", p*1.5 + c*7.5) : tier("long_context", p*3.0 + c*11.25)
package pricing

import (
	"fmt"
	"sync"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/vm"
)

// Env is the expression environment: raw token counts for one request.
// Call Normalize before Eval when p/c include cache/media tokens.
type Env struct {
	P    float64 `expr:"p"`
	C    float64 `expr:"c"`
	Len  float64 `expr:"len"`
	CR   float64 `expr:"cr"`
	CC   float64 `expr:"cc"`
	CC1h float64 `expr:"cc1h"`
	Img  float64 `expr:"img"`
	Ai   float64 `expr:"ai"`
	Ao   float64 `expr:"ao"`

	Tier string `expr:"-"` // recorded by tier(); not an expr variable
}

// tier records the matched tier name and returns the cost unchanged.
func (e *Env) tier(name string, cost float64) float64 {
	e.Tier = name
	return cost
}

func exprEnv(e *Env) map[string]any {
	return map[string]any{
		"p": e.P, "c": e.C, "len": e.Len,
		"cr": e.CR, "cc": e.CC, "cc1h": e.CC1h,
		"img": e.Img, "ai": e.Ai, "ao": e.Ao,
		"tier": e.tier,
	}
}

// cacheSize bounds the compiled-program cache (config-priced models are few;
// 256 covers pathological dynamic configs without unbounded growth).
const cacheSize = 256

var (
	cacheMu sync.Mutex
	cache   = map[string]*cachedExpr{}
)

type cachedExpr struct {
	prog     *vm.Program
	usedVars map[string]bool // referenced identifiers (drives normalization)
}

// Compile parses + type-checks an expression; result cached (bounded).
// UsedVars reports which detail variables the expression references so
// Normalize only subtracts tokens the expression prices separately.
func Compile(expression string) (*vm.Program, map[string]bool, error) {
	cacheMu.Lock()
	if ce, ok := cache[expression]; ok {
		cacheMu.Unlock()
		return ce.prog, ce.usedVars, nil
	}
	cacheMu.Unlock()

	prog, err := expr.Compile(expression, expr.Env(exprEnv(&Env{})), expr.AsFloat64())
	if err != nil {
		return nil, nil, fmt.Errorf("pricing expr: %w", err)
	}
	used := map[string]bool{}
	node := prog.Node()
	ast.Walk(&node, &varCollector{out: used})
	cacheMu.Lock()
	if len(cache) >= cacheSize {
		cache = map[string]*cachedExpr{} // crude eviction; expressions are few
	}
	cache[expression] = &cachedExpr{prog: prog, usedVars: used}
	cacheMu.Unlock()
	return prog, used, nil
}

type varCollector struct {
	out map[string]bool
}

func (v *varCollector) Visit(n *ast.Node) {
	if id, ok := (*n).(*ast.IdentifierNode); ok {
		v.out[id.Value] = true
	}
}

// Eval runs a compiled expression. Returns cost USD and the matched tier
// name ("" when the expression never calls tier()). The expression result
// (USD per 1M-token rates × raw counts) is divided by 1e6.
func Eval(prog *vm.Program, e *Env) (float64, string, error) {
	out, err := expr.Run(prog, exprEnv(e))
	if err != nil {
		return 0, "", fmt.Errorf("pricing eval: %w", err)
	}
	f, ok := out.(float64)
	if !ok {
		return 0, "", fmt.Errorf("pricing eval: result %T not float64", out)
	}
	return f / 1e6, e.Tier, nil
}

// Normalize adjusts p/c/len per new-api BuildTieredTokenParams:
//   - [OI] semantics: prompt_tokens includes cached/image/audio tokens.
//     When the expression references those vars, subtract them from p so
//     p stays plain-text billable; cr/cc/img are priced by their own terms.
//   - Anthropic semantics: input_tokens is text-only (cache tokens reported
//     separately); nothing subtracted — instead len = p + cr + cc so tier
//     conditions see the full input size.
//
// Completion side mirrors prompt for ao. Anthropic semantics are reported by
// the caller (the provider type is known at the call site).
func Normalize(e *Env, usedVars map[string]bool, anthropicSemantics bool) {
	if anthropicSemantics {
		e.Len = e.P + e.CR + e.CC
		return
	}
	sub := func(from *float64, v float64) {
		*from -= v
		if *from < 0 {
			*from = 0
		}
	}
	if usedVars["cr"] {
		sub(&e.P, e.CR)
	}
	if usedVars["cc"] || usedVars["cc1h"] {
		sub(&e.P, e.CC) // cc1h is a subset of cc
	}
	if usedVars["img"] {
		sub(&e.P, e.Img)
	}
	if usedVars["ai"] {
		sub(&e.P, e.Ai)
	}
	if usedVars["ao"] {
		sub(&e.C, e.Ao)
	}
	if e.Len == 0 {
		e.Len = e.P
	}
}
