package usage

import (
	"bytes"
	"encoding/json"
)

// SSEUsageTracker scans SSE "data:" lines for the terminal usage chunk.
// Forward the raw bytes to the client unchanged; feed a copy here.
type SSEUsageTracker struct {
	usage *Usage
}

// Usage is the upstream usage object, extended with token details for
// expression pricing (cache/image/audio breakdowns).
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// [OI] detail blocks.
	PromptTokensDetails     *PromptDetails `json:"prompt_tokens_details"`
	CompletionTokensDetails *CompDetails   `json:"completion_tokens_details"`
	// Anthropic flat fields (adapter translates to this shape).
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	// [OI] flat cache-creation (some providers).
	CacheCreationTokens int `json:"cache_creation_tokens"`
}

type PromptDetails struct {
	CachedTokens int `json:"cached_tokens"`
	AudioTokens  int `json:"audio_tokens"`
	ImageTokens  int `json:"image_tokens"`
}

type CompDetails struct {
	AudioTokens int `json:"audio_tokens"`
}

// CacheRead returns cached-read tokens (0 when none).
func (u *Usage) CacheRead() int {
	if u.CacheReadInputTokens > 0 {
		return u.CacheReadInputTokens
	}
	if u.PromptTokensDetails != nil {
		return u.PromptTokensDetails.CachedTokens
	}
	return 0
}

// CacheCreate returns cache-creation tokens (0 when none).
func (u *Usage) CacheCreate() int {
	if u.CacheCreationInputTokens > 0 {
		return u.CacheCreationInputTokens
	}
	return u.CacheCreationTokens
}

// ImageTokens returns image input tokens (0 when none).
func (u *Usage) ImageTokens() int {
	if u.PromptTokensDetails != nil {
		return u.PromptTokensDetails.ImageTokens
	}
	return 0
}

// AudioIn returns audio input tokens (0 when none).
func (u *Usage) AudioIn() int {
	if u.PromptTokensDetails != nil {
		return u.PromptTokensDetails.AudioTokens
	}
	return 0
}

// AudioOut returns audio output tokens (0 when none).
func (u *Usage) AudioOut() int {
	if u.CompletionTokensDetails != nil {
		return u.CompletionTokensDetails.AudioTokens
	}
	return 0
}

type chunk struct {
	Usage *Usage `json:"usage"`
}

// Feed consumes one complete SSE line (without trailing newline is fine).
// Garbage or partial lines are skipped silently.
func (t *SSEUsageTracker) Feed(line []byte) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	if bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	var c chunk
	if err := json.Unmarshal(payload, &c); err != nil {
		return
	}
	if c.Usage != nil {
		t.usage = c.Usage // keep the LAST non-nil usage
	}
}

// Usage returns the last usage seen, or nil.
func (t *SSEUsageTracker) Usage() *Usage { return t.usage }
