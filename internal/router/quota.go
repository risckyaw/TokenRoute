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
}

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

// Remaining returns tokens left in the current window and the ratio 0..1.
// Unknown/unconfigured -> (limit>0 ? limit : +inf semantics, ratio 1).
func (l *QuotaLedger) Remaining(provider, model string) (remaining int64, ratio float64, known bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	q := l.rows[quotaKey(provider, model)]
	if q == nil || q.TokenLimit <= 0 {
		return 0, 1, false
	}
	l.rollWindow(q)
	rem := q.TokenLimit - q.tokensUsed
	if rem < 0 {
		rem = 0
	}
	return rem, float64(rem) / float64(q.TokenLimit), true
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
func (l *QuotaLedger) WindowReset(provider, model string) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	q := l.rows[quotaKey(provider, model)]
	if q == nil || q.TokenLimit <= 0 {
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
