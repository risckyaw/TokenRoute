package router

import (
	"sync"
	"time"
)

// CircuitConfig tunes a per-provider circuit breaker.
type CircuitConfig struct {
	FailureThreshold int // consecutive failures before opening; default 3
	CooldownMs       int // how long the circuit stays open; default 30000
	// AutoDisableAfter: after N open-transitions the breaker enters the
	// disabled state (Allow always false) until Enable; 0 = never.
	AutoDisableAfter int // default 3
	// Mode "percent" (LiteLLM DEFAULT_FAILURE_THRESHOLD_PERCENT): trip when
	// failures in the current minute >= max(MinRequests, FailurePercent*total)
	// AND failures >= 1. Default "consecutive" (current behavior).
	Mode           string
	FailurePercent float64 // default 0.5
	MinRequests    int     // default 5
	// AllowedFails (LiteLLM per-exception allowed_fails): consecutive failures
	// of a kind tolerated before opening; the kind trips at allowed_fails+1.
	// Kinds absent from the map use the global threshold semantics.
	// Auth/permission keep instant-open unless explicitly present here.
	AllowedFails map[FailureKind]int
}

const (
	stateClosed   = iota
	stateDegraded // early-warning band: traffic flows, dashboards warn
	stateOpen
	stateHalfOpen
	stateDisabled
)

// CircuitBreaker is a per-provider state machine:
// closed -> degraded at 60% of threshold (warning only) -> open at threshold;
// open for an escalating cooldown (each failed probe cycle doubles it, 16x cap);
// half-open allowing exactly 1 probe; probe success -> closed, failure -> open.
// Failure-kind aware: auth/permission failures open immediately (no point
// probing a bad key); client aborts never count (handled by ClassifyFailure
// callers — OnFailureKind(FailureUnknown) is ignored).
type CircuitBreaker struct {
	mu             sync.Mutex
	threshold      int
	cooldown       time.Duration
	autoDisable    int           // open-transitions before disabled; 0 = never
	customCooldown time.Duration // set by OpenFor; consumed on next opening
	failures       int
	state          int
	openedAt       time.Time
	trips          int // total open-transitions
	openCycles     int // open->half_open->open cycles (backoff escalation)
	lastKind       FailureKind
	now            func() time.Time // injectable for tests
	// percent-mode rolling minute window (inert in consecutive mode)
	mode           string
	failurePercent float64
	minRequests    int
	winStart       time.Time
	winTotal       int
	winFailures    int
	// per-kind policy (nil = single-counter behavior)
	allowedFails map[FailureKind]int
	kindFailures map[FailureKind]int
}

// failureKindNames maps snake_case config names to FailureKind values.
var failureKindNames = map[string]FailureKind{
	"auth":              FailureAuth,
	"permission":        FailurePermission,
	"rate_limit":        FailureRateLimit,
	"quota_exhausted":   FailureQuotaExhausted,
	"timeout":           FailureTimeout,
	"server":            FailureProvider5xx,
	"invalid_request":   FailureInvalidRequest,
	"model_unavailable": FailureModelUnavailable,
	"network":           FailureNetwork,
	"unknown":           FailureUnknown,
}

// ParseAllowedFails converts a snake_case name->count map (YAML) to
// FailureKind->count; unknown names are skipped.
func ParseAllowedFails(m map[string]int) map[FailureKind]int {
	if len(m) == 0 {
		return nil
	}
	out := map[FailureKind]int{}
	for name, n := range m {
		if kind, ok := failureKindNames[name]; ok {
			out[kind] = n
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func NewCircuitBreaker(cfg CircuitConfig) *CircuitBreaker {
	threshold := cfg.FailureThreshold
	if threshold <= 0 {
		threshold = 3
	}
	cooldown := time.Duration(cfg.CooldownMs) * time.Millisecond
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	autoDisable := cfg.AutoDisableAfter
	if autoDisable <= 0 {
		// ponytail: 0 = unset -> default 3; use a huge value to effectively
		// never disable, or switch to *int when a real "never" knob is needed.
		autoDisable = 3
	}
	cb := &CircuitBreaker{threshold: threshold, cooldown: cooldown, autoDisable: autoDisable, now: time.Now}
	if cfg.Mode == "percent" {
		cb.mode = "percent"
		cb.failurePercent = cfg.FailurePercent
		if cb.failurePercent <= 0 {
			cb.failurePercent = 0.5
		}
		cb.minRequests = cfg.MinRequests
		if cb.minRequests <= 0 {
			cb.minRequests = 5
		}
	}
	if len(cfg.AllowedFails) > 0 {
		cb.allowedFails = cfg.AllowedFails
		cb.kindFailures = map[FailureKind]int{}
	}
	return cb
}

// recordWindow feeds the percent-mode rolling minute window; returns true
// when the failure ratio trips the circuit (LiteLLM percent threshold).
func (c *CircuitBreaker) recordWindow(success bool) bool {
	now := c.now()
	if now.Sub(c.winStart) >= 60*time.Second {
		c.winStart, c.winTotal, c.winFailures = now, 0, 0
	}
	c.winTotal++
	if success {
		return false
	}
	c.winFailures++
	need := int(c.failurePercent * float64(c.winTotal))
	if need < c.minRequests {
		need = c.minRequests
	}
	return c.winFailures >= need && c.winFailures >= 1
}

// degradationThreshold is the failure count at which the breaker enters the
// DEGRADED warning band: 60% of the open threshold. Only meaningful for
// thresholds >= 4; smaller breakers skip the band (returns threshold, so the
// degraded check never fires before open).
func (c *CircuitBreaker) degradationThreshold() int {
	if c.threshold < 4 {
		return c.threshold
	}
	d := (c.threshold * 60) / 100
	if d < 2 {
		d = 2
	}
	if d >= c.threshold {
		d = c.threshold - 1
	}
	return d
}

// effectiveCooldown escalates the base cooldown after repeated failed probe
// cycles: 2^(cycles-3), capped at 16x (ported from OmniRoute
// _effectiveResetTimeout — a provider that keeps failing probes backs off
// instead of hot-looping every 30s).
func (c *CircuitBreaker) effectiveCooldown() time.Duration {
	cd := c.cooldown
	if c.customCooldown > 0 {
		cd = c.customCooldown
	}
	const escalateAfter = 3
	if c.openCycles <= escalateAfter {
		return cd
	}
	shift := c.openCycles - escalateAfter
	if shift > 4 {
		shift = 4 // 16x cap
	}
	return cd << shift
}

// Allow reports whether a request may be sent to the provider. An open
// circuit transitions to half-open (one probe) once the cooldown elapses.
func (c *CircuitBreaker) Allow() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case stateClosed, stateDegraded:
		return true
	case stateDisabled:
		return false // manual re-enable only
	case stateOpen:
		if c.now().Sub(c.openedAt) >= c.effectiveCooldown() {
			c.state = stateHalfOpen
			return true
		}
		return false
	default: // half-open: probe already in flight
		return false
	}
}

// open transitions to the open state and counts the trip; after
// autoDisable trips the breaker is disabled instead (stays down until Enable).
func (c *CircuitBreaker) open() {
	c.trips++
	if c.autoDisable > 0 && c.trips >= c.autoDisable {
		c.state = stateDisabled
		c.openedAt = c.now()
		c.customCooldown = 0
		return
	}
	c.state = stateOpen
	c.openedAt = c.now()
	c.customCooldown = 0
}

// Disable forces the breaker into the disabled state (admin channel off).
func (c *CircuitBreaker) Disable() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = stateDisabled
	c.customCooldown = 0
}

