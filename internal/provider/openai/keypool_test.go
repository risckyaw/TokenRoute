package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
)

// First key gets a 401 -> cooled; retry (same caller loop) uses the second key.
func TestKeyPool_FailoverToSecondKey(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		got = append(got, key)
		if key == "k1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	p := New(Config{Name: "t", BaseURL: srv.URL, APIKeys: []string{"k1", "k2"}})
	req := &provider.Request{Model: "m", Body: []byte(`{"model":"m"}`)}

	resp, err := p.ChatComplete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("first attempt status = %d, want 401", resp.StatusCode)
	}

	// Retry: k1 cooling, so k2 must be picked.
	resp, err = p.ChatComplete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second attempt status = %d, want 200", resp.StatusCode)
	}
	if fmt.Sprint(got) != "[k1 k2]" {
		t.Fatalf("keys used = %v, want [k1 k2]", got)
	}
}

func TestKeyPool_AllCoolingFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	p := New(Config{Name: "t", BaseURL: srv.URL, APIKey: "only"})
	resp, err := p.ChatComplete(context.Background(), &provider.Request{Model: "m", Body: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, err := p.ChatComplete(context.Background(), &provider.Request{Model: "m", Body: []byte(`{}`)}); err == nil {
		t.Fatal("want error when all keys cooling")
	}
}

func TestKeyPool_RoundRobin(t *testing.T) {
	pool := provider.NewKeyPool("a", "b")
	k1, _ := pool.Pick()
	k2, _ := pool.Pick()
	k3, _ := pool.Pick()
	if k1 != "a" || k2 != "b" || k3 != "a" {
		t.Fatalf("round robin = %q %q %q", k1, k2, k3)
	}
	pool.Cool("a")
	if !pool.Cooling("a") {
		t.Fatal("a should be cooling")
	}
	k, ok := pool.Pick()
	if !ok || k != "b" {
		t.Fatalf("pick after cooling a = %q,%v want b,true", k, ok)
	}
	pool.Cool("b")
	if _, ok := pool.Pick(); ok {
		t.Fatal("all cooling should return ok=false")
	}
}

// Fair-share: the key with fewer requests in its 60s window wins; ties
// rotate in round-robin order.
func TestKeyPool_FairShare(t *testing.T) {
	pool := provider.NewKeyPool("a", "b")
	pool.RecordUse("a")
	pool.RecordUse("a")
	k, ok := pool.Pick()
	if !ok || k != "b" {
		t.Fatalf("pick = %q,%v want b,true (a has more use)", k, ok)
	}
	pool.RecordUse(k)
	// Now a=2, b=1: b still lower.
	k, _ = pool.Pick()
	if k != "b" {
		t.Fatalf("pick = %q want b", k)
	}
	pool.RecordUse(k)
	pool.RecordUse(k)
	// a=2, b=3: a lower now.
	k, _ = pool.Pick()
	if k != "a" {
		t.Fatalf("pick = %q want a", k)
	}
	// Tie at zero recorded use after window would reset: round-robin order.
	fresh := provider.NewKeyPool("x", "y")
	a1, _ := fresh.Pick()
	a2, _ := fresh.Pick()
	if a1 != "x" || a2 != "y" {
		t.Fatalf("tie rotation = %q %q want x y", a1, a2)
	}
}
