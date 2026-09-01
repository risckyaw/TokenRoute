package provider

import (
	"sync"
	"sync/atomic"
	"time"
)

// KeyCooldown is how long a key is skipped after an upstream 401/429.
const KeyCooldown = 60 * time.Second

// KeyPool round-robins API keys per request, skipping keys in cooldown.
type KeyPool struct {
	keys []string
	next atomic.Uint64

	mu   sync.Mutex
	cool map[string]time.Time // key -> cooling until
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
	return &KeyPool{keys: out, cool: map[string]time.Time{}}
}

// Pick returns the next non-cooling key, or ("", false) when all keys are
// cooling. An empty pool returns ("", true) — no key needed.
func (p *KeyPool) Pick() (string, bool) {
	if len(p.keys) == 0 {
		return "", true
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for range p.keys {
		k := p.keys[int(p.next.Add(1)-1)%len(p.keys)]
		if until, ok := p.cool[k]; ok && now.Before(until) {
			continue
		}
		return k, true
	}
	return "", false
}

// Cool marks a key unusable for KeyCooldown (upstream 401/429).
func (p *KeyPool) Cool(key string) {
	if key == "" {
		return
	}
	p.mu.Lock()
	p.cool[key] = time.Now().Add(KeyCooldown)
	p.mu.Unlock()
}

// Cooling reports whether the key is currently in cooldown (for tests).
func (p *KeyPool) Cooling(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.cool[key]
	return ok && time.Now().Before(until)
}
