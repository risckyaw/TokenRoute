package server

import (
	"encoding/json"
	"unicode"
)

// Weighted token estimator (port of new-api service/token_estimator.go):
// single pass over runes, chars bucketed by class, each class scaled by a
// per-provider-family weight (tokens per class unit). Words and numbers are
// counted as runs; CJK per char. Deviations from new-api: segmentation
// classes are mapped to the nearest unicode category (math symbols = Sm,
// emoji = So outside the math/dingbat ranges, URL delimiters fixed set).
var familyWeights = map[string][10]float64{
	// word, number, cjk, symbol, math, urldelim, atsign, emoji, newline, space
	"openai": {1.02, 1.55, 0.85, 0.4, 2.68, 1.0, 2.0, 2.12, 0.5, 0.42},
	"gemini": {1.15, 2.8, 0.68, 0.38, 1.05, 1.2, 2.5, 1.08, 1.15, 0.2},
	"claude": {1.13, 1.63, 1.21, 0.4, 4.52, 1.26, 2.82, 2.6, 0.89, 0.39},
}

const (
	wWord = iota
	wNumber
	wCJK
	wSymbol
	wMath
	wURLDelim
	wAtSign
	wEmoji
	wNewline
	wSpace
)

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}

func isURLLikeDelim(r rune) bool {
	switch r {
	case ':', '/', '?', '&', '=', '#', '.', '-', '_', '~':
		return true
	}
	return false
}

// estimateWeighted returns the weighted token estimate for text under a
// provider family ("openai"|"claude"|"gemini"; unknown -> openai).
func estimateWeighted(text, family string) int {
	w, ok := familyWeights[family]
	if !ok {
		w = familyWeights["openai"]
	}
	var counts [10]float64
	inWord, inNum := false, false
	wordLen := 0
	for _, r := range text {
		switch {
		case r == '\n':
			counts[wNewline]++
			inWord, inNum = false, false
		case r == ' ' || r == '\t' || r == '\r':
			counts[wSpace]++
			inWord, inNum = false, false
		case r == '@':
			counts[wAtSign]++
			inWord, inNum = false, false
		case isCJK(r):
			counts[wCJK]++
			inWord, inNum = false, false
		case unicode.IsDigit(r):
			if !inNum {
				counts[wNumber]++ // number run
			}
			inNum, inWord = true, false
		case unicode.IsLetter(r):
			if !inWord {
				counts[wWord]++ // word run
				wordLen = 0
			}
			wordLen++
			// ponytail: pathological single-letter runs (e.g. "aaaa...") are
			// one word per new-api segmentation, which undercounts badly;
			// charge letters past 8 at the symbol weight instead.
			if wordLen > 8 {
				counts[wSymbol]++
			}
			inWord, inNum = true, false
		case unicode.Is(unicode.Sm, r):
			counts[wMath]++
			inWord, inNum = false, false
		case unicode.Is(unicode.So, r) || r > 0xFFFF && !unicode.IsPrint(r):
			counts[wEmoji]++
			inWord, inNum = false, false
		case isURLLikeDelim(r):
			counts[wURLDelim]++
			inWord, inNum = false, false
		default:
			counts[wSymbol]++
			inWord, inNum = false, false
		}
	}
	total := 0.0
	for i, c := range counts {
		total += c * w[i]
	}
	return int(total + 0.5)
}

// perMessageOverhead approximates the per-message role/format tokens
// (new-api uses a small constant per message).
const perMessageOverhead = 4

// estimateChatTokens approximates prompt tokens: weighted char-class
// estimate over message text content + per-message overhead, for the
// provider family of the first candidate. Falls back to len/4 when the body
// has no messages.
func estimateChatTokens(body []byte, family string) int {
	var parsed struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Messages) == 0 {
		return len(body) / 4
	}
	total := 0
	for _, m := range parsed.Messages {
		total += perMessageOverhead
		// String content.
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			total += estimateWeighted(s, family)
			continue
		}
		// Block content: sum text blocks only (image/audio parts bill by
		// their own schedules upstream; text is what we can estimate).
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(m.Content, &blocks); err == nil {
			for _, b := range blocks {
				if b.Type == "text" || b.Type == "" && b.Text != "" {
					total += estimateWeighted(b.Text, family)
				}
			}
			continue
		}
		// Unknown shape: raw JSON fallback.
		total += len(m.Content) / 4
	}
	return total
}

// estimateEmbedTokens approximates prompt tokens as len(input)/4 (chars).
func estimateEmbedTokens(body []byte) int {
	var parsed struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Input) == 0 {
		return len(body) / 4
	}
	return len(parsed.Input) / 4
}
