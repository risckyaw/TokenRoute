package router

import (
	"testing"
	"time"
)

func TestHeaderKeyHash(t *testing.T) {
	if HeaderKeyHash("") != 0 {
		t.Fatal("empty value must hash to 0 (no pin)")
	}
	if HeaderKeyHash("a") == HeaderKeyHash("b") {
		t.Fatal("distinct values must isolate pins")
	}
	if HeaderKeyHash("a") != HeaderKeyHash("a") {
		t.Fatal("hash must be stable")
	}
}

func TestPutTTL_OverridesDefault(t *testing.T) {
	c := NewAffinityCache(time.Hour)
	c.now = func() time.Time { return time.Unix(1000, 0) }
	c.PutTTL(1, "p", "m", time.Minute)
	p, ok := c.Get(1)
	if !ok {
		t.Fatal("pin missing")
	}
	if p.ExpiresAt != time.Unix(1060, 0) {
		t.Fatalf("expires %v, want 60s TTL", p.ExpiresAt)
	}
	// Default TTL path unchanged.
	c.Put(2, "p", "m")
	p, _ = c.Get(2)
	if p.ExpiresAt != time.Unix(4600, 0) {
		t.Fatalf("default expires %v, want 1h", p.ExpiresAt)
	}
}
