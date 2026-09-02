package router

import (
	"fmt"
	"strconv"
	"strings"
)

// RetryPolicy overrides the hardcoded failover/disable decisions (port of
// new-api status_code_ranges + AutomaticDisableKeywords). A nil *RetryPolicy
// preserves the built-in behavior exactly.
type RetryPolicy struct {
	retry   [600]bool // retry_status_ranges minus never_retry
	disable [600]bool // disable_status_ranges
	// DisableKeywords: case-insensitive substring match on the error body;
	// a hit classifies like auth/quota (opens the circuit fast).
	DisableKeywords []string
}

// ParseStatusRanges parses "100-199,401,409-499" into a bool table.
// Single codes are a degenerate range. Invalid input errors (config load
// fails fast).
func ParseStatusRanges(s string) ([600]bool, error) {
	var out [600]bool
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, found := strings.Cut(part, "-")
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil || a < 0 || a > 599 {
			return out, fmt.Errorf("bad status range %q", part)
		}
		b := a
		if found {
			b, err = strconv.Atoi(strings.TrimSpace(hi))
			if err != nil || b < a || b > 599 {
				return out, fmt.Errorf("bad status range %q", part)
			}
		}
		for i := a; i <= b; i++ {
			out[i] = true
		}
	}
	return out, nil
}

// NewRetryPolicy builds a policy from config values. neverRetry wins over
// retryStatusRanges. Keywords lowercased once here.
func NewRetryPolicy(retryRanges, disableRanges string, neverRetry []int, keywords []string) (*RetryPolicy, error) {
	p := &RetryPolicy{}
	var err error
	if p.retry, err = ParseStatusRanges(retryRanges); err != nil {
		return nil, err
	}
	for _, code := range neverRetry {
		if code < 0 || code > 599 {
			return nil, fmt.Errorf("bad never_retry status %d", code)
		}
		p.retry[code] = false
	}
	if disableRanges != "" {
		if p.disable, err = ParseStatusRanges(disableRanges); err != nil {
			return nil, err
		}
	}
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" {
			p.DisableKeywords = append(p.DisableKeywords, kw)
		}
	}
	return p, nil
}

// Retryable reports whether an upstream status triggers failover per the
// configured ranges (only called when a policy is installed).
func (p *RetryPolicy) Retryable(code int) bool {
	if code < 0 || code > 599 {
		return false
	}
	return p.retry[code]
}

// DisableStatus reports whether an upstream status is an instant-disable
// (auth-class) per the configured ranges.
func (p *RetryPolicy) DisableStatus(code int) bool {
	if code < 0 || code > 599 {
		return false
	}
	return p.disable[code]
}

// ClassifyKeyword maps a keyword hit to a failure kind: billing-related
// keywords (balance/credit/quota/insufficient) become quota_exhausted
// (long cooldown), the rest auth (instant circuit open). ok=false when no
// keyword matches.
func (p *RetryPolicy) ClassifyKeyword(bodySnippet string) (FailureKind, bool) {
	if p == nil || len(p.DisableKeywords) == 0 {
		return FailureUnknown, false
	}
	norm := strings.ToLower(bodySnippet)
	for _, kw := range p.DisableKeywords {
		if !strings.Contains(norm, kw) {
			continue
		}
		if strings.Contains(kw, "balance") || strings.Contains(kw, "credit") ||
			strings.Contains(kw, "quota") || strings.Contains(kw, "insufficient") {
			return FailureQuotaExhausted, true
		}
		return FailureAuth, true
	}
	return FailureUnknown, false
}
