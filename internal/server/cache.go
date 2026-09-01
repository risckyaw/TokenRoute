package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

// cacheEntry is one stored non-stream chat response.
type cacheEntry struct {
	body             []byte
	status           int
	contentType      string
	storedAt         time.Time
	promptTokens     int
	completionTokens int
	totalTokens      int
}

// RespCache is a per-process in-memory response cache.
// ponytail: capped at 1000 entries with random eviction; swap for an LRU
// when cache-hit ratio matters.
type RespCache struct {
	m   sync.Map // key string -> *cacheEntry
	ttl time.Duration
	n   atomic.Int64
}

const cacheCap = 1000

func newRespCache(ttlSeconds int) *RespCache {
	if ttlSeconds <= 0 {
		ttlSeconds = 300
	}
	return &RespCache{ttl: time.Duration(ttlSeconds) * time.Second}
}

// NewCache builds the response cache option value; ttlSeconds <= 0 = 300.
// Returns nil when disabled so Options.Cache stays nil.
func NewCache(enabled bool, ttlSeconds int) *RespCache {
	if !enabled {
		return nil
	}
	return newRespCache(ttlSeconds)
}

// cacheKey hashes the canonical request shape: route model, upstream model,
// messages, temperature. Array order inside messages is preserved by
// encoding/json; Go maps marshal with sorted keys, so the encoding is
// deterministic. max_tokens deliberately excluded.
func cacheKey(routeModel, upstreamModel string, body []byte) string {
	var parsed struct {
		Messages    []any `json:"messages"`
		Temperature any   `json:"temperature"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "" // uncacheable
	}
	canon, err := json.Marshal([]any{routeModel, upstreamModel, parsed.Messages, parsed.Temperature})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

func (c *RespCache) get(key string) *cacheEntry {
	if c == nil || key == "" {
		return nil
	}
	v, ok := c.m.Load(key)
	if !ok {
		return nil
	}
	e := v.(*cacheEntry)
	if time.Since(e.storedAt) > c.ttl {
		c.m.Delete(key)
		c.n.Add(-1)
		return nil
	}
	return e
}

func (c *RespCache) store(key string, e *cacheEntry) {
	if c == nil || key == "" {
		return
	}
	if _, loaded := c.m.LoadOrStore(key, e); !loaded {
		if c.n.Add(1) > cacheCap {
			c.m.Range(func(k, _ any) bool {
				c.m.Delete(k)
				c.n.Add(-1)
				return false // one random victim
			})
		}
	}
}
