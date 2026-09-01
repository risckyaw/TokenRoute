package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// fakeProvider streams a canned response; in small chunks when nonStream=false.
type fakeProvider struct {
	body        string
	nonStream   bool
	lastReqBody []byte
}

func (f *fakeProvider) Name() string      { return "fake" }
func (f *fakeProvider) Priority() int     { return 1 }
func (f *fakeProvider) ModelsURL() string { return "" }
func (f *fakeProvider) Models(context.Context) ([]string, error) {
	return nil, nil
}
func (f *fakeProvider) ChatComplete(_ context.Context, req *provider.Request) (*http.Response, error) {
	f.lastReqBody = req.Body
	ct := "text/event-stream"
	if f.nonStream {
		ct = "application/json"
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{ct}},
			Body:       io.NopCloser(strings.NewReader(f.body)),
		}, nil
	}
	pr, pw := io.Pipe()
	go func() {
		data := []byte(f.body)
		for len(data) > 0 {
			n := 7 // tiny writes to exercise line-splitting
			if n > len(data) {
				n = len(data)
			}
			if _, err := pw.Write(data[:n]); err != nil {
				return
			}
			data = data[n:]
			time.Sleep(time.Millisecond)
		}
		pw.Close()
	}()
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       pr,
	}, nil
}

func TestRelayStream_TokenCounting(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"1","choices":[{"delta":{"content":"hi"}}]}`,
		``,
		`data: {"id":"1","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}}`,
		`data: [DONE]`,
		``,
	}, "\n") + "\n"

	fp := &fakeProvider{body: sse}
	rt := router.New([]provider.Provider{fp}, []*router.Route{{
		Model:      "auto",
		Candidates: []router.Candidate{{Provider: fp, Model: "up-model"}},
	}})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	prices := map[string]usage.Price{"up-model": {PromptPer1M: 1.0, CompletionPer1M: 2.0}}

	h := New(rt, ul, prices)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"auto","stream":true}`))
	h.ServeHTTP(rec, req)

	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("missing X-Request-Id")
	}
	if rec.Body.String() != sse {
		t.Fatalf("body not relayed verbatim:\ngot  %q\nwant %q", rec.Body.String(), sse)
	}

	entries, err := ul.QueryRecent(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.PromptTokens != 12 || e.CompletionTokens != 7 || e.TotalTokens != 19 {
		t.Fatalf("bad tokens: %+v", e)
	}
	if e.CostUSD == nil {
		t.Fatal("cost not computed")
	}
	want := 12.0/1e6*1.0 + 7.0/1e6*2.0
	if diff := *e.CostUSD - want; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("bad cost: got %v want %v", *e.CostUSD, want)
	}
	if e.VirtualModel != "auto" || e.Model != "up-model" || !e.Stream || e.Status != 200 {
		t.Fatalf("bad entry: %+v", e)
	}
}

func TestRelayFull_NonStreamUsage(t *testing.T) {
	fp := &fakeProvider{body: `{"id":"1","usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`}
	fp.nonStream = true
	rt := router.New([]provider.Provider{fp}, []*router.Route{{
		Model:      "auto",
		Candidates: []router.Candidate{{Provider: fp, Model: "up-model"}},
	}})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	h := New(rt, ul, nil) // no prices -> cost must be nil

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"auto"}`)))

	if rec.Body.String() != fp.body {
		t.Fatalf("body mismatch: %q", rec.Body.String())
	}
	entries, err := ul.QueryRecent(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.TotalTokens != 7 || e.Stream || e.CostUSD != nil {
		t.Fatalf("bad entry: %+v", e)
	}
}

func TestUsageRecentEndpoint(t *testing.T) {
	fp := &fakeProvider{body: "data: [DONE]\n"}
	rt := router.New([]provider.Provider{fp}, nil)
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	h := New(rt, ul, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/usage/recent?limit=10", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type %q", ct)
	}
}
