package catalog

import (
	"sync"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// TestCatalogMergeConcurrentWithReaders runs the catalog syncer's merge loop
// against the shared price store while request-path readers hit it — the
// reviewer finding's catalog half. Deterministic: no network; a map
// race/panic fails the test even without -race.
func TestCatalogMergeConcurrentWithReaders(t *testing.T) {
	store := usage.NewPriceStore(map[string]usage.Price{
		"glm-9.9": {PromptPer1M: 1.0}, // hand-written wins
	})
	cat := NewSyncer(t.TempDir()+"/c.json", "", 0, store)
	models := parse([]byte(sample))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			cat.merge(models)
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_, _ = store.Get("gpt-9")
			_ = store.Has("glm-9.9")
			_ = store.Snapshot()
		}
	}()
	wg.Wait()

	if p, _ := store.Get("glm-9.9"); p.PromptPer1M != 1.0 {
		t.Fatalf("config price clobbered: %+v", p)
	}
	if p, ok := store.Get("gpt-9"); !ok || p.ContextTokens != 256000 {
		t.Fatalf("catalog entry missing: %+v ok=%v", p, ok)
	}
}
