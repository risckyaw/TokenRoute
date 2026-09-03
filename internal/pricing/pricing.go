// Package pricing syncs external model pricing (LiteLLM community catalog)
// into the shared price map. Resolution order, ported from OmniRoute
// pricingSync.ts: config price (user override) > synced external > unknown.
package pricing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// LiteLLMURL is the community pricing catalog (same source OmniRoute uses).
const LiteLLMURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// Syncer merges LiteLLM prices into a shared price store (same pattern as
// internal/catalog: strictly additive — hand-written config prices win).
type Syncer struct {
	mu     sync.Mutex
	prices *usage.PriceStore // shared with router/server; never reassigned
	models int
}

// NewSyncer binds the shared price store (the one passed to router/server).
func NewSyncer(prices *usage.PriceStore) *Syncer {
	return &Syncer{prices: prices}
}

// Count returns how many models the last fetch contained (observability).
func (s *Syncer) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.models
}

type litellmEntry struct {
	InputCostPerToken  float64 `json:"input_cost_per_token"`
	OutputCostPerToken float64 `json:"output_cost_per_token"`
	MaxInputTokens     flexInt `json:"max_input_tokens"`
	MaxTokens          flexInt `json:"max_tokens"`
	LiteLLMProvider    string  `json:"litellm_provider"`
	Mode               string  `json:"mode"`
}

// flexInt tolerates JSON numbers and numeric strings ("128000") — LiteLLM's
// catalog mixes both in max_*_tokens fields.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	b = bytes.Trim(b, `"`)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		*f = 0
		return nil
	}
	// Some rows are floats ("128000.0"); parse via float then truncate.
	v, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		*f = 0
		return nil
	}
	*f = flexInt(int(v))
	return nil
}

// Merge applies a parsed catalog to the shared map: only models with no
// configured price get a synced entry. Returns added count.
func (s *Syncer) Merge(catalog map[string]litellmEntry) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models = len(catalog)
	added := 0
	seen := map[string]bool{}
	put := func(name string, e litellmEntry) {
		if seen[name] {
			return
		}
		seen[name] = true
		if s.prices.Has(name) {
			return // config price wins (OmniRoute: user override > synced)
		}
		p := usage.Price{
			PromptPer1M:     e.InputCostPerToken * 1e6,
			CompletionPer1M: e.OutputCostPerToken * 1e6,
			ContextTokens:   int(e.MaxInputTokens),
		}
		if p.ContextTokens == 0 {
			p.ContextTokens = int(e.MaxTokens)
		}
		if e.Mode == "embedding" {
			p.EmbedPer1M = p.PromptPer1M
		}
		s.prices.Set(name, p)
		added++
	}
	for name, e := range catalog {
		if name == "sample_spec" {
			continue
		}
		put(name, e)
		// Bare alias for provider-prefixed names ("openai/gpt-4o" -> "gpt-4o").
		if i := strings.IndexByte(name, '/'); i > 0 {
			put(name[i+1:], e)
		}
	}
	return added
}

// FetchOnce downloads the catalog and merges it. Errors returned, never fatal.
func (s *Syncer) FetchOnce(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, LiteLLMURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("pricing fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pricing fetch: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("pricing read: %w", err)
	}
	// Decode per-entry: the catalog's sample_spec (and occasional junk rows)
	// carry non-numeric fields that would fail a whole-document unmarshal.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return fmt.Errorf("pricing parse: %w", err)
	}
	catalog := make(map[string]litellmEntry, len(raw))
	for name, blob := range raw {
		if name == "sample_spec" {
			continue
		}
		var e litellmEntry
		if err := json.Unmarshal(blob, &e); err != nil {
			continue
		}
		catalog[name] = e
	}
	s.Merge(catalog)
	return nil
}

// Run syncs immediately, then every interval, until ctx is cancelled.
// Failures are logged and retried next tick (map keeps last-good values).
func (s *Syncer) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	if err := s.FetchOnce(ctx); err != nil {
		slog.Warn("pricing sync", "err", err)
	} else {
		slog.Info("pricing sync", "models", s.Count())
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.FetchOnce(ctx); err != nil {
				slog.Warn("pricing sync", "err", err)
			} else {
				slog.Info("pricing sync", "models", s.Count())
			}
		}
	}
}
