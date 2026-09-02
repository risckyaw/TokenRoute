package server

import (
	"strings"
	"testing"
)

func TestEstimateWeighted_EnglishProse(t *testing.T) {
	text := "The quick brown fox jumps over the lazy dog repeatedly near rivers."
	words := len(strings.Fields(text))
	est := estimateWeighted(text, "openai")
	// Sanity: roughly words*1.3, ±50%.
	low, high := float64(words)*0.65, float64(words)*1.95
	if float64(est) < low || float64(est) > high {
		t.Fatalf("est %d outside [%v, %v] for %d words", est, low, high, words)
	}
}

func TestEstimateWeighted_CJKMuchHigherThanLenDiv4(t *testing.T) {
	text := strings.Repeat("語", 100) // 100 CJK chars
	est := estimateWeighted(text, "openai")
	// len/4 would be 25; CJK weights ~0.85/char -> ~85.
	if est < 60 {
		t.Fatalf("CJK est %d too low (len/4 = %d)", est, len(text)/4)
	}
	// Claude weights CJK higher (1.21).
	claude := estimateWeighted(text, "claude")
	if claude <= est {
		t.Fatalf("claude CJK est %d should exceed openai %d", claude, est)
	}
}

func TestEstimateWeighted_Empty(t *testing.T) {
	if got := estimateWeighted("", "openai"); got != 0 {
		t.Fatalf("empty = %d, want 0", got)
	}
}

func TestEstimateWeighted_UnknownFamilyFallsBack(t *testing.T) {
	text := "hello world"
	if estimateWeighted(text, "bogus") != estimateWeighted(text, "openai") {
		t.Fatal("unknown family must fall back to openai weights")
	}
}

func TestEstimateChatTokens_StringAndBlocks(t *testing.T) {
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"system","content":"You are helpful."},` +
		`{"role":"user","content":[{"type":"text","text":"Hello there"},{"type":"image_url","image_url":{"url":"data:..."}}]}` +
		`]}`)
	est := estimateChatTokens(body, "openai")
	// 2 messages overhead (8) + weighted text of both text parts.
	textEst := estimateWeighted("You are helpful.", "openai") + estimateWeighted("Hello there", "openai")
	if est != textEst+8 {
		t.Fatalf("est %d, want %d (text %d + overhead 8)", est, textEst+8, textEst)
	}
}

func TestEstimateChatTokens_NoMessagesFallback(t *testing.T) {
	body := []byte(`{"model":"m"}`)
	if got := estimateChatTokens(body, "openai"); got != len(body)/4 {
		t.Fatalf("fallback = %d, want %d", got, len(body)/4)
	}
}

func TestEstimateFamily(t *testing.T) {
	if estimateFamily("anthropic") != "claude" || estimateFamily("gemini") != "gemini" ||
		estimateFamily("openai") != "openai" || estimateFamily("") != "openai" {
		t.Fatal("bad family mapping")
	}
}
