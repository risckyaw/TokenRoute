// Package router maps virtual model names to provider candidates.
package router

import (
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// Strategy names for per-route candidate ordering.
const (
	StrategyPriority     = "priority"
	StrategyRoundRobin   = "round_robin"
	StrategyLeastLatency = "least_latency"
	StrategyWeighted     = "weighted"
	StrategyCost         = "cost"
	StrategyLKGP         = "lkgp" // last-known-good provider first
	StrategyHeadroom     = "headroom"
	StrategyFusion       = "fusion"      // race first 2 candidates concurrently
	StrategyP2C          = "p2c"         // power-of-2-choices: pick 2 random, least-loaded wins
	StrategyResetAware   = "reset_aware" // prefer candidates whose quota window resets soonest
	StrategyFillFirst    = "fill_first"  // exhaust first candidate's quota before moving on
	StrategyAuto         = "auto"        // composite multi-factor scoring (OmniRoute port)
)

// ValidStrategy reports whether s is a known strategy name.
func ValidStrategy(s string) bool {
	switch s {
	case StrategyPriority, StrategyRoundRobin, StrategyLeastLatency,
		StrategyWeighted, StrategyCost, StrategyLKGP, StrategyHeadroom, StrategyFusion,
		StrategyP2C, StrategyResetAware, StrategyFillFirst, StrategyAuto:
		return true
	}
	return false
}

const emaAlpha = 0.3

type Candidate struct {
	Provider provider.Provider
	Model    string
	Weight   int      // used by the weighted strategy; defaults to 1
	Groups   []string // empty = usable by every key group
	Tags     []string // tag-routing labels; empty = matches all plain/! selectors
}

type Route struct {
	Model      string
	Strategy   string
	Multiplier float64     // cost multiplier; 0 -> 1.0
	Candidates []Candidate // sorted by Provider.Priority() ascending
	// FallbackRoutes: other virtual models tried when every candidate here
	// fails retryably (LiteLLM fallbacks); resolved by name at request time.
	FallbackRoutes []string
	// PromptCacheAffinity pins requests with a cacheable prefix to the
	// provider+model that served that prefix (overrides global default).
	PromptCacheAffinity bool

	rr       atomic.Uint64 // round-robin counter
	lastGood atomic.Value  // string: provider name that served last success (lkgp)
	mu       sync.Mutex    // guards rand source for weighted
	randSrc  *rand.Rand
	tags     atomic.Value  // *TagSelector: request-scoped, set via WithTags
}

// WithTags returns a shallow copy of the route carrying a request-scoped tag
// selector; OrderCandidates filters on it (the shared route stays selector-free).
func (rt *Route) WithTags(sel *TagSelector) *Route {
	if sel == nil {
		return rt
	}
	cp := *rt
	cp.tags.Store(sel)
	return &cp
}

// TagSelector returns the request-scoped selector (nil = no filtering).
func (rt *Route) TagSelector() *TagSelector {
	if sel, ok := rt.tags.Load().(*TagSelector); ok {
		return sel
	}
	return nil
}

// window is a 60s tumbling counter used by the headroom strategy.
type window struct {
	start time.Time
	reqs  int
	toks  int
}

type Router struct {
	providers []provider.Provider // sorted by priority ascending
	routes    []*Route
	// AffinityDefault is the global prompt_cache_affinity default; a route's
	// own flag wins (Route.PromptCacheAffinity || AffinityDefault).
	AffinityDefault bool
	byName    map[string]provider.Provider
	circuits  map[string]*CircuitBreaker
	latency   map[string]float64 // EMA latency ms per provider name
	errRate   map[string]float64 // EMA error rate 0..1 per provider name
	prices    map[string]usage.Price
	mappings  map[string]map[string]string // provider name -> alias -> upstream model
	priceMu   sync.RWMutex                 // guards prices
	latMu     sync.Mutex
	lockMu    sync.Mutex
	modelLock map[string]time.Time // provider|model -> locked until
	windows   map[string]*window   // provider name -> 60s tumbling counters
	quota     *QuotaLedger         // pre-request budget awareness (nil-safe)
	aliases   map[string]string    // client model name -> virtual route model
	affinity  *AffinityCache       // prompt-prefix pinning (nil = disabled)
}

// SetAffinity enables prompt-cache affinity pinning (nil disables).
func (r *Router) SetAffinity(a *AffinityCache) {
	r.affinity = a
}

// Affinity returns the pin cache (nil when disabled) — for tests/server.
func (r *Router) Affinity() *AffinityCache {
	return r.affinity
}

// PinByAffinity reorders allowed candidates so the pinned provider+model
// goes first when the prefix hash has a live pin. Returns true on a hit.
// The pin must still pass circuit/lock filters (it reorders the already-
// filtered list, so a filtered-out pin just falls through to normal order).
func (r *Router) PinByAffinity(allowed []Candidate, hash uint64) bool {
	if r.affinity == nil || hash == 0 || len(allowed) < 2 {
		return false
	}
	pin, ok := r.affinity.Get(hash)
	if !ok {
		return false
	}
	for i, c := range allowed {
		if c.Provider.Name() == pin.Provider && c.Model == pin.Model {
			if i == 0 {
				return true
			}
			rest := append([]Candidate(nil), allowed[:i]...)
			rest = append(rest, allowed[i+1:]...)
			copy(allowed, append([]Candidate{allowed[i]}, rest...))
			return true
		}
	}
	return false
}

// RecordAffinity stores prefix -> provider+model after a successful response.
func (r *Router) RecordAffinity(hash uint64, providerName, model string) {
	if r.affinity != nil {
		r.affinity.Put(hash, providerName, model)
	}
}

func New(providers []provider.Provider, routes []*Route) *Router {
	sorted := append([]provider.Provider(nil), providers...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Priority() < sorted[j].Priority() })
	byName := make(map[string]provider.Provider, len(sorted))
	for _, p := range sorted {
		byName[p.Name()] = p
	}
	r := &Router{
		providers: sorted,
		routes:    routes,
		byName:    byName,
		circuits:  map[string]*CircuitBreaker{},
		latency:   map[string]float64{},
		errRate:   map[string]float64{},
		modelLock: map[string]time.Time{},
		windows:   map[string]*window{},
		quota:     NewQuotaLedger(),
	}
	for _, rt := range routes {
		if rt.Strategy == "" {
			rt.Strategy = StrategyPriority
		}
		if rt.Multiplier == 0 {
			rt.Multiplier = 1.0
		}
		for i := range rt.Candidates {
			if rt.Candidates[i].Weight <= 0 {
				rt.Candidates[i].Weight = 1
			}
		}
		sort.SliceStable(rt.Candidates, func(i, j int) bool {
			return rt.Candidates[i].Provider.Priority() < rt.Candidates[j].Provider.Priority()
		})
		rt.randSrc = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return r
}

// SetCircuit installs a circuit breaker for a provider (by name).
func (r *Router) SetCircuit(providerName string, cfg CircuitConfig) {
	r.circuits[providerName] = NewCircuitBreaker(cfg)
}

// SetAliases installs the global client-name -> virtual-model alias map
// (resolved before route lookup).
func (r *Router) SetAliases(aliases map[string]string) {
	r.aliases = aliases
}

// ResolveAlias maps a client-facing model name to its virtual route model
// (identity when unaliased).
func (r *Router) ResolveAlias(model string) string {
	if r.aliases != nil {
		if target, ok := r.aliases[model]; ok && target != "" {
			return target
		}
	}
	return model
}

// SetPrices installs the price map used by the cost and auto strategies.
func (r *Router) SetPrices(prices map[string]usage.Price) {
	r.priceMu.Lock()
	r.prices = prices
	r.priceMu.Unlock()
}

// price returns a model's price (RLock; pricing sync may swap the map).
func (r *Router) price(model string) (usage.Price, bool) {
	r.priceMu.RLock()
	defer r.priceMu.RUnlock()
	p, ok := r.prices[model]
	return p, ok
}

// SetModelMapping installs a provider's alias -> upstream-model map
// (applied after route resolution, before the provider call).
func (r *Router) SetModelMapping(providerName string, m map[string]string) {
	if len(m) == 0 {
		return
	}
	if r.mappings == nil {
		r.mappings = map[string]map[string]string{}
	}
	r.mappings[providerName] = m
}

// MapModel resolves a provider's model alias to its upstream model
// (identity when unmapped).
func (r *Router) MapModel(providerName, model string) string {
	if m, ok := r.mappings[providerName]; ok {
		if up, ok := m[model]; ok {
			return up
		}
	}
	return model
}

// circuitAllow reports whether the provider's circuit permits a request
// (true when no breaker configured).
func (r *Router) circuitAllow(name string) bool {
	cb, ok := r.circuits[name]
	return !ok || cb.Allow()
}

// LockModel marks provider+model as unusable until d elapses (e.g. upstream
// 429/404 for that model). Does not touch the provider circuit breaker.
func (r *Router) LockModel(providerName, model string, d time.Duration) {
	r.lockMu.Lock()
	r.modelLock[providerName+"|"+model] = time.Now().Add(d)
	r.lockMu.Unlock()
}

// ModelLockUntil returns when the provider+model lock expires
// (zero when not locked).
func (r *Router) ModelLockUntil(providerName, model string) time.Time {
	r.lockMu.Lock()
	defer r.lockMu.Unlock()
	until, ok := r.modelLock[providerName+"|"+model]
	if !ok {
		return time.Time{}
	}
	if time.Now().After(until) {
		delete(r.modelLock, providerName+"|"+model)
		return time.Time{}
	}
	return until
}

// IsModelLocked reports whether provider+model is currently locked out.
func (r *Router) IsModelLocked(providerName, model string) bool {
	r.lockMu.Lock()
	defer r.lockMu.Unlock()
	until, ok := r.modelLock[providerName+"|"+model]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(r.modelLock, providerName+"|"+model)
		return false
	}
	return true
}

