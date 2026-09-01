package router

import (
	"strings"
	"testing"
	"time"
)

func TestCachePrefixHash(t *testing.T) {
	if CachePrefixHash([]byte(`{"messages":[]}`)) != 0 {
		t.Fatal("empty messages must not be cacheable")
	}
	short := `{"messages":[{"role":"system","content":"hi"},{"role":"user","content":"yo"}]}`
	if CachePrefixHash([]byte(short)) != 0 {
		t.Fatal("short implicit prefix must not be cacheable (<1024 bytes)")
	}
	big := `{"messages":[{"role":"system","content":"` + strings.Repeat("s", 1100) + `"},{"role":"user","content":"u"}]}`
	if CachePrefixHash([]byte(big)) == 0 {
		t.Fatal(">=1024-byte system+user prefix must be cacheable")
	}
	marked := `{"messages":[{"role":"system","content":"s"},{"role":"user","content":"u","cache_control":{"type":"ephemeral"}}]}`
	if CachePrefixHash([]byte(marked)) == 0 {
		t.Fatal("explicit cache_control must be cacheable regardless of size")
	}
	// Block form (Anthropic content arrays).
	blocks := `{"messages":[{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}]}]}`
	if CachePrefixHash([]byte(blocks)) == 0 {
		t.Fatal("content-block cache_control must be detected")
	}
	// Stability: same input, same hash; different suffix beyond marker same prefix.
	h1 := CachePrefixHash([]byte(marked))
	marked2 := `{"messages":[{"role":"system","content":"s"},{"role":"user","content":"u","cache_control":{"type":"ephemeral"}},{"role":"user","content":"later"}]}`
	if CachePrefixHash([]byte(marked2)) != h1 {
		t.Fatal("messages after the last cache_control marker must not change the hash")
	}
}

func TestAffinityCacheTTL(t *testing.T) {
	ac := NewAffinityCache(time.Hour)
	now := time.Now()
	ac.now = func() time.Time { return now }
	ac.Put(42, "p1", "m1")
	pin, ok := ac.Get(42)
	if !ok || pin.Provider != "p1" || pin.Model != "m1" {
		t.Fatalf("Get = %+v %v", pin, ok)
	}
	ac.now = func() time.Time { return now.Add(61 * time.Minute) }
	if _, ok := ac.Get(42); ok {
		t.Fatal("pin must expire after TTL")
	}
}

func TestPinByAffinity(t *testing.T) {
	hi := &fakeProvider{name: "hi", priority: 1}
	lo := &fakeProvider{name: "lo", priority: 10}
	r := New(nil, nil)
	ac := NewAffinityCache(time.Hour)
	r.SetAffinity(ac)

	cands := []Candidate{{Provider: hi, Model: "a"}, {Provider: lo, Model: "b"}}
	ac.Put(7, "lo", "b")
	if !r.PinByAffinity(cands, 7) {
		t.Fatal("pin hit expected")
	}
	if cands[0].Provider.Name() != "lo" {
		t.Fatalf("pinned candidate must go first: %v", cands[0].Provider.Name())
	}
	// Miss: order untouched, false.
	cands2 := []Candidate{{Provider: hi, Model: "a"}, {Provider: lo, Model: "b"}}
	if r.PinByAffinity(cands2, 999) {
		t.Fatal("no pin -> no hit")
	}
	if cands2[0].Provider.Name() != "hi" {
		t.Fatal("miss must keep order")
	}
	// Pinned provider not in list (filtered by circuit/tags): fall through.
	ac.Put(8, "ghost", "x")
	if r.PinByAffinity(cands2, 8) {
		t.Fatal("pin for absent candidate must not hit")
	}
	// Disabled router (nil cache): never hits.
	r2 := New(nil, nil)
	if r2.PinByAffinity(cands2, 7) {
		t.Fatal("nil affinity cache must not hit")
	}
}
