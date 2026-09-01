package server

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/provider/openai"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// upstream returns an httptest server answering /chat/completions with the
// given status and body.
func upstream(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

// deadURL returns the URL of a listener that is immediately closed
// (connection refused).
func deadURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr
}

// setup builds the real gateway handler with two candidates, p1 preferred.
func setup(t *testing.T, base1, base2 string, circuit *router.CircuitConfig) http.Handler {
	t.Helper()
	p1 := openai.New(openai.Config{Name: "p1", BaseURL: base1, Priority: 1, TimeoutMs: 5000})
	p2 := openai.New(openai.Config{Name: "p2", BaseURL: base2, Priority: 2, TimeoutMs: 5000})
	routes := []*router.Route{{
		Model: "auto",
		Candidates: []router.Candidate{
			{Provider: p1, Model: "m1"},
			{Provider: p2, Model: "m2"},
		},
	}}
	rt := router.New([]provider.Provider{p1, p2}, routes)
	if circuit != nil {
		rt.SetCircuit("p1", *circuit)
	}
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	return New(rt, ul, nil)
}

func post(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"auto","messages":[]}`)))
	return rec
}

func TestFailover_Upstream500Then200(t *testing.T) {
	bad := upstream(t, 500, `{"error":"boom"}`)
	good := upstream(t, 200, `{"id":"ok","usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	h := setup(t, bad.URL, good.URL, nil)

	rec := post(t, h)
	if rec.Code != 200 {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.ID != "ok" {
		t.Fatalf("body %q not from second upstream", rec.Body.String())
	}
}

func TestFailover_ConnectionRefused(t *testing.T) {
	good := upstream(t, 200, `{"id":"ok"}`)
	h := setup(t, deadURL(t), good.URL, nil)

	rec := post(t, h)
	if rec.Code != 200 {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestFailover_NoFailoverOn400(t *testing.T) {
	first := upstream(t, 400, `{"error":{"message":"bad request"}}`)
	second := upstream(t, 200, `{"id":"ok"}`)
	h := setup(t, first.URL, second.URL, nil)

	rec := post(t, h)
	if rec.Code != 400 {
		t.Fatalf("status %d, want 400 relayed from first upstream", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "bad request") {
		t.Fatalf("body %q, want first upstream's 400 body", rec.Body.String())
	}
}

func TestFailover_AllFail502(t *testing.T) {
	bad1 := upstream(t, 503, `{"error":"down1"}`)
	bad2 := upstream(t, 500, `{"error":"down2"}`)
	h := setup(t, bad1.URL, bad2.URL, nil)

	rec := post(t, h)
	// All candidates returned retryable statuses: last one relayed as-is.
	if rec.Code != 500 {
		t.Fatalf("status %d, want 500 (last upstream error relayed)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "down2") {
		t.Fatalf("body %q, want last upstream's error body", rec.Body.String())
	}
}

func TestFailover_AllTransportErrors502(t *testing.T) {
	h := setup(t, deadURL(t), deadURL(t), nil)

	rec := post(t, h)
	if rec.Code != 502 {
		t.Fatalf("status %d, want 502", rec.Code)
	}
	var got struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Error.Type != "upstream_error" {
		t.Fatalf("body %q, want upstream_error JSON", rec.Body.String())
	}
}

func TestFailover_CircuitOpensAndSkips(t *testing.T) {
	badHits := 0
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badHits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer bad.Close()
	good := upstream(t, 200, `{"id":"ok"}`)
	// Threshold 2: after two failed attempts the circuit opens.
	h := setup(t, bad.URL, good.URL, &router.CircuitConfig{FailureThreshold: 2, CooldownMs: 600000})

	for i := 0; i < 2; i++ {
		if rec := post(t, h); rec.Code != 200 {
			t.Fatalf("request %d: status %d, want 200", i+1, rec.Code)
		}
	}
	if badHits != 2 {
		t.Fatalf("bad upstream hit %d times, want 2", badHits)
	}
	// Circuit now open: next request must skip p1 entirely.
	rec := post(t, h)
	if rec.Code != 200 {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if badHits != 2 {
		t.Fatalf("bad upstream hit %d times after circuit opened, want still 2", badHits)
	}
}
