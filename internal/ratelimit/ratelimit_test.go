package ratelimit

import (
	"testing"
	"time"
)

func TestBucket_AllowsUpToCapacityThenDenies(t *testing.T) {
	b := NewBucket(3, 0) // no refill
	for i := 0; i < 3; i++ {
		if !b.Allow(1) {
			t.Fatalf("denied at %d, want allow", i)
		}
	}
	if b.Allow(1) {
		t.Fatal("allowed past capacity")
	}
}

func TestBucket_RefillsOverTime(t *testing.T) {
	b := NewBucket(10, 10) // 10 tokens/sec
	now := time.Now()
	b.now = func() time.Time { return now }
	b.last = now
	for i := 0; i < 10; i++ {
		if !b.Allow(1) {
			t.Fatalf("denied at %d", i)
		}
	}
	if b.Allow(1) {
		t.Fatal("allowed past capacity")
	}
	now = now.Add(500 * time.Millisecond) // +5 tokens
	if !b.Allow(5) {
		t.Fatal("refill did not restore 5 tokens")
	}
	if b.Allow(1) {
		t.Fatal("allowed more than refilled")
	}
	now = now.Add(time.Hour) // clamps at capacity
	if !b.Allow(10) {
		t.Fatal("want full bucket after long wait")
	}
}

func TestBucket_UnlimitedNeverDenies(t *testing.T) {
	b := NewBucket(0, 0)
	for i := 0; i < 1000; i++ {
		if !b.Allow(1000) {
			t.Fatal("unlimited bucket denied")
		}
	}
}

func TestBucket_DeductFloorsAtZero(t *testing.T) {
	b := NewBucket(5, 0)
	b.Deduct(3)
	if got := b.Remaining(); got != 2 {
		t.Fatalf("remaining %d, want 2", got)
	}
	b.Deduct(10) // more than available
	if got := b.Remaining(); got != 0 {
		t.Fatalf("remaining %d, want 0", got)
	}
	if b.Allow(1) {
		t.Fatal("allowed on empty bucket")
	}
}

func TestRegistry_InvalidateRebuilds(t *testing.T) {
	r := NewRegistry()
	if !r.AllowRPM(7, 1) {
		t.Fatal("first request denied")
	}
	if r.AllowRPM(7, 1) {
		t.Fatal("second request allowed with rpm=1")
	}
	r.Invalidate(7)
	if !r.AllowRPM(7, 1) {
		t.Fatal("denied after invalidate")
	}
}