// Disabled reports whether the breaker was auto-disabled.
func (c *CircuitBreaker) Disabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state == stateDisabled
}

// Enable clears the disabled state, trips, cycles, and failures (manual re-enable).
func (c *CircuitBreaker) Enable() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = stateClosed
	c.failures = 0
	c.trips = 0
	c.openCycles = 0
	c.customCooldown = 0
}

func (c *CircuitBreaker) OnSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mode == "percent" {
		c.recordWindow(true)
	}
	c.state = stateClosed
	c.failures = 0
	c.openCycles = 0
	c.customCooldown = 0
	if c.kindFailures != nil {
		c.kindFailures = map[FailureKind]int{}
	}
}

// OnFailure counts a generic failure (legacy callers: counts against provider).
func (c *CircuitBreaker) OnFailure() {
	c.OnFailureKind(FailureUnknown, true)
}

// OnFailureKind counts a classified failure. unknown+not-from-upstream (client
// abort filtered by ClassifyFailure) does not count. Auth/permission opens
// immediately — probing a bad key is pointless.
func (c *CircuitBreaker) OnFailureKind(kind FailureKind, countsAgainstProvider bool) {
	if !countsAgainstProvider {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == stateDisabled {
		return
	}
	c.lastKind = kind
	if c.allowedFails != nil {
		if c.state == stateHalfOpen {
			c.openCycles++
			c.open()
			return
		}
		c.kindFailures[kind]++
		limit, explicit := c.allowedFails[kind]
		switch {
		case explicit:
			// Explicit policy: auth/permission lose their instant-open.
			if c.kindFailures[kind] > limit {
				c.open()
			}
		case kind == FailureAuth || kind == FailurePermission:
			c.open() // default: instant-open for hopeless credentials
		default:
			if c.kindFailures[kind] >= c.threshold {
				c.open()
				return
			}
			if c.state == stateClosed {
				max := 0
				for _, n := range c.kindFailures {
					if n > max {
						max = n
					}
				}
				if max >= c.degradationThreshold() {
					c.state = stateDegraded
				}
			}
		}
		return
	}
	if c.mode == "percent" {
		trip := c.recordWindow(false)
		// A failed half-open probe always reopens, regardless of ratio.
		if c.state == stateHalfOpen {
			c.openCycles++
			c.open()
			return
		}
		if trip {
			c.open()
		}
		return
	}
	if kind == FailureAuth || kind == FailurePermission {
		c.failures = c.threshold
		if c.state == stateHalfOpen {
			c.openCycles++
		}
		c.open()
		return
	}
	if c.state == stateHalfOpen {
		c.openCycles++
		c.open()
		return
	}
	c.failures++
	if c.failures >= c.threshold {
		c.open()
		return
	}
	if c.state == stateClosed && c.failures >= c.degradationThreshold() {
		c.state = stateDegraded
	}
}

// OpenFor opens the circuit for a custom duration (e.g. Retry-After from a
// 429 response) instead of the configured cooldown. Failures are set to the
// threshold so the state is meaningful; the next Allow after d transitions
// to half-open as usual.
func (c *CircuitBreaker) OpenFor(d time.Duration) {
	if d <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == stateDisabled {
		return
	}
	c.open()
	c.customCooldown = d
	c.failures = c.threshold
}

// OpenUntil returns when the open circuit will allow a half-open probe.
// Zero time when not open.
func (c *CircuitBreaker) OpenUntil() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != stateOpen {
		return time.Time{}
	}
	return c.openedAt.Add(c.effectiveCooldown())
}

// State returns "closed", "degraded", "open", "half-open", or "disabled".
func (c *CircuitBreaker) State() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case stateDegraded:
		return "degraded"
	case stateOpen:
		return "open"
	case stateHalfOpen:
		return "half-open"
	case stateDisabled:
		return "disabled"
	default:
		return "closed"
	}
}
