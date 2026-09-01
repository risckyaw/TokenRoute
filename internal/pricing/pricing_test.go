package pricing

import (
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

func TestMergeConfigWins(t *testing.T) {
	shared := map[string]usage.Price{
		"gpt-4o": {PromptPer1M: 99, CompletionPer1M: 99}, // config override
	}
	s := NewSyncer(shared)
	catalog := map[string]litellmEntry{
		"gpt-4o":       {InputCostPerToken: 2.5e-6, OutputCostPerToken: 10e-6, MaxInputTokens: 128000},
		"openai/gpt-5": {InputCostPerToken: 1e-6, OutputCostPerToken: 4e-6, MaxInputTokens: 400000},
	}
	added := s.Merge(catalog)
	if added != 2 { // gpt-5 + bare alias; gpt-4o skipped (config wins)
		t.Fatalf("added = %d, want 2", added)
	}
	if shared["gpt-4o"].PromptPer1M != 99 {
		t.Fatalf("config price clobbered: %+v", shared["gpt-4o"])
	}
	if p, ok := shared["gpt-5"]; !ok || p.PromptPer1M != 1.0 || p.ContextTokens != 400000 {
		t.Fatalf("gpt-5 merged wrong: %+v ok=%v", p, ok)
	}
	if _, ok := shared["openai/gpt-5"]; !ok {
		t.Fatal("prefixed name missing")
	}
}

func TestMergeEmbeddingMode(t *testing.T) {
	shared := map[string]usage.Price{}
	s := NewSyncer(shared)
	s.Merge(map[string]litellmEntry{
		"text-embedding-3-small": {InputCostPerToken: 0.02e-6, Mode: "embedding"},
	})
	p := shared["text-embedding-3-small"]
	if p.EmbedPer1M != 0.02 {
		t.Fatalf("embed price = %v, want 0.02", p.EmbedPer1M)
	}
}
