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
	return &CircuitBreaker{threshold: threshold, cooldown: cooldown, autoDisable: autoDisable, now: time.Now}
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
	c.state = stateClosed
	c.failures = 0
	c.openCycles = 0
	c.customCooldown = 0
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
