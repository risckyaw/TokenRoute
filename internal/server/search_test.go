package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/search"
)

type fakeBackend struct {
	name string
	res  []search.Result
	err  error
}

func (f fakeBackend) Name() string { return f.name }
func (f fakeBackend) Search(_ context.Context, _ string, _ int) ([]search.Result, error) {
	return f.res, f.err
}

var errFake = errors.New("boom")

func TestSearchHandler(t *testing.T) {
	rt := router.New(nil, nil)
	okBackend := fakeBackend{name: "fake", res: []search.Result{{Title: "t", URL: "https://x"}}}
	h := NewWithOptions(Options{Router: rt, SearchBackends: []search.Backend{okBackend}})

	req := httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(`{"query":"hello","max_results":3}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Backend string          `json:"backend"`
		Results []search.Result `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Backend != "fake" || len(out.Results) != 1 {
		t.Fatalf("bad payload: %s", rec.Body)
	}

	// Empty query -> 400.
	req = httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(`{"query":""}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}

	// No backends -> 501.
	h = NewWithOptions(Options{Router: rt})
	req = httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(`{"query":"x"}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("want 501, got %d", rec.Code)
	}

	// First backend fails -> fall through to the second.
	h = NewWithOptions(Options{Router: rt, SearchBackends: []search.Backend{
		fakeBackend{name: "broken", err: errFake}, okBackend,
	}})
	req = httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(`{"query":"x"}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"backend":"fake"`) {
		t.Fatalf("fallback failed: %d %s", rec.Code, rec.Body)
	}
}
