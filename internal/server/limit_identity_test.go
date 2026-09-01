package server

import (
	"net/http/httptest"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/auth"
)

func TestLimitIdentityDefault(t *testing.T) {
	k := &auth.Key{ID: 42}
	req := httptest.NewRequest("POST", "/", nil)
	if got := limitIdentity(k, req); got != 42 {
		t.Errorf("limitIdentity = %d, want 42", got)
	}
}

func TestLimitIdentityByHeader(t *testing.T) {
	k := &auth.Key{ID: 42, LimitByHeader: "X-User-Id"}
	r1 := httptest.NewRequest("POST", "/", nil)
	r1.Header.Set("X-User-Id", "alice")
	r2 := httptest.NewRequest("POST", "/", nil)
	r2.Header.Set("X-User-Id", "bob")
	r3 := httptest.NewRequest("POST", "/", nil)
	r3.Header.Set("X-User-Id", "alice")

	a, b, a2 := limitIdentity(k, r1), limitIdentity(k, r2), limitIdentity(k, r3)
	if a == b {
		t.Error("alice and bob share an identity")
	}
	if a != a2 {
		t.Error("alice identity not stable across requests")
	}
	if a == 42 || b == 42 {
		t.Error("header-derived identity collides with key ID")
	}
	if a < 0 {
		t.Error("identity negative — sign bit not masked")
	}
}

func TestLimitIdentityHeaderMissing(t *testing.T) {
	k := &auth.Key{ID: 7, LimitByHeader: "X-User-Id"}
	req := httptest.NewRequest("POST", "/", nil)
	if got := limitIdentity(k, req); got != 7 {
		t.Errorf("missing header should fall back to key ID, got %d", got)
	}
}
