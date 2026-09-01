package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/auth"
	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/provider/openai"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

func TestGroupAccess_Filtering(t *testing.T) {
	db, err := usage.OpenDB(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keys, _ := auth.NewStore(db)

	kVIP, _ := keys.Create(auth.Key{Name: "vip-user", Groups: []string{"vip"}, Enabled: true})
	_, _ = keys.Create(auth.Key{Name: "free-user", Groups: []string{"free"}, Enabled: true})

	provVIP := openai.New(openai.Config{Name: "p-vip", BaseURL: "http://example.com"})
	provFree := openai.New(openai.Config{Name: "p-free", BaseURL: "http://example.com"})

	rt := router.New([]provider.Provider{provVIP, provFree}, []*router.Route{
		{
			Model: "auto",
			Candidates: []router.Candidate{
				{Provider: provVIP, Model: "vip-m", Groups: []string{"vip"}},
				{Provider: provFree, Model: "free-m", Groups: []string{"free"}},
			},
		},
	})

	h := NewWithOptions(Options{
		Router: rt,
		Keys:   keys,
	})

	// VIP key only reaches p-vip (which errors with dial fail, meaning candidate p-vip was selected)
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+kVIP.Key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("X-TokenRoute-Decision") != "" && !strings.Contains(rec.Header().Get("X-TokenRoute-Decision"), "provider=p-vip") {
		t.Fatalf("expected p-vip in decision, got %v", rec.Header().Get("X-TokenRoute-Decision"))
	}

	// Unmatched group
	kOther, _ := keys.Create(auth.Key{Name: "other-user", Groups: []string{"enterprise"}, Enabled: true})
	req2, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("Authorization", "Bearer "+kOther.Key)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("expected 403 group_forbidden, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestModelMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["model"] != "deepseek-chat" {
			t.Errorf("expected mapped model deepseek-chat, got %v", req["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"total_tokens":10}}`)
	}))
	defer srv.Close()

	p := openai.New(openai.Config{Name: "test-prov", BaseURL: srv.URL})
	rt := router.New([]provider.Provider{p}, []*router.Route{
		{
			Model: "gpt-4o",
			Candidates: []router.Candidate{
				{Provider: p, Model: "gpt-4o"},
			},
		},
	})
	rt.SetModelMapping("test-prov", map[string]string{"gpt-4o": "deepseek-chat"})

	h := NewWithOptions(Options{Router: rt})
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("X-TokenRoute-Decision"), "model=deepseek-chat") {
		t.Fatalf("expected decision header to reflect mapped model, got %v", rec.Header().Get("X-TokenRoute-Decision"))
	}
}

func TestChannelTestEndpoint(t *testing.T) {
	db, err := usage.OpenDB(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keys, _ := auth.NewStore(db)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"pong"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	p := openai.New(openai.Config{Name: "prov-live", BaseURL: srv.URL})
	rt := router.New([]provider.Provider{p}, []*router.Route{
		{
			Model: "auto",
			Candidates: []router.Candidate{
				{Provider: p, Model: "test-m"},
			},
		},
	})

	h := NewWithOptions(Options{
		Router:   rt,
		Keys:     keys,
		AdminKey: "admin-secret",
	})

	req, _ := http.NewRequest(http.MethodPost, "/admin/providers/prov-live/test", nil)
	req.Header.Set("X-Admin-Key", "admin-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var res struct {
		OK        bool  `json:"ok"`
		Status    int   `json:"status"`
		LatencyMs int64 `json:"latency_ms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || !res.OK || res.Status != 200 {
		t.Fatalf("bad test response: %+v err: %v", res, err)
	}
}

func init() {
	_ = io.EOF
}
