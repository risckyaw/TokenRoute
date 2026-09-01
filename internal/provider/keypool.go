package provider

import (
	"sync"
	"sync/atomic"
	"time"
)

// KeyCooldown is how long a key is skipped after an upstream 401/429.
const KeyCooldown = 60 * time.Second

// KeyPool picks API keys fairly, skipping keys in cooldown.
//
// Fair-share: among non-cooling keys the one with the lowest REQUEST COUNT
// in its own 60s tumbling window wins (ties break in round-robin order).
// Request count is used instead of tokens because the passthrough providers
// do not parse response usage at the key level; call RecordUse after a
// successful (non-401/429) response.
type KeyPool struct {
	keys []string
	next atomic.Uint64

	mu   sync.Mutex
	cool map[string]time.Time  // key -> cooling until
	use  map[string]*keyWindow // key -> 60s tumbling request count
}

type keyWindow struct {
	start time.Time
	reqs  int
}

// NewKeyPool builds a pool; empty/blank keys are dropped. A nil/empty pool
// means "no auth key" (e.g. Ollama): Pick returns "".
func NewKeyPool(keys ...string) *KeyPool {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if k != "" {
			out = append(out, k)
		}
	}
	return &KeyPool{keys: out, cool: map[string]time.Time{}, use: map[string]*keyWindow{}}
}

// Pick returns the least-used non-cooling key, or ("", false) when all keys
// are cooling. An empty pool returns ("", true) — no key needed.
func (p *KeyPool) Pick() (string, bool) {
	if len(p.keys) == 0 {
		return "", true
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	// Round-robin scan order so ties rotate.
	start := int(p.next.Load() % uint64(len(p.keys)))
	best := -1
	bestReqs := 0
	for i := range p.keys {
		idx := (start + i) % len(p.keys)
		k := p.keys[idx]
		if until, ok := p.cool[k]; ok && now.Before(until) {
			continue
		}
		reqs := p.windowReqs(k, now)
		if best < 0 || reqs < bestReqs {
			best, bestReqs = idx, reqs
		}
	}
	if best < 0 {
		return "", false
	}
	p.next.Store(uint64(best + 1))
	return p.keys[best], true
}

// windowReqs returns the key's current-window request count, resetting the
// window when 60s elapsed. Caller holds p.mu.
func (p *KeyPool) windowReqs(k string, now time.Time) int {
	w := p.use[k]
	if w == nil {
		return 0
	}
	if now.Sub(w.start) >= 60*time.Second {
		w.start, w.reqs = now, 0
	}
	return w.reqs
}

// RecordUse increments the key's request count for the current 60s window.
// Call after a response that did not cool the key.
func (p *KeyPool) RecordUse(key string) {
	if key == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	w := p.use[key]
	if w == nil {
		w = &keyWindow{start: now}
		p.use[key] = w
	}
	if now.Sub(w.start) >= 60*time.Second {
		w.start, w.reqs = now, 0
	}
	w.reqs++
}

// Cool marks a key unusable for KeyCooldown (upstream 401/429).
func (p *KeyPool) Cool(key string) {
	p.CoolUntil(key, time.Now().Add(KeyCooldown))
}

// CoolUntil marks a key unusable until the given time (upstream-signalled
// rate-limit reset). Zero cap: the caller bounds the duration.
func (p *KeyPool) CoolUntil(key string, until time.Time) {
	if key == "" {
		return
	}
	p.mu.Lock()
	p.cool[key] = until
	p.mu.Unlock()
}

// EarliestCooldown returns the soonest cooldown expiry across keys
// (zero when none are cooling).
func (p *KeyPool) EarliestCooldown() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	var earliest time.Time
	now := time.Now()
	for k, until := range p.cool {
		if now.After(until) {
			delete(p.cool, k)
			continue
		}
		if earliest.IsZero() || until.Before(earliest) {
			earliest = until
		}
	}
	return earliest
}

// Cooling reports whether the key is currently in cooldown (for tests).
func (p *KeyPool) Cooling(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.cool[key]
	return ok && time.Now().Before(until)
}
