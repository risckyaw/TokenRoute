package catalog

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

const sample = `{
  "openai": {"models": {
    "openai/GPT-9:free": {"modalities": {"input": ["text", "image"]}, "limit": {"context": 256000, "output": 8192}}
  }},
  "zai": {"models": {
    "zai-org/GLM-9.9": {"modalities": {"input": ["text"]}, "limit": {"context": 128000, "output": 4096}}
  }}
}`

func TestBaseID(t *testing.T) {
	cases := map[string]string{
		"openai/GPT-9:free": "gpt-9",
		"zai-org/GLM-9.9":   "glm-9.9",
		"plain":             "plain",
	}
	for in, want := range cases {
		if got := baseID(in); got != want {
			t.Errorf("baseID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseAndMerge(t *testing.T) {
	models := parse([]byte(sample))
	if len(models) != 2 {
		t.Fatalf("want 2 models, got %d", len(models))
	}
	if models["gpt-9"].ContextTokens != 256000 || len(models["gpt-9"].Modalities) != 1 {
		t.Fatalf("gpt-9 entry wrong: %+v", models["gpt-9"])
	}

	prices := map[string]usage.Price{
		"glm-9.9": {PromptPer1M: 1.0}, // hand-written wins
	}
	s := NewSyncer(filepath.Join(t.TempDir(), "cat.json"), "", 0, prices)
	added := s.merge(models)
	if added != 1 {
		t.Fatalf("want 1 added (gpt-9 only), got %d", added)
	}
	if prices["gpt-9"].ContextTokens != 256000 {
		t.Fatalf("gpt-9 context not merged: %+v", prices["gpt-9"])
	}
	if prices["glm-9.9"].PromptPer1M != 1.0 {
		t.Fatalf("hand-written price clobbered: %+v", prices["glm-9.9"])
	}
}

func TestSyncOnceETag(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		fmt.Fprint(w, sample)
	}))
	defer srv.Close()

	prices := map[string]usage.Price{}
	s := NewSyncer(filepath.Join(t.TempDir(), "cat.json"), srv.URL, 0, prices)
	ctx := context.Background()

	if err := s.SyncOnce(ctx); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if prices["gpt-9"].ContextTokens != 256000 {
		t.Fatalf("merge after sync failed: %+v", prices)
	}
	if err := s.SyncOnce(ctx); err != nil {
		t.Fatalf("second sync (304): %v", err)
	}
	if hits != 2 {
		t.Fatalf("want 2 upstream hits, got %d", hits)
	}
}

// Modalities exposes synced non-text input modalities for capability routing;
// unknown models must report ok=false, not an empty (text-only) list.
func TestModalitiesLookup(t *testing.T) {
	s := NewSyncer(filepath.Join(t.TempDir(), "cat.json"), "", 0, map[string]usage.Price{})
	s.merge(parse([]byte(sample)))

	mods, ok := s.Modalities("gpt-9")
	if !ok || len(mods) != 1 || mods[0] != "image" {
		t.Fatalf("gpt-9 modalities = %v ok=%v, want [image] true", mods, ok)
	}
	// Vendor prefix / :suffix normalization matches the sync.
	if mods, ok := s.Modalities("openai/GPT-9:free"); !ok || len(mods) != 1 {
		t.Fatalf("normalized lookup = %v ok=%v", mods, ok)
	}
	// Text-only model: known, with no non-text modalities.
	if mods, ok := s.Modalities("glm-9.9"); !ok || len(mods) != 0 {
		t.Fatalf("glm-9.9 modalities = %v ok=%v, want [] true", mods, ok)
	}
	if _, ok := s.Modalities("never-synced"); ok {
		t.Fatal("unknown model must report ok=false")
	}
}
