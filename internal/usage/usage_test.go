package usage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestCost(t *testing.T) {
	if Cost(100, 100, nil) != nil {
		t.Fatal("nil price must yield nil cost")
	}
	p := &Price{PromptPer1M: 0.14, CompletionPer1M: 0.28}
	c := Cost(1_000_000, 500_000, p)
	if c == nil || *c < 0.27 || *c > 0.29 {
		t.Fatalf("bad cost: %v", c)
	}
}

func TestLoggerRoundTrip(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "sub", "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	c := 0.42
	e := Entry{
		RequestID: "abc", TS: time.Now(), VirtualModel: "auto", Provider: "deepseek",
		Model: "deepseek-chat", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
		Stream: true, Status: 200, LatencyMs: 123, CostUSD: &c,
	}
	if err := l.Log(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if err := l.Log(context.Background(), Entry{RequestID: "no-cost", Status: 502}); err != nil {
		t.Fatal(err)
	}

	got, err := l.QueryRecent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}
	// newest first
	if got[0].RequestID != "no-cost" || got[0].CostUSD != nil {
		t.Fatalf("bad first entry: %+v", got[0])
	}
	if got[1].RequestID != "abc" || !got[1].Stream || got[1].CostUSD == nil || *got[1].CostUSD != 0.42 {
		t.Fatalf("bad second entry: %+v", got[1])
	}
}
