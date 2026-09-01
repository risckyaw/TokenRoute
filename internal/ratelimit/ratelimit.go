// Package ratelimit implements per-key token buckets for RPM and TPM.
package ratelimit

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// TokenBucket is a thread-safe token bucket: capacity tokens, refilled at
// rate tokens/second. Capacity <= 0 means unlimited (Allow never denies).
type TokenBucket struct {
	mu       sync.Mutex
	capacity float64
	rate     float64 // tokens per second
	tokens   float64
	last     time.Time
	now      func() time.Time // injectable for tests
}

// NewBucket returns a bucket with the given capacity and refill rate.
// capacity <= 0 = unlimited.
func NewBucket(capacity int, ratePerSec float64) *TokenBucket {
	return &TokenBucket{
		capacity: float64(capacity),
		rate:     ratePerSec,
		tokens:   float64(capacity),
		last:     time.Now(),
		now:      time.Now,
	}
}

func (b *TokenBucket) refill() {
	now := b.now()
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * b.rate
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = now
	}
}

// Allow takes n tokens if available.
func (b *TokenBucket) Allow(n int) bool {
	if b.capacity <= 0 {
		return true // unlimited
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	f := float64(n)
	if b.tokens >= f {
		b.tokens -= f
		return true
	}
	return false
}

// Remaining reports the currently available tokens (after refill).
func (b *TokenBucket) Remaining() int {
	if b.capacity <= 0 {
		return 1 << 62 // unlimited
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	return int(b.tokens)
}

// Deduct removes n tokens unconditionally, flooring at 0. Used to charge
// actual usage after a request completes.
func (b *TokenBucket) Deduct(n int) {
	if b.capacity <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	b.tokens -= float64(n)
	if b.tokens < 0 {
		b.tokens = 0
	}
}

type buckets struct {
	rpm *TokenBucket
	tpm *TokenBucket
}

// Registry lazily keeps RPM/TPM buckets per key ID, plus per-(key,model)
// RPM buckets.
type Registry struct {
	mu       sync.Mutex
	byID     map[int64]*buckets
	modelRPM map[string]*TokenBucket // "keyID:model" -> bucket
}

func NewRegistry() *Registry {
	return &Registry{byID: map[int64]*buckets{}, modelRPM: map[string]*TokenBucket{}}
}

// AllowRPM takes one request token; always true when rpm is 0.
func (r *Registry) AllowRPM(keyID int64, rpm int) bool {
	if rpm <= 0 {
		return true
	}
	return r.get(keyID, rpm, 0).rpm.Allow(1)
}

// TPMRemaining reports available TPM tokens; huge when tpm is 0.
func (r *Registry) TPMRemaining(keyID int64, tpm int) int {
	if tpm <= 0 {
		return 1 << 62
	}
	return r.get(keyID, 0, tpm).tpm.Remaining()
}

// DeductTPM charges n actual tokens after a request; no-op when tpm is 0.
func (r *Registry) DeductTPM(keyID int64, tpm, n int) {
	if tpm <= 0 || n <= 0 {
		return
	}
	r.get(keyID, 0, tpm).tpm.Deduct(n)
}

// get returns the buckets for keyID, creating them when missing or when the
// limits changed (rpm/tpm mutated -> fresh buckets).
func (r *Registry) get(keyID int64, rpm, tpm int) *buckets {
	r.mu.Lock()
	defer r.mu.Unlock()
	bs, ok := r.byID[keyID]
	if !ok {
		bs = &buckets{}
		r.byID[keyID] = bs
	}
	if rpm > 0 && (bs.rpm == nil || bs.rpm.capacity != float64(rpm)) {
		bs.rpm = NewBucket(rpm, float64(rpm)/60)
	}
	if tpm > 0 && (bs.tpm == nil || bs.tpm.capacity != float64(tpm)) {
		bs.tpm = NewBucket(tpm, float64(tpm)/60)
	}
	return bs
}

// Invalidate drops the buckets for a key (called on admin mutation).
func (r *Registry) Invalidate(keyID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, keyID)
	prefix := strconv.FormatInt(keyID, 10) + ":"
	for k := range r.modelRPM {
		if strings.HasPrefix(k, prefix) {
			delete(r.modelRPM, k)
		}
	}
}

// AllowModelRPM takes one request token from the per-(key,model) bucket of
// capacity modelRPM; always true when modelRPM is 0.
func (r *Registry) AllowModelRPM(keyID int64, model string, modelRPM int) bool {
	if modelRPM <= 0 {
		return true
	}
	ns := strconv.FormatInt(keyID, 10) + ":" + model
	r.mu.Lock()
	b, ok := r.modelRPM[ns]
	if !ok || b.capacity != float64(modelRPM) {
		b = NewBucket(modelRPM, float64(modelRPM)/60)
		if r.modelRPM == nil {
			r.modelRPM = map[string]*TokenBucket{}
		}
		r.modelRPM[ns] = b
	}
	r.mu.Unlock()
	return b.Allow(1)
}
