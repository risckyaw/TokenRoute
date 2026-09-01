package router

import (
	"sync"
	"time"
)

// CircuitConfig tunes a per-provider circuit breaker.
type CircuitConfig struct {
	FailureThreshold int // consecutive failures before opening; default 3
	CooldownMs       int // how long the circuit stays open; default 30000
}

const (
	stateClosed = iota
	stateOpen
	stateHalfOpen
)

// CircuitBreaker is a per-provider state machine:
// closed -> open after threshold consecutive failures; open for cooldown;
// half-open allowing exactly 1 probe; probe success -> closed, failure -> open.
type CircuitBreaker struct {
	mu             sync.Mutex
	threshold      int
	cooldown       time.Duration
	customCooldown time.Duration // set by OpenFor; consumed on next opening
	failures       int
	state          int
	openedAt       time.Time
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
	return &CircuitBreaker{threshold: threshold, cooldown: cooldown, now: time.Now}
}

// Allow reports whether a request may be sent to the provider. An open
// circuit transitions to half-open (one probe) once the cooldown elapses.
func (c *CircuitBreaker) Allow() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case stateClosed:
		return true
	case stateOpen:
		cd := c.cooldown
		if c.customCooldown > 0 {
			cd = c.customCooldown
		}
		if c.now().Sub(c.openedAt) >= cd {
			c.state = stateHalfOpen
			return true
		}
		return false
	default: // half-open: probe already in flight
		return false
	}
}

func (c *CircuitBreaker) OnSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = stateClosed
	c.failures = 0
	c.customCooldown = 0
}

func (c *CircuitBreaker) OnFailure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == stateHalfOpen {
		c.state = stateOpen
		c.openedAt = c.now()
		c.customCooldown = 0
		return
	}
	c.failures++
	if c.failures >= c.threshold {
		c.state = stateOpen
		c.openedAt = c.now()
		c.customCooldown = 0
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
	c.state = stateOpen
	c.openedAt = c.now()
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
	cd := c.cooldown
	if c.customCooldown > 0 {
		cd = c.customCooldown
	}
	return c.openedAt.Add(cd)
}

// State returns "closed", "open", or "half-open" (for later admin use).
func (c *CircuitBreaker) State() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case stateOpen:
		return "open"
	case stateHalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}
