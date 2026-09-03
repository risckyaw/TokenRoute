package usage

import "sync"

// PriceStore is a goroutine-safe shared store for per-model pricing.
// Router, server, catalog sync, and pricing sync all use one instance so
// reads and writes are serialized under the same RWMutex.
type PriceStore struct {
	mu sync.RWMutex
	m  map[string]Price
}

// NewPriceStore creates a store seeded with the given entries (may be nil).
func NewPriceStore(seed map[string]Price) *PriceStore {
	m := make(map[string]Price, len(seed))
	for k, v := range seed {
		m[k] = v
	}
	return &PriceStore{m: m}
}

// Get returns the price for a model; ok=false when unknown.
func (ps *PriceStore) Get(model string) (Price, bool) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	p, ok := ps.m[model]
	return p, ok
}

// Set adds or overwrites a model's price.
func (ps *PriceStore) Set(model string, p Price) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.m[model] = p
}

// Has reports whether a model has any price entry.
func (ps *PriceStore) Has(model string) bool {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	_, ok := ps.m[model]
	return ok
}

// Snapshot returns a copy of the full map (safe for callers to mutate).
func (ps *PriceStore) Snapshot() map[string]Price {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	out := make(map[string]Price, len(ps.m))
	for k, v := range ps.m {
		out[k] = v
	}
	return out
}
