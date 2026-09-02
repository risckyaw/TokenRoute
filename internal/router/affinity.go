package router

import (
	"encoding/json"
	"hash/fnv"
	"sync"
	"time"
)

// CachePrefixHash computes a stable hash of the cacheable prompt prefix
// (LiteLLM prompt_caching_cache): messages up to and including the last
// message carrying an explicit cache_control marker; when none, the system
// message plus first user message — but only when their serialized size
// reaches 1024 bytes (approximation of provider-side minimum cacheable
// prefix, e.g. Anthropic 1024 tokens for most models). 0 = not cacheable.
func CachePrefixHash(body []byte) uint64 {
	var parsed struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Messages) == 0 {
		return 0
	}
	// Explicit cache_control: hash through the last marked message.
	lastMarked := -1
	for i, raw := range parsed.Messages {
		var m struct {
			CacheControl json.RawMessage `json:"cache_control"`
			Content      json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		if len(m.CacheControl) > 0 {
			lastMarked = i
			continue
		}
		// Anthropic content-block form: content: [..., {cache_control: {...}}]
		var blocks []struct {
			CacheControl json.RawMessage `json:"cache_control"`
		}
		if err := json.Unmarshal(m.Content, &blocks); err == nil {
			for _, b := range blocks {
				if len(b.CacheControl) > 0 {
					lastMarked = i
					break
				}
			}
		}
	}
	h := fnv.New64a()
	if lastMarked >= 0 {
		for _, raw := range parsed.Messages[:lastMarked+1] {
			_, _ = h.Write(raw)
			_, _ = h.Write([]byte{0})
		}
		return h.Sum64()
	}
	// Implicit prefix: system + first user message, >= 1024 bytes serialized.
	var prefix []json.RawMessage
	size := 0
	for _, raw := range parsed.Messages {
		var m struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return 0
		}
		switch {
		case m.Role == "system" && len(prefix) == 0:
			prefix = append(prefix, raw)
			size += len(raw)
		case m.Role == "user" && len(prefix) >= 1:
			prefix = append(prefix, raw)
			size += len(raw)
		}
		if len(prefix) == 2 {
			break
		}
	}
	if len(prefix) < 2 || size < 1024 {
		return 0
	}
	for _, raw := range prefix {
		_, _ = h.Write(raw)
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64()
}

// AffinityPin remembers which provider+model served a prompt prefix
// (provider-side prompt caches hit when the same prefix returns).
type AffinityPin struct {
	Provider  string
	Model     string
	ExpiresAt time.Time
}

// AffinityCache is an in-memory prefix->pin map with TTL (LiteLLM
// prompt_caching_cache TTL 1h). Zero value unusable; NewAffinityCache.
type AffinityCache struct {
	mu  sync.Mutex
	m   map[uint64]AffinityPin
	ttl time.Duration
	now func() time.Time // injectable for tests
}

func NewAffinityCache(ttl time.Duration) *AffinityCache {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &AffinityCache{m: map[uint64]AffinityPin{}, ttl: ttl, now: time.Now}
}

// Get returns the pin for a prefix hash (nil + false when absent/expired).
func (a *AffinityCache) Get(hash uint64) (AffinityPin, bool) {
	if a == nil || hash == 0 {
		return AffinityPin{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.m[hash]
	if !ok {
		return AffinityPin{}, false
	}
	if a.now().After(p.ExpiresAt) {
		delete(a.m, hash)
		return AffinityPin{}, false
	}
	return p, true
}

// Put records prefix -> provider+model for the TTL.
func (a *AffinityCache) Put(hash uint64, providerName, model string) {
	a.PutTTL(hash, providerName, model, 0)
}

// PutTTL is Put with a per-entry TTL override (0 = cache default).
func (a *AffinityCache) PutTTL(hash uint64, providerName, model string, ttl time.Duration) {
	if a == nil || hash == 0 {
		return
	}
	if ttl <= 0 {
		ttl = a.ttl
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// ponytail: unbounded map keyed by content hash; add an LRU cap when
	// cardinality matters (entries expire after ttl, bounding live size).
	a.m[hash] = AffinityPin{Provider: providerName, Model: model, ExpiresAt: a.now().Add(ttl)}
}

// HeaderKeyHash hashes a key-header value as an affinity key (new-api
// channel affinity: session/thread ids pin to one channel). Distinct
// namespace from prefix hashes to avoid cross-source collisions.
func HeaderKeyHash(value string) uint64 {
	if value == "" {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte("hdr:"))
	_, _ = h.Write([]byte(value))
	return h.Sum64()
}