// OrderCandidates returns the route's candidates ordered per its strategy,
// excluding providers whose circuit is open (half-open allows the probe)
// and candidates under a per-model lockout.
func (r *Router) OrderCandidates(rt *Route) []Candidate {
	sel := rt.TagSelector()
	allowed := make([]Candidate, 0, len(rt.Candidates))
	for _, c := range rt.Candidates {
		if !sel.MatchTags(c.Tags) {
			continue
		}
		if r.circuitAllow(c.Provider.Name()) && !r.IsModelLocked(c.Provider.Name(), c.Model) {
			allowed = append(allowed, c)
		}
	}
	if rt.Strategy == StrategyLKGP {
		// Last-known-good provider first, rest in priority order.
		if lg, ok := rt.lastGood.Load().(string); ok && lg != "" {
			for i, c := range allowed {
				if c.Provider.Name() == lg {
					rest := append([]Candidate(nil), allowed[:i]...)
					rest = append(rest, allowed[i+1:]...)
					allowed = append([]Candidate{allowed[i]}, rest...)
					break
				}
			}
		}
		return allowed
	}
	switch rt.Strategy {
	case StrategyRoundRobin:
		if len(allowed) > 1 {
			start := int(rt.rr.Add(1)-1) % len(allowed)
			rotated := append([]Candidate(nil), allowed[start:]...)
			allowed = append(rotated, allowed[:start]...)
		}
	case StrategyLeastLatency:
		r.latMu.Lock()
		lat := make(map[string]float64, len(r.latency))
		for k, v := range r.latency {
			lat[k] = v
		}
		r.latMu.Unlock()
		sort.SliceStable(allowed, func(i, j int) bool {
			li, lj := lat[allowed[i].Provider.Name()], lat[allowed[j].Provider.Name()]
			return li < lj // unseen providers (EMA=0) first
		})
	case StrategyWeighted:
		if len(allowed) > 1 {
			total := 0
			for _, c := range allowed {
				total += c.Weight
			}
			rt.mu.Lock()
			n := rt.randSrc.Intn(total)
			rt.mu.Unlock()
			pick := 0
			for i, c := range allowed {
				n -= c.Weight
				if n < 0 {
					pick = i
					break
				}
			}
			rest := append([]Candidate(nil), allowed[:pick]...)
			rest = append(rest, allowed[pick+1:]...)
			allowed = append([]Candidate{allowed[pick]}, rest...)
		}
	case StrategyCost:
		sort.SliceStable(allowed, func(i, j int) bool {
			return r.costKey(allowed[i]) < r.costKey(allowed[j])
		})
	case StrategyHeadroom:
		r.latMu.Lock()
		counts := make(map[string]int, len(r.windows))
		for name, w := range r.windows {
			counts[name] = w.reqs
		}
		r.latMu.Unlock()
		// Stable sort: ties keep priority order.
		sort.SliceStable(allowed, func(i, j int) bool {
			return counts[allowed[i].Provider.Name()] < counts[allowed[j].Provider.Name()]
		})
	case StrategyP2C:
		// Power-of-2-choices (OmniRoute p2c): draw two distinct candidates at
		// random, put the least-loaded first, keep the rest in priority order.
		if len(allowed) > 1 {
			r.latMu.Lock()
			counts := make(map[string]int, len(r.windows))
			for name, w := range r.windows {
				counts[name] = w.reqs
			}
			r.latMu.Unlock()
			rt.mu.Lock()
			if rt.randSrc == nil {
				rt.randSrc = rand.New(rand.NewSource(time.Now().UnixNano()))
			}
			i := rt.randSrc.Intn(len(allowed))
			j := rt.randSrc.Intn(len(allowed) - 1)
			if j >= i {
				j++
			}
			rt.mu.Unlock()
			winner, loser := allowed[i], allowed[j]
			if counts[loser.Provider.Name()] < counts[winner.Provider.Name()] {
				winner, loser = loser, winner
			}
			rest := make([]Candidate, 0, len(allowed)-1)
			for k, c := range allowed {
				if k != i && k != j {
					rest = append(rest, c)
				}
			}
			allowed = append([]Candidate{winner, loser}, rest...)
		}
	case StrategyResetAware:
		// Prefer candidates whose quota window resets soonest; unknown quota
		// sorts last (no signal). Ties keep priority order.
		sort.SliceStable(allowed, func(i, j int) bool {
			ri := r.quota.WindowReset(allowed[i].Provider.Name(), allowed[i].Model)
			rj := r.quota.WindowReset(allowed[j].Provider.Name(), allowed[j].Model)
			if ri.IsZero() && rj.IsZero() {
				return false
			}
			if ri.IsZero() {
				return false
			}
			if rj.IsZero() {
				return true
			}
			return ri.Before(rj)
		})
	case StrategyFillFirst:
		// Keep priority order but sink candidates whose quota window is
		// exhausted (remaining <= 0) behind those with budget left — the first
		// candidate keeps serving until its quota runs out (OmniRoute
		// fill-first: maximize one free tier before touching the next).
		sort.SliceStable(allowed, func(i, j int) bool {
			_, ri, ki := r.quota.Remaining(allowed[i].Provider.Name(), allowed[i].Model)
			_, rj, kj := r.quota.Remaining(allowed[j].Provider.Name(), allowed[j].Model)
			ei := ki && ri <= 0
			ej := kj && rj <= 0
			if ei == ej {
				return false
			}
			return !ei // non-exhausted first
		})
	case StrategyAuto:
		// Composite multi-factor score, ported from OmniRoute
		// adaptiveRouting.ts: product of clamped 0..1 factors so a single
		// zero disqualifies. Factors: health (1 - errRate), latency, cost,
		// quota headroom. Circuit open / model lock already filtered above.
		r.latMu.Lock()
		lat := make(map[string]float64, len(r.latency))
		for k, v := range r.latency {
			lat[k] = v
		}
		errs := make(map[string]float64, len(r.errRate))
		for k, v := range r.errRate {
			errs[k] = v
		}
		r.latMu.Unlock()
		type scored struct {
			c     Candidate
			score float64
		}
		scores := make([]scored, 0, len(allowed))
		for _, c := range allowed {
			name := c.Provider.Name()
			health := 1 - clamp01(errs[name])
			latencyF := 1.0
			if l, ok := lat[name]; ok && l > 0 {
				latencyF = 1 - minF(l/50_000, 0.6) // 50s+ -> floor 0.4
			}
			costF := 1.0
			if p, ok := r.price(c.Model); ok {
				// $10/1M combined -> 0. Cheapest providers win.
				costF = 1 - clamp01((p.PromptPer1M+p.CompletionPer1M)/10)
			}
			quotaF := 1.0
			if _, ratio, known := r.quota.Remaining(name, c.Model); known {
				quotaF = clamp01(ratio * 1.3) // full budget -> 1.0; ~23% left -> 0.3
				if ratio <= 0 {
					quotaF = 0
				}
			}
			score := health * latencyF * costF * quotaF
			scores = append(scores, scored{c, score})
		}
		sort.SliceStable(scores, func(i, j int) bool { return scores[i].score > scores[j].score })
		for i := range scores {
			allowed[i] = scores[i].c
		}
	}
	return allowed
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// Quota returns the router's quota ledger (never nil).
func (r *Router) Quota() *QuotaLedger {
	return r.quota
}

// costKey sorts by prompt+completion price asc; unknown price = last.
func (r *Router) costKey(c Candidate) float64 {
	if p, ok := r.price(c.Model); ok {
		return p.PromptPer1M + p.CompletionPer1M
	}
	return 1e18
}

// RecordResult updates the EMA latency, circuit breaker, and (for lkgp
// routes) the last-known-good provider. Called by the server after each
// attempt.
func (r *Router) RecordResult(providerName string, latency time.Duration, success bool) {
	r.RecordResultKind(providerName, latency, success, FailureUnknown, true)
}

// RecordResultKind is RecordResult with a classified failure; kinds that are
// not genuine provider failures (client aborts — countsAgainstProvider=false)
// skip the circuit breaker while still recording latency for observability.
func (r *Router) RecordResultKind(providerName string, latency time.Duration, success bool, kind FailureKind, countsAgainstProvider bool) {
	r.latMu.Lock()
	cur := r.latency[providerName]
	if cur == 0 {
		cur = float64(latency.Milliseconds())
	} else {
		cur = emaAlpha*float64(latency.Milliseconds()) + (1-emaAlpha)*cur
	}
	r.latency[providerName] = cur
	if success {
		r.errRate[providerName] = (1 - emaAlpha) * r.errRate[providerName]
	} else if countsAgainstProvider {
		r.errRate[providerName] = emaAlpha + (1-emaAlpha)*r.errRate[providerName]
	}
	w := r.windows[providerName]
	if w == nil {
		w = &window{}
		r.windows[providerName] = w
	}
	if now := time.Now(); now.Sub(w.start) >= 60*time.Second {
		w.start, w.reqs, w.toks = now, 1, 0
	} else {
		w.reqs++
	}
	r.latMu.Unlock()
	if cb, ok := r.circuits[providerName]; ok {
		if success {
			cb.OnSuccess()
		} else {
			cb.OnFailureKind(kind, countsAgainstProvider)
		}
	}
	for _, rt := range r.routes {
		if rt.Strategy != StrategyLKGP {
			continue
		}
		if success {
			rt.lastGood.Store(providerName)
		} else if lg, _ := rt.lastGood.Load().(string); lg == providerName {
			rt.lastGood.Store("")
		}
	}
}

// OpenCircuitFor opens a provider's circuit for a custom duration
// (Retry-After honoring). No-op when no breaker is configured.
func (r *Router) OpenCircuitFor(providerName string, d time.Duration) {
	if cb, ok := r.circuits[providerName]; ok {
		cb.OpenFor(d)
	}
}

// CircuitOpenUntil returns when the provider's open circuit allows a probe
// (zero if closed or no breaker).
func (r *Router) CircuitOpenUntil(providerName string) time.Time {
	if cb, ok := r.circuits[providerName]; ok {
		return cb.OpenUntil()
	}
	return time.Time{}
}

// CircuitState returns the breaker state for a provider ("closed" if none).
func (r *Router) CircuitState(providerName string) string {
	if cb, ok := r.circuits[providerName]; ok {
		return cb.State()
	}
	return "closed"
}

// ResetCircuit force-closes the breaker for a provider (no-op if none).
// Does not clear the auto-disabled state; use EnableProvider for that.
func (r *Router) ResetCircuit(providerName string) {
	if cb, ok := r.circuits[providerName]; ok {
		cb.OnSuccess()
	}
}

// DisableProvider forces the provider's breaker into the disabled state
// (Allow always false) until EnableProvider.
func (r *Router) DisableProvider(providerName string) {
	if cb, ok := r.circuits[providerName]; ok {
		cb.Disable()
	}
}

// EnableProvider re-enables a disabled provider: breaker closed, trips reset.
func (r *Router) EnableProvider(providerName string) {
	if cb, ok := r.circuits[providerName]; ok {
		cb.Enable()
	}
}

// ProviderDisabled reports whether the provider's breaker is disabled
// (manually or via auto-disable); false when no breaker is configured.
func (r *Router) ProviderDisabled(providerName string) bool {
	if cb, ok := r.circuits[providerName]; ok {
		return cb.Disabled()
	}
	return false
}

// LatencyMs returns the EMA latency in ms for a provider (0 if unseen).
func (r *Router) LatencyMs(providerName string) float64 {
	r.latMu.Lock()
	defer r.latMu.Unlock()
	return r.latency[providerName]
}

// WindowReqs returns the provider's request count in the current 60s window
// (0 if unseen) — headroom/lowest_usage signal, exposed for tests.
func (r *Router) WindowReqs(providerName string) int {
	r.latMu.Lock()
	defer r.latMu.Unlock()
	if w := r.windows[providerName]; w != nil {
		return w.reqs
	}
	return 0
}

// Resolve returns the route for an exact match, else nil.
func (r *Router) Resolve(model string) *Route {
	for _, rt := range r.routes {
		if rt.Model == model {
			return rt
		}
	}
	return nil
}

// Providers returns all providers sorted by priority ascending.
func (r *Router) Providers() []provider.Provider {
	return append([]provider.Provider(nil), r.providers...)
}

// RouteModels returns the configured virtual model names.
func (r *Router) RouteModels() []string {
	out := make([]string, 0, len(r.routes))
	for _, rt := range r.routes {
		out = append(out, rt.Model)
	}
	return out
}

// FirstModelFor returns the first candidate upstream model configured for a
// provider across all routes ("" when none) — used by the admin channel test.
func (r *Router) FirstModelFor(providerName string) string {
	for _, rt := range r.routes {
		for _, c := range rt.Candidates {
			if c.Provider.Name() == providerName {
				return c.Model
			}
		}
	}
	return ""
}
