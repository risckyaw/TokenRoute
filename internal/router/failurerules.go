package router

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Backoff bounds for failure rules with backoff: true (9router errorConfig.js):
// 2s doubling per escalation level, capped at 5 minutes.
const (
	backoffBase = 2 * time.Second
	backoffCap  = 5 * time.Minute
)

// FailureRule is one ordered cooldown rule (9router ERROR_RULES). Exactly one
// of Match (case-insensitive substring of the error body) or Status is set.
type FailureRule struct {
	Match      string
	Status     int
	CooldownMs int
	// Backoff selects a per-provider escalating cooldown (2s, 4s, 8s, ... 5min)
	// instead of the fixed CooldownMs; the level resets on the provider's next
	// success.
	Backoff bool
}

// FailureRules evaluates cooldown rules in order — every text rule first, then
// the status rules, first hit wins — and tracks per-provider backoff levels.
// A nil *FailureRules preserves the built-in cooldown behavior exactly.
type FailureRules struct {
	text   []FailureRule // Match rules, config order preserved
	status []FailureRule // Status rules, config order preserved
	mu     sync.Mutex
	levels map[string]int // provider name -> backoff escalation level
}

// NewFailureRules validates and orders the configured rules. A rule with
// neither match nor status (or with both) is a config error, as is a rule with
// no cooldown at all.
func NewFailureRules(rules []FailureRule) (*FailureRules, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	fr := &FailureRules{levels: map[string]int{}}
	for i, r := range rules {
		hasMatch := strings.TrimSpace(r.Match) != ""
		hasStatus := r.Status != 0
		switch {
		case !hasMatch && !hasStatus:
			return nil, fmt.Errorf("failure_rules[%d]: needs match or status", i)
		case hasMatch && hasStatus:
			return nil, fmt.Errorf("failure_rules[%d]: match and status are mutually exclusive", i)
		case hasStatus && (r.Status < 100 || r.Status > 599):
			return nil, fmt.Errorf("failure_rules[%d]: bad status %d", i, r.Status)
		case !r.Backoff && r.CooldownMs <= 0:
			return nil, fmt.Errorf("failure_rules[%d]: needs cooldown_ms or backoff: true", i)
		case r.Backoff && r.CooldownMs > 0:
			return nil, fmt.Errorf("failure_rules[%d]: cooldown_ms and backoff are mutually exclusive", i)
		}
		if hasMatch {
			r.Match = strings.ToLower(strings.TrimSpace(r.Match))
			fr.text = append(fr.text, r)
			continue
		}
		fr.status = append(fr.status, r)
	}
	return fr, nil
}

// Cooldown returns the cooldown a matching rule prescribes for one failure of
// the given provider. ok=false when no rule matches, leaving the circuit's own
// cooldown in charge. Text rules are checked before status rules, in config
// order; the first hit wins. A backoff rule consumes one escalation level for
// that provider (2s, 4s, 8s, ... capped at 5min).
func (fr *FailureRules) Cooldown(providerName string, status int, bodySnippet string) (time.Duration, bool) {
	if fr == nil {
		return 0, false
	}
	norm := strings.ToLower(bodySnippet)
	for _, r := range fr.text {
		if strings.Contains(norm, r.Match) {
			return fr.duration(providerName, r), true
		}
	}
	for _, r := range fr.status {
		if r.Status == status {
			return fr.duration(providerName, r), true
		}
	}
	return 0, false
}

// duration resolves a matched rule's cooldown, escalating when it uses backoff.
func (fr *FailureRules) duration(providerName string, r FailureRule) time.Duration {
	if !r.Backoff {
		return time.Duration(r.CooldownMs) * time.Millisecond
	}
	fr.mu.Lock()
	level := fr.levels[providerName]
	fr.levels[providerName] = level + 1
	fr.mu.Unlock()
	d := backoffBase
	for i := 0; i < level; i++ {
		d *= 2
		if d >= backoffCap {
			return backoffCap
		}
	}
	return d
}

// OnSuccess clears a provider's backoff level (9router: a working provider
// starts over at 2s).
func (fr *FailureRules) OnSuccess(providerName string) {
	if fr == nil {
		return
	}
	fr.mu.Lock()
	delete(fr.levels, providerName)
	fr.mu.Unlock()
}

// BackoffLevel reports a provider's current escalation level (tests/debug).
func (fr *FailureRules) BackoffLevel(providerName string) int {
	if fr == nil {
		return 0
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	return fr.levels[providerName]
}
