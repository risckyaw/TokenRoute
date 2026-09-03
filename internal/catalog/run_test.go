package catalog

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

func TestRunDoesNotReloadCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	prices := usage.NewPriceStore(nil)
	s := NewSyncer(path, "", time.Hour, prices)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Run(ctx)
	if len(prices.Snapshot()) != 0 {
		t.Fatal("Run reloaded cache; caller must own the one-time LoadCache step")
	}
}
