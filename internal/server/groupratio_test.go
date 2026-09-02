package server

import (
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/auth"
	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

func TestGroupRatioMultiplier(t *testing.T) {
	ratios := map[string]float64{"vip": 1.0, "free": 1.2, "us": 2.0}
	// Single group.
	if m := groupRatioMultiplier(ratios, []string{"vip"}, []string{"vip", "us"}); m != 1.0 {
		t.Fatalf("single vip = %v, want 1.0", m)
	}
	// Multi-group product (vip × us).
	if m := groupRatioMultiplier(ratios, []string{"vip", "us"}, []string{"vip", "us"}); m != 2.0 {
		t.Fatalf("vip×us = %v, want 2.0", m)
	}
	// No intersection = 1.0.
	if m := groupRatioMultiplier(ratios, []string{"free"}, []string{"vip"}); m != 1.0 {
		t.Fatalf("no intersection = %v, want 1.0", m)
	}
	// Group not in ratio table contributes nothing.
	if m := groupRatioMultiplier(ratios, []string{"vip", "unknown"}, []string{"vip", "unknown"}); m != 1.0 {
		t.Fatalf("unknown group = %v, want 1.0", m)
	}
}

func groupRatioSetup(t *testing.T, ratios map[string]float64) (http.Handler, *usage.Logger, *auth.Store) {
	t.Helper()
	body := `{"id":"1","model":"up","choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":0,"total_tokens":1000}}`
	fp := &fakeProvider{body: body, nonStream: true}
	rt := router.New([]provider.Provider{fp}, []*router.Route{{
		Model:      "auto",
		Candidates: []router.Candidate{{Provider: fp, Model: "up", Groups: []string{"free"}}},
	}})
	db, err := usage.OpenDB(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ul := usage.NewLogger(db)
	keys, err := auth.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	prices := map[string]usage.Price{"up": {PromptPer1M: 1.0}}
	return NewWithOptions(Options{
		Router: rt, Usage: ul, Prices: prices, Keys: keys, GroupRatio: ratios,
	}), ul, keys
}

// End-to-end: key in group "free" hitting candidate group "free" pays 1.2×.
func TestGroupRatio_Applied(t *testing.T) {
	h, ul, keys := groupRatioSetup(t, map[string]float64{"free": 1.2})
	created, err := keys.Create(auth.Key{Name: "f", Enabled: true, Groups: []string{"free"}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"auto"}`))
	req.Header.Set("Authorization", "Bearer "+created.Key)
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	entries, err := ul.QueryRecent(1)
	if err != nil {
		t.Fatal(err)
	}
	want := (1000.0 / 1e6 * 1.0) * 1.2
	if len(entries) != 1 || entries[0].CostUSD == nil ||
		math.Abs(*entries[0].CostUSD-want) > 1e-12 {
		t.Fatalf("entries %+v, want cost %v", entries, want)
	}
}

// Unset group_ratio: same request pays the flat 1.0 multiplier.
func TestGroupRatio_UnsetUnchanged(t *testing.T) {
	h, ul, keys := groupRatioSetup(t, nil)
	created, err := keys.Create(auth.Key{Name: "f", Enabled: true, Groups: []string{"free"}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"auto"}`))
	req.Header.Set("Authorization", "Bearer "+created.Key)
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	entries, err := ul.QueryRecent(1)
	if err != nil {
		t.Fatal(err)
	}
	want := 1000.0 / 1e6 * 1.0
	if len(entries) != 1 || entries[0].CostUSD == nil ||
		math.Abs(*entries[0].CostUSD-want) > 1e-12 {
		t.Fatalf("entries %+v, want cost %v", entries, want)
	}
}
