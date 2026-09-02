// Package catalog periodically syncs model capabilities (context window,
// modalities) from models.dev, strictly additive below the hand-written
// price table: it only fills in models the config does not price.
package catalog

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// Entry is one synced model's capabilities.
type Entry struct {
	ContextTokens int      `json:"c,omitempty"`
	OutputTokens  int      `json:"o,omitempty"`
	Modalities    []string `json:"i,omitempty"` // non-text inputs: image, pdf, audio, video
}

// Syncer downloads the models.dev catalog daily and merges it into the
// price map. ETag-cached; failures are swallowed (stale file is fine).
type Syncer struct {
	url      string
	file     string // cache file next to the DB
	interval time.Duration
	client   *http.Client

	mu     sync.Mutex
	etag   string
	models map[string]Entry // normalized model id -> entry
	prices map[string]usage.Price
}

// models.dev api.json shape (trimmed to what we read).
type apiJSON map[string]struct {
	Models map[string]struct {
		Modalities struct {
			Input []string `json:"input"`
		} `json:"modalities"`
		Limit struct {
			Context int `json:"context"`
			Output  int `json:"output"`
		} `json:"limit"`
	} `json:"models"`
}

// NewSyncer builds a daily syncer. interval <= 0 = 24h.
func NewSyncer(file, url string, interval time.Duration, prices map[string]usage.Price) *Syncer {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	if url == "" {
		url = "https://models.dev/api.json"
	}
	return &Syncer{
		url: url, file: file, interval: interval, prices: prices,
		client: &http.Client{Timeout: 60 * time.Second},
		models: map[string]Entry{},
	}
}

// baseID normalizes "vendor/Model-X:free" -> "model-x".
func baseID(modelID string) string {
	if i := strings.LastIndex(modelID, "/"); i >= 0 {
		modelID = modelID[i+1:]
	}
	if i := strings.Index(modelID, ":"); i >= 0 {
		modelID = modelID[:i]
	}
	return strings.ToLower(modelID)
}

// fetch downloads the catalog; nil (no error) with empty raw = not modified.
func (s *Syncer) fetch(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	etag := s.etag
	s.mu.Unlock()
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &http.MaxBytesError{} // terse non-200 signal
	}
	var raw []byte
	buf := make([]byte, 64*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			raw = append(raw, buf[:n]...)
		}
		if rerr != nil {
			break
		}
	}
	if et := resp.Header.Get("ETag"); et != "" {
		s.mu.Lock()
		s.etag = et
		s.mu.Unlock()
	}
	return raw, nil
}

// parse converts the raw catalog into normalized entries.
func parse(raw []byte) map[string]Entry {
	var api apiJSON
	if err := json.Unmarshal(raw, &api); err != nil {
		return nil
	}
	out := map[string]Entry{}
	for _, provider := range api {
		for id, m := range provider.Models {
			e := Entry{ContextTokens: m.Limit.Context, OutputTokens: m.Limit.Output}
			for _, in := range m.Modalities.Input {
				if in != "text" {
					e.Modalities = append(e.Modalities, in)
				}
			}
			key := baseID(id)
			// First writer wins; models.dev lists the canonical vendor first.
			if _, ok := out[key]; !ok {
				out[key] = e
			}
		}
	}
	return out
}

// merge applies synced capabilities to the price map, strictly additive:
// only models with no configured price get a synthetic entry (context guard
// only, no cost). Returns the count of models added.
func (s *Syncer) merge(models map[string]Entry) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models = models
	added := 0
	for id, e := range models {
		if e.ContextTokens <= 0 {
			continue
		}
		if _, ok := s.prices[id]; ok {
			continue // hand-written table wins
		}
		s.prices[id] = usage.Price{ContextTokens: e.ContextTokens}
		added++
	}
	return added
}

// loadCache restores the last synced catalog from disk (startup fast path).
func (s *Syncer) loadCache() {
	raw, err := os.ReadFile(s.file)
	if err != nil {
		return
	}
	if models := parse(raw); len(models) > 0 {
		if n := s.merge(models); n > 0 {
			slog.Info("model catalog cache loaded", "models", len(models), "added", n)
		}
	}
}

// SyncOnce performs one fetch+merge cycle. Exported for tests/admin.
func (s *Syncer) SyncOnce(ctx context.Context) error {
	raw, err := s.fetch(ctx)
	if err != nil {
		return err
	}
	if raw == nil {
		return nil // not modified
	}
	models := parse(raw)
	if len(models) == 0 {
		return nil
	}
	// Atomic cache write: tmp + rename.
	tmp := s.file + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.file), 0o755); err == nil {
		if err := os.WriteFile(tmp, raw, 0o644); err == nil {
			_ = os.Rename(tmp, s.file)
		}
	}
	if n := s.merge(models); n > 0 {
		slog.Info("model catalog synced", "models", len(models), "added", n)
	}
	return nil
}

// Modalities returns a model's synced non-text input modalities (image, pdf,
// audio, video). ok=false when the catalog has no entry for the model, which
// callers must treat as "unknown", not "text-only". Model ids are normalized
// the same way as the sync (vendor prefix and :suffix stripped, lowercased),
// so upstream names like "openai/gpt-4o:free" resolve.
func (s *Syncer) Modalities(model string) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.models[baseID(model)]
	if !ok {
		return nil, false
	}
	return e.Modalities, true
}

// Run starts the daily sync loop until ctx cancels. First sync after a
// 60s startup delay so the server serves requests before fetching.
func (s *Syncer) Run(ctx context.Context) {
	s.loadCache()
	select {
	case <-ctx.Done():
		return
	case <-time.After(60 * time.Second):
	}
	for {
		if err := s.SyncOnce(ctx); err != nil {
			slog.Warn("model catalog sync", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.interval):
		}
	}
}
