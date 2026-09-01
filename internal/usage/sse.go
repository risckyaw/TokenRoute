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

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
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
