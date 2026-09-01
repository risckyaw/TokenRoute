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
	StrategyFusion       = "fusion" // race first 2 candidates concurrently
)

// ValidStrategy reports whether s is a known strategy name.
func ValidStrategy(s string) bool {
	switch s {
	case StrategyPriority, StrategyRoundRobin, StrategyLeastLatency,
		StrategyWeighted, StrategyCost, StrategyLKGP, StrategyHeadroom, StrategyFusion:
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
}

type Route struct {
	Model      string
	Strategy   string
	Multiplier float64     // cost multiplier; 0 -> 1.0
	Candidates []Candidate // sorted by Provider.Priority() ascending

	rr       atomic.Uint64 // round-robin counter
	lastGood atomic.Value  // string: provider name that served last success (lkgp)
	mu       sync.Mutex    // guards rand source for weighted
	randSrc  *rand.Rand
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
	byName    map[string]provider.Provider
	circuits  map[string]*CircuitBreaker
	latency   map[string]float64 // EMA latency ms per provider name
	prices    map[string]usage.Price
	mappings  map[string]map[string]string // provider name -> alias -> upstream model
	latMu     sync.Mutex
	lockMu    sync.Mutex
	modelLock map[string]time.Time // provider|model -> locked until
	windows   map[string]*window   // provider name -> 60s tumbling counters
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
		modelLock: map[string]time.Time{},
		windows:   map[string]*window{},
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

// SetPrices installs the price map used by the cost strategy.
func (r *Router) SetPrices(prices map[string]usage.Price) {
	r.prices = prices
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
	allowed := make([]Candidate, 0, len(rt.Candidates))
	for _, c := range rt.Candidates {
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
	}
	return allowed
}

// costKey sorts by prompt+completion price asc; unknown price = last.
func (r *Router) costKey(c Candidate) float64 {
	if p, ok := r.prices[c.Model]; ok {
		return p.PromptPer1M + p.CompletionPer1M
	}
	return 1e18
}

// RecordResult updates the EMA latency, circuit breaker, and (for lkgp
// routes) the last-known-good provider. Called by the server after each
// attempt.
func (r *Router) RecordResult(providerName string, latency time.Duration, success bool) {
	r.latMu.Lock()
	cur := r.latency[providerName]
	if cur == 0 {
		cur = float64(latency.Milliseconds())
	} else {
		cur = emaAlpha*float64(latency.Milliseconds()) + (1-emaAlpha)*cur
	}
	r.latency[providerName] = cur
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
			cb.OnFailure()
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
