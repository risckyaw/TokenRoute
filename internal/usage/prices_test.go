package usage

import (
	"sync"
	"testing"
)

// TestPriceStoreConcurrentReadWrite hammers Get/Set/Has/Snapshot from many
// goroutines. Deterministic: no network, no sleeps; failure mode is a Go
// map race/panic (fatal even without -race) or wrong final state.
func TestPriceStoreConcurrentReadWrite(t *testing.T) {
	ps := NewPriceStore(map[string]Price{"seed": {PromptPer1M: 1}})
	const writers = 8
	const readers = 8
	const iters = 500

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			model := string(rune('a' + id))
			for i := 0; i < iters; i++ {
				ps.Set(model, Price{PromptPer1M: float64(i)})
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				_, _ = ps.Get("seed")
				_ = ps.Has("seed")
				_ = ps.Snapshot()
			}
		}()
	}
	wg.Wait()

	if p, ok := ps.Get("seed"); !ok || p.PromptPer1M != 1 {
		t.Fatalf("seed clobbered: %+v ok=%v", p, ok)
	}
	snap := ps.Snapshot()
	if len(snap) != writers+1 {
		t.Fatalf("snapshot size = %d, want %d", len(snap), writers+1)
	}
}
