package pricing

import (
	"sync"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

func TestMergeConfigWins(t *testing.T) {
	shared := usage.NewPriceStore(map[string]usage.Price{
		"gpt-4o": {PromptPer1M: 99, CompletionPer1M: 99}, // config override
	})
	s := NewSyncer(shared)
	catalog := map[string]litellmEntry{
		"gpt-4o":       {InputCostPerToken: 2.5e-6, OutputCostPerToken: 10e-6, MaxInputTokens: 128000},
		"openai/gpt-5": {InputCostPerToken: 1e-6, OutputCostPerToken: 4e-6, MaxInputTokens: 400000},
	}
	added := s.Merge(catalog)
	if added != 2 { // gpt-5 + bare alias; gpt-4o skipped (config wins)
		t.Fatalf("added = %d, want 2", added)
	}
	if p, _ := shared.Get("gpt-4o"); p.PromptPer1M != 99 {
		t.Fatalf("config price clobbered: %+v", p)
	}
	if p, ok := shared.Get("gpt-5"); !ok || p.PromptPer1M != 1.0 || p.ContextTokens != 400000 {
		t.Fatalf("gpt-5 merged wrong: %+v ok=%v", p, ok)
	}
	if !shared.Has("openai/gpt-5") {
		t.Fatal("prefixed name missing")
	}
}

func TestFlexIntToleratesStringsAndFloats(t *testing.T) {
	for in, want := range map[string]flexInt{
		`128000`:     128000,
		`"128000"`:   128000,
		`"128000.0"`: 128000,
		`null`:       0,
		`"unknown"`:  0,
	} {
		var f flexInt
		if err := f.UnmarshalJSON([]byte(in)); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if f != want {
			t.Fatalf("%s = %d, want %d", in, f, want)
		}
	}
}

func TestMergeEmbeddingMode(t *testing.T) {
	shared := usage.NewPriceStore(nil)
	s := NewSyncer(shared)
	s.Merge(map[string]litellmEntry{
		"text-embedding-3-small": {InputCostPerToken: 0.02e-6, Mode: "embedding"},
	})
	p, _ := shared.Get("text-embedding-3-small")
	if p.EmbedPer1M != 0.02 {
		t.Fatalf("embed price = %v, want 0.02", p.EmbedPer1M)
	}
}

// TestPricingMergeConcurrentWithReaders runs the pricing syncer's merge loop
// against the shared price store while request-path readers hit it — the
// reviewer finding's pricing half. Deterministic: no network; a map
// race/panic fails the test even without -race.
func TestPricingMergeConcurrentWithReaders(t *testing.T) {
	shared := usage.NewPriceStore(map[string]usage.Price{
		"gpt-4o": {PromptPer1M: 99}, // config wins
	})
	s := NewSyncer(shared)
	catalog := map[string]litellmEntry{
		"gpt-4o":       {InputCostPerToken: 2.5e-6, OutputCostPerToken: 10e-6, MaxInputTokens: 128000},
		"openai/gpt-5": {InputCostPerToken: 1e-6, OutputCostPerToken: 4e-6, MaxInputTokens: 400000},
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			s.Merge(catalog)
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_, _ = shared.Get("gpt-5")
			_ = shared.Has("gpt-4o")
			_ = shared.Snapshot()
		}
	}()
	wg.Wait()

	if p, _ := shared.Get("gpt-4o"); p.PromptPer1M != 99 {
		t.Fatalf("config price clobbered: %+v", p)
	}
	if p, ok := shared.Get("gpt-5"); !ok || p.PromptPer1M != 1.0 {
		t.Fatalf("synced price missing: %+v ok=%v", p, ok)
	}
}
