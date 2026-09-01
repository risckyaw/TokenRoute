package router

import (
	"regexp"
	"strings"
	"time"
)

// FailureKind classifies an upstream failure for retry/cooldown policy.
type FailureKind int

const (
	FailureUnknown          FailureKind = iota
	FailureAuth                         // 401/403 invalid key — not retryable on this provider
	FailurePermission                   // 403 permission — not retryable
	FailureRateLimit                    // 429 plain — retryable elsewhere, short cooldown
	FailureQuotaExhausted               // 429 with balance/credit wording — long cooldown, not "rate limit"
	FailureTimeout                      // 408/504 — retryable
	FailureProvider5xx                  // 5xx — retryable
	FailureInvalidRequest               // 400 — not retryable (deterministic)
	FailureModelUnavailable             // 404 — not retryable on this model
	FailureNetwork                      // transport error — retryable
)

// Failure describes one upstream attempt's failure.
type Failure struct {
	Kind      FailureKind
	Retryable bool
}

var (
	reAuth         = regexp.MustCompile(`invalid.*(key|token)|unauthori[sz]ed|forbidden`)
	reQuota        = regexp.MustCompile(`quota|insufficient.?balance|credits? exhausted|balance is \$?0|insufficient_user_quota`)
	reRateLimit    = regexp.MustCompile(`rate.?limit|too many requests|retry.?after`)
	reTimeout      = regexp.MustCompile(`timeout|timed out|deadline exceeded`)
	reModelMissing = regexp.MustCompile(`model (unavailable|not found|does not exist)|unknown model|no such model`)
	reInvalid      = regexp.MustCompile(`invalid request|malformed|unsupported parameter|validation`)
	reNetwork      = regexp.MustCompile(`connection (reset|refused)|no such host|EOF|broken pipe|network`)
)

// ClassifyFailure maps an upstream status + body snippet + transport error to
// a Failure. Ported from OmniRoute failureClassification.ts: a 429 whose body
// signals exhausted balance is quota_exhausted (long cooldown), not a plain
// rate limit — retrying it elsewhere with the same account is pointless.
func ClassifyFailure(statusCode int, bodySnippet string, transportErr error) Failure {
	if transportErr != nil {
		if isLocalAbort(transportErr) {
			// Client disconnect / local lifecycle error: NOT a provider failure.
			return Failure{Kind: FailureUnknown, Retryable: false}
		}
		msg := strings.ToLower(transportErr.Error())
		if reTimeout.MatchString(msg) {
			return Failure{Kind: FailureTimeout, Retryable: true}
		}
		return Failure{Kind: FailureNetwork, Retryable: true}
	}
	norm := strings.ToLower(bodySnippet)
	switch {
	case statusCode == 401:
		return Failure{Kind: FailureAuth}
	case statusCode == 403:
		if strings.Contains(norm, "permission") {
			return Failure{Kind: FailurePermission}
		}
		return Failure{Kind: FailureAuth}
	case statusCode == 408 || statusCode == 504 || reTimeout.MatchString(norm):
		return Failure{Kind: FailureTimeout, Retryable: true}
	case statusCode == 429:
		if reQuota.MatchString(norm) {
			return Failure{Kind: FailureQuotaExhausted}
		}
		return Failure{Kind: FailureRateLimit, Retryable: true}
	case statusCode >= 500:
		return Failure{Kind: FailureProvider5xx, Retryable: true}
	case statusCode == 400 || reInvalid.MatchString(norm):
		return Failure{Kind: FailureInvalidRequest}
	case statusCode == 404 || reModelMissing.MatchString(norm):
		return Failure{Kind: FailureModelUnavailable}
	}
	return Failure{Kind: FailureUnknown}
}

// QuotaExhaustedCooldown is the model lockout for an exhausted-quota 429 —
// much longer than a plain rate-limit cooldown since the window is unknown
// and retrying burns attempts. Overridden by upstream reset hints.
const QuotaExhaustedCooldown = 15 * time.Minute

// isLocalAbort reports whether err is a client-side abort that must not count
// as a provider failure (ported from OmniRoute isLocalStreamLifecycleError:
// one user closing a tab must not cool down a whole provider).
func isLocalAbort(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "context canceled") ||
		strings.Contains(msg, "client disconnected") ||
		strings.Contains(msg, "operation was aborted") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection reset by peer")
}
