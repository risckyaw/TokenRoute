package router

import "testing"

func TestParseStatusRanges(t *testing.T) {
	tab, err := ParseStatusRanges("100-101,401,409-410")
	if err != nil {
		t.Fatal(err)
	}
	matrix := map[int]bool{100: true, 101: true, 102: false, 401: true, 402: false, 409: true, 410: true, 411: false, 200: false}
	for code, want := range matrix {
		if tab[code] != want {
			t.Errorf("status %d = %v, want %v", code, tab[code], want)
		}
	}
}

func TestParseStatusRangesInvalid(t *testing.T) {
	for _, s := range []string{"abc", "1-2-3", "500-400", "600", "-5", "401x"} {
		if _, err := ParseStatusRanges(s); err == nil {
			t.Errorf("%q: want error", s)
		}
	}
}

// new-api defaults: never retry 504/524 even when a range covers them.
func TestNeverRetryWins(t *testing.T) {
	p, err := NewRetryPolicy("500-599", "", []int{504, 524}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Retryable(500) || !p.Retryable(503) || !p.Retryable(599) {
		t.Fatal("500-599 except never_retry must be retryable")
	}
	if p.Retryable(504) || p.Retryable(524) {
		t.Fatal("never_retry must override the range")
	}
}

func TestDisableStatusRanges(t *testing.T) {
	p, err := NewRetryPolicy("500-503", "401", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !p.DisableStatus(401) {
		t.Fatal("401 must be a disable status")
	}
	if p.DisableStatus(403) || p.DisableStatus(500) {
		t.Fatal("403/500 must not be disable statuses")
	}
}

func TestKeywordClassification(t *testing.T) {
	p, err := NewRetryPolicy("500-503", "", nil,
		[]string{"Insufficient Balance", "account suspended", "Your quota is exceeded"})
	if err != nil {
		t.Fatal(err)
	}
	// Billing-related keyword -> quota_exhausted (long cooldown).
	kind, ok := p.ClassifyKeyword(`{"error":"insufficient balance remaining"}`)
	if !ok || kind != FailureQuotaExhausted {
		t.Fatalf("kind=%v ok=%v, want quota_exhausted", kind, ok)
	}
	kind, ok = p.ClassifyKeyword(`{"error":"your quota is exceeded for today"}`)
	if !ok || kind != FailureQuotaExhausted {
		t.Fatalf("kind=%v ok=%v, want quota_exhausted", kind, ok)
	}
	// Non-billing keyword -> auth.
	kind, ok = p.ClassifyKeyword(`{"error":"Account Suspended"}`)
	if !ok || kind != FailureAuth {
		t.Fatalf("kind=%v ok=%v, want auth", kind, ok)
	}
	// No match.
	if _, ok = p.ClassifyKeyword(`{"error":"rate limit"}`); ok {
		t.Fatal("no keyword should match")
	}
}

func TestNilPolicyKeywordSafe(t *testing.T) {
	var p *RetryPolicy
	if _, ok := p.ClassifyKeyword("insufficient balance"); ok {
		t.Fatal("nil policy must not match")
	}
}
