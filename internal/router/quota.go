package router

import (
	"sync"
	"time"
)

// ProviderQuota is a per (provider, model) token budget ledger with fixed
// windows — ported from OmniRoute providerQuotaState.ts. Purpose: PRE-REQUEST
// capacity awareness. Before dispatching, strategies ask "does this candidate
// have budget left?" and skip/exhausted-rank exhausted ones instead of
// waiting for a 429.
//
// Fail-open: unknown budget = available (routing unchanged when quota
// tracking is not configured). Writes are best-effort.
type ProviderQuota struct {
	TokenLimit  int64 // tokens per window; 0 = unlimited/unknown
	Window      time.Duration
	tokensUsed  int64
	windowStart time.Time
	// obs: last upstream-signalled quota state (Kong response-ratelimiting
	// port — trust the provider's own headers over local accounting).
	obs *upstreamObs
}

// upstreamObs is a quota observation parsed from upstream response headers
// (x-ratelimit-remaining-tokens / x-ratelimit-reset-tokens).
type upstreamObs struct {
	remaining  int64
	resetAt    time.Time
	observedAt time.Time
}

// observedFresh is the staleness window for upstream observations (Kong
// treats header state as authoritative only briefly).
const observedFresh = 60 * time.Second

// QuotaLedger tracks per provider|model token windows. In-memory only:
// windows are short (per-minute/per-day), so persistence buys nothing.
type QuotaLedger struct {
	mu   sync.Mutex
	rows map[string]*ProviderQuota // provider|model -> quota
	now  func() time.Time
}

func NewQuotaLedger() *QuotaLedger {
	return &QuotaLedger{rows: map[string]*ProviderQuota{}, now: time.Now}
}

func quotaKey(provider, model string) string { return provider + "|" + model }

// SetLimit configures the window budget for a provider+model pair.
func (l *QuotaLedger) SetLimit(provider, model string, limit int64, window time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := quotaKey(provider, model)
	q := l.rows[k]
	if q == nil {
		q = &ProviderQuota{}
		l.rows[k] = q
	}
	q.TokenLimit = limit
	q.Window = window
}

// ObserveUpstream stores the provider-signalled remaining-token budget and
// reset hint (Kong response-ratelimiting port). Quota-aware strategies prefer
// a fresh (<60s) observation over local accounting.
func (l *QuotaLedger) ObserveUpstream(provider, model string, remainingTokens int64, resetIn time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := quotaKey(provider, model)
	q := l.rows[k]
	if q == nil {
		q = &ProviderQuota{}
		l.rows[k] = q
	}
	now := l.now()
	q.obs = &upstreamObs{remaining: remainingTokens, resetAt: now.Add(resetIn), observedAt: now}
}

// observed returns the fresh upstream observation, or nil (caller holds mu).
func (l *QuotaLedger) observed(q *ProviderQuota) *upstreamObs {
	if q.obs == nil || l.now().Sub(q.obs.observedAt) >= observedFresh {
		return nil
	}
	return q.obs
}

// Remaining returns tokens left in the current window and the ratio 0..1.
// A fresh upstream observation wins over local accounting; its ratio uses
// the configured TokenLimit as denominator when known (else ratio 1 when
// remaining > 0). Unknown/unconfigured -> ratio 1, known=false.
func (l *QuotaLedger) Remaining(provider, model string) (remaining int64, ratio float64, known bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	q := l.rows[quotaKey(provider, model)]
	if q == nil {
		return 0, 1, false
	}
	if obs := l.observed(q); obs != nil {
		rem := obs.remaining
		if rem < 0 {
			rem = 0
		}
		if q.TokenLimit > 0 {
			return rem, clampF(float64(rem)/float64(q.TokenLimit)), true
		}
		if rem > 0 {
			return rem, 1, true
		}
		return 0, 0, true
	}
	if q.TokenLimit <= 0 {
		return 0, 1, false
	}
	l.rollWindow(q)
	rem := q.TokenLimit - q.tokensUsed
	if rem < 0 {
		rem = 0
	}
	return rem, float64(rem) / float64(q.TokenLimit), true
}

func clampF(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// Affordable reports whether est tokens fit the remaining window budget.
// Fail-open: unknown budget = affordable.
func (l *QuotaLedger) Affordable(provider, model string, est int64) bool {
	rem, _, known := l.Remaining(provider, model)
	if !known {
		return true
	}
	return rem >= est
}

// Record adds used tokens to the current window (post-response).
func (l *QuotaLedger) Record(provider, model string, tokens int64) {
	if tokens <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	q := l.rows[quotaKey(provider, model)]
	if q == nil {
		return // no limit configured: nothing to track
	}
	l.rollWindow(q)
	q.tokensUsed += tokens
}

// WindowReset returns when the current window resets (zero when unknown).
// A fresh upstream observation's resetAt wins.
func (l *QuotaLedger) WindowReset(provider, model string) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	q := l.rows[quotaKey(provider, model)]
	if q == nil {
		return time.Time{}
	}
	if obs := l.observed(q); obs != nil {
		return obs.resetAt
	}
	if q.TokenLimit <= 0 {
		return time.Time{}
	}
	l.rollWindow(q)
	return q.windowStart.Add(q.Window)
}

// rollWindow resets the counter when the window has elapsed (caller holds mu).
func (l *QuotaLedger) rollWindow(q *ProviderQuota) {
	if q.Window <= 0 {
		q.Window = time.Minute
	}
	now := l.now()
	if q.windowStart.IsZero() {
		q.windowStart = now
		return
	}
	if now.Sub(q.windowStart) >= q.Window {
		q.windowStart = now
		q.tokensUsed = 0
	}
}
