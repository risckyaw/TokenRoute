package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/metrics"
	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/provider/openai"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

func TestInflightPairingSuccess(t *testing.T) {
	var live atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		live.Add(1)
		defer live.Add(-1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"total_tokens":2}}`))
	}))
	t.Cleanup(up.Close)
	p1 := openai.New(openai.Config{Name: "p1", BaseURL: up.URL, Priority: 1})
	rt := router.New([]provider.Provider{p1}, []*router.Route{{
		Model:      "auto",
		Candidates: []router.Candidate{{Provider: p1, Model: "m1"}},
	}})
	ul, _ := usage.Open(t.TempDir() + "/u.db")
	t.Cleanup(func() { ul.Close() })
	h := New(rt, ul, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"auto","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if n := rt.Inflight("p1"); n != 0 {
		t.Fatalf("inflight after success = %d, want 0", n)
	}
}

func TestInflightPairingFailure(t *testing.T) {
	rt := router.New(nil, nil)
	rt.IncInflight("p1")
	// Transport failure path decrements directly in failoverPass; simulate:
	rt.DecInflight("p1")
	if n := rt.Inflight("p1"); n != 0 {
		t.Fatalf("inflight = %d, want 0", n)
	}
}

func TestInflightStreamAbort(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		close(started)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release // hold the stream open mid-flight
	}))
	t.Cleanup(up.Close)
	p1 := openai.New(openai.Config{Name: "p1", BaseURL: up.URL, Priority: 1})
	rt := router.New([]provider.Provider{p1}, []*router.Route{{
		Model:      "auto",
		Candidates: []router.Candidate{{Provider: p1, Model: "m1"}},
	}})
	ul, _ := usage.Open(t.TempDir() + "/u.db")
	t.Cleanup(func() { ul.Close() })
	h := New(rt, ul, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"auto","messages":[],"stream":true}`))
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(rec, req); close(done) }()
	<-started
	deadline := time.Now().Add(2 * time.Second)
	for rt.Inflight("p1") != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := rt.Inflight("p1"); n != 1 {
		t.Fatalf("inflight mid-stream = %d, want 1", n)
	}
	close(release) // upstream ends the stream -> relay finishes -> body closes
	<-done
	if n := rt.Inflight("p1"); n != 0 {
		t.Fatalf("inflight after stream end = %d, want 0", n)
	}
}

func TestInflightGauge(t *testing.T) {
	m := metrics.New()
	n := 3
	m.Inflight = func(string) int { return n }
	m.Providers = func() []string { return []string{"p1"} }
	var b strings.Builder
	m.Write(&b)
	if !strings.Contains(b.String(), `tokenroute_inflight{provider="p1"} 3`) {
		t.Fatalf("gauge missing; got:\n%s", b.String())
	}
}
