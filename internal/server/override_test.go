package server

import (
	"encoding/json"
	"io"
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

func TestParamOps_SetDeletePrecedence(t *testing.T) {
	body := `{"model":"m","temperature":0.5,"top_k":40,"max_tokens":100}`
	out := ParamOps([]byte(body),
		map[string]any{"temperature": 1.0, "extra": "yes"},
		[]string{"top_k"},
		map[string]any{"temperature": 0.1, "max_tokens": 4096},
	)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["temperature"] != 0.1 {
		t.Errorf("candidate must win: temperature=%v", m["temperature"])
	}
	if m["max_tokens"] != float64(4096) {
		t.Errorf("candidate set max_tokens=%v", m["max_tokens"])
	}
	if m["extra"] != "yes" {
		t.Errorf("provider set lost: extra=%v", m["extra"])
	}
	if _, ok := m["top_k"]; ok {
		t.Error("param_delete must remove top_k")
	}
	if m["model"] != "m" {
		t.Errorf("untouched key changed: model=%v", m["model"])
	}
}

func TestParamOps_NonObjectUntouched(t *testing.T) {
	for _, in := range []string{`[1,2]`, `"str"`, `123`, `not json`} {
		out := ParamOps([]byte(in), map[string]any{"a": 1}, []string{"b"}, map[string]any{"c": 2})
		if string(out) != in {
			t.Errorf("non-object body %q rewritten to %q", in, out)
		}
	}
}

func TestParamOps_NoOps(t *testing.T) {
	in := `{"a":1}`
	if out := ParamOps([]byte(in), nil, nil, nil); string(out) != in {
		t.Errorf("no ops must return body unchanged, got %q", out)
	}
}

func TestHeaderPassMatch(t *testing.T) {
	pats := []string{"X-Custom-*", "X-Exact"}
	for name, want := range map[string]bool{
		"X-Custom-Foo": true, "x-custom-bar": true, "X-Exact": true,
		"x-exact": true, "X-Other": false, "X-Custom": false,
	} {
		if got := HeaderPassMatch(pats, name); got != want {
			t.Errorf("match(%q) = %v, want %v", name, got, want)
		}
	}
}

// End-to-end: provider param/header override + candidate precedence reach the
// upstream request.
func TestOverrides_EndToEnd(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"max_tokens":4096`) {
			t.Errorf("candidate param_override missing in upstream body: %s", b)
		}
		if strings.Contains(string(b), `"top_k"`) {
			t.Errorf("param_delete key still present: %s", b)
		}
		if !strings.Contains(string(b), `"temperature":1`) {
			t.Errorf("provider param_override missing: %s", b)
		}
		if r.Header.Get("X-Title") != "tokenroute" {
			t.Errorf("header_override not set: %v", r.Header)
		}
		if r.Header.Get("X-Custom-Trace") != "abc" {
			t.Errorf("header_pass header missing: %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()

	p := openai.New(openai.Config{Name: "ovr", BaseURL: up.URL, Priority: 1, TimeoutMs: 5000})
	rt := router.New([]provider.Provider{p}, []*router.Route{{
		Model: "auto",
		Candidates: []router.Candidate{{
			Provider: p, Model: "m",
			ParamOverride: map[string]any{"max_tokens": 4096},
		}},
	}})
	rt.SetProviderOverride("ovr", router.ProviderOverride{
		ParamSet:   map[string]any{"temperature": 1.0},
		ParamDel:   []string{"top_k"},
		HeaderSet:  map[string]string{"X-Title": "tokenroute"},
		HeaderPass: []string{"X-Custom-*"},
	})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	h := New(rt, ul, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"auto","temperature":0.5,"top_k":40,"max_tokens":100}`))
	req.Header.Set("X-Custom-Trace", "abc")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

// header_pass must resurrect headers the default blocklist drops
// (X-Timeout-Ms here stands in for a gateway-local control header).
func TestHeaderPass_ResurrectsBlocked(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Timeout-Ms") != "45000" {
			t.Errorf("blocked header not resurrected: %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()
	p := openai.New(openai.Config{Name: "ovr", BaseURL: up.URL, Priority: 1, TimeoutMs: 5000})
	rt := router.New([]provider.Provider{p}, []*router.Route{{
		Model:      "auto",
		Candidates: []router.Candidate{{Provider: p, Model: "m"}},
	}})
	rt.SetProviderOverride("ovr", router.ProviderOverride{HeaderPass: []string{"X-Timeout-Ms"}})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	h := New(rt, ul, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"auto"}`))
	req.Header.Set("X-Timeout-Ms", "45000")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}
