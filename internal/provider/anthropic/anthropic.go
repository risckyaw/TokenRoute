// Package anthropic implements the Anthropic Messages API provider,
// translating [OI] chat.completions format at the boundary so clients
// always speak [OI] to the gateway.
package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
)

const defaultBaseURL = "https://api.anthropic.com/v1"
const apiVersion = "2023-06-01"

type Config struct {
	Name      string
	BaseURL   string
	APIKey    string
	APIKeys   []string // pool; APIKey appended when set
	Priority  int
	TimeoutMs int
	// ResponseHeaderTimeoutMs bounds the wait for response headers only
	// (0 = disabled); streaming bodies are unaffected.
	ResponseHeaderTimeoutMs int
}

type Provider struct {
	name     string
	baseURL  string
	pool     *provider.KeyPool
	priority int
	client   *http.Client
}

func New(cfg Config) *Provider {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	keys := append([]string(nil), cfg.APIKeys...)
	if cfg.APIKey != "" {
		keys = append(keys, cfg.APIKey)
	}
	return &Provider{
		name:     cfg.Name,
		baseURL:  base,
		pool:     provider.NewKeyPool(keys...),
		priority: cfg.Priority,
		client:   provider.NewHTTPClient(cfg.TimeoutMs, cfg.ResponseHeaderTimeoutMs),
	}
}

func (p *Provider) Name() string      { return p.name }
func (p *Provider) Priority() int     { return p.priority }
func (p *Provider) ModelsURL() string { return p.baseURL + "/messages" }

// staticModels: Anthropic has no public /models on this API path.
var staticModels = []string{
	"claude-3-5-sonnet-latest",
	"claude-3-5-haiku-latest",
	"claude-3-opus-latest",
}

func (p *Provider) Models(context.Context) ([]string, error) {
	return staticModels, nil
}

// ---- request translation ([OI] -> Anthropic) ----

type oiMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type oiRequest struct {
	Model       string      `json:"model"`
	Messages    []oiMessage `json:"messages"`
	MaxTokens   *int        `json:"max_tokens"`
	Temperature *float64    `json:"temperature"`
	TopP        *float64    `json:"top_p"`
	Stream      bool        `json:"stream"`
}

// contentText flattens [OI] content: plain string or array of parts
// (text parts concatenated).
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, pt := range parts {
		if pt.Type == "text" || pt.Type == "" {
			sb.WriteString(pt.Text)
		}
	}
	return sb.String()
}

type antMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type antRequest struct {
	Model       string       `json:"model"`
	Messages    []antMessage `json:"messages"`
	System      string       `json:"system,omitempty"`
	MaxTokens   int          `json:"max_tokens"`
	Temperature *float64     `json:"temperature,omitempty"`
	TopP        *float64     `json:"top_p,omitempty"`
	Stream      bool         `json:"stream,omitempty"`
}

// translateRequest converts an [OI] chat.completions body to an Anthropic
// /messages body. System messages are joined with \n\n into top-level
// "system". max_tokens is required by Anthropic: request value else 1024.
func translateRequest(body []byte, model string) ([]byte, error) {
	var oi oiRequest
	if err := json.Unmarshal(body, &oi); err != nil {
		return nil, fmt.Errorf("parse request: %w", err)
	}
	out := antRequest{Model: model, Stream: oi.Stream, Temperature: oi.Temperature, TopP: oi.TopP}
	if oi.MaxTokens != nil {
		out.MaxTokens = *oi.MaxTokens
	} else {
		out.MaxTokens = 1024
	}
	var sys []string
	for _, m := range oi.Messages {
		txt := contentText(m.Content)
		if m.Role == "system" {
			sys = append(sys, txt)
			continue
		}
		role := m.Role
		if role != "user" && role != "assistant" {
			role = "user"
		}
		out.Messages = append(out.Messages, antMessage{Role: role, Content: txt})
	}
	out.System = strings.Join(sys, "\n\n")
	return json.Marshal(out)
}

// ---- response translation (Anthropic -> [OI]) ----

func mapFinishReason(stop string) string {
	switch stop {
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	}
	return stop
}

type antResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

func translateResponse(body []byte) ([]byte, error) {
	var ar antResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("parse upstream response: %w", err)
	}
	var sb strings.Builder
	for _, b := range ar.Content {
		if b.Type == "text" || b.Type == "" {
			sb.WriteString(b.Text)
		}
	}
	total := ar.Usage.InputTokens + ar.Usage.OutputTokens
	usage := map[string]any{
		"prompt_tokens":     ar.Usage.InputTokens,
		"completion_tokens": ar.Usage.OutputTokens,
		"total_tokens":      total,
	}
	if ar.Usage.CacheCreationInputTokens > 0 {
		usage["cache_creation_input_tokens"] = ar.Usage.CacheCreationInputTokens
	}
	if ar.Usage.CacheReadInputTokens > 0 {
		usage["cache_read_input_tokens"] = ar.Usage.CacheReadInputTokens
	}
	out := map[string]any{
		"id":      ar.ID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   ar.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": sb.String()},
			"finish_reason": mapFinishReason(ar.StopReason),
		}},
		"usage": usage,
	}
	return json.Marshal(out)
}

// ---- streaming ----

// streamTranslator converts Anthropic SSE events to [OI]-style chunks so the
// gateway SSEUsageTracker (top-level "usage" with prompt_tokens etc.) works.
type streamTranslator struct {
	r    *bufio.Reader
	body io.Closer
	out  bytes.Buffer

	id, model    string
	created      int64
	inputTokens  int
	outputTokens int
	cacheCreate  int
	cacheRead    int
	sentRole     bool
	finish       string
	done         bool

	eventType string
	dataLines [][]byte
}

func newStreamTranslator(body io.ReadCloser, model string) *streamTranslator {
	return &streamTranslator{
		r:       bufio.NewReader(body),
		body:    body,
		model:   model,
		created: time.Now().Unix(),
	}
}

func (t *streamTranslator) Close() error { return t.body.Close() }

func (t *streamTranslator) emitChunk(delta map[string]any, finish *string) {
	ch := map[string]any{
		"id": t.id, "object": "chat.completion.chunk", "created": t.created,
		"model": t.model,
		"choices": []any{map[string]any{
			"index": 0, "delta": delta, "finish_reason": finish,
		}},
	}
	b, _ := json.Marshal(ch)
	t.out.WriteString("data: ")
	t.out.Write(b)
	t.out.WriteString("\n\n")
}

func (t *streamTranslator) emitUsageAndDone() {
	usage := map[string]any{
		"prompt_tokens":     t.inputTokens,
		"completion_tokens": t.outputTokens,
		"total_tokens":      t.inputTokens + t.outputTokens,
	}
	if t.cacheCreate > 0 {
		usage["cache_creation_input_tokens"] = t.cacheCreate
	}
	if t.cacheRead > 0 {
		usage["cache_read_input_tokens"] = t.cacheRead
	}
	ch := map[string]any{
		"id": t.id, "object": "chat.completion.chunk", "created": t.created,
		"model": t.model, "choices": []any{},
		"usage": usage,
	}
	b, _ := json.Marshal(ch)
	t.out.WriteString("data: ")
	t.out.Write(b)
	t.out.WriteString("\n\ndata: [DONE]\n\n")
	t.done = true
}

func (t *streamTranslator) dispatch() {
	data := bytes.Join(t.dataLines, []byte("\n"))
	t.eventType, t.dataLines = "", nil
	if len(data) == 0 {
		return
	}
	var ev map[string]any
	if err := json.Unmarshal(data, &ev); err != nil {
		return
	}
	typ, _ := ev["type"].(string)
	switch typ {
	case "message_start":
		msg, _ := ev["message"].(map[string]any)
		if msg != nil {
			t.id, _ = msg["id"].(string)
			if m, _ := msg["model"].(string); m != "" {
				t.model = m
			}
			if u, _ := msg["usage"].(map[string]any); u != nil {
				t.inputTokens = num(u["input_tokens"])
				t.cacheCreate = num(u["cache_creation_input_tokens"])
				t.cacheRead = num(u["cache_read_input_tokens"])
			}
		}
		if !t.sentRole {
			t.sentRole = true
			t.emitChunk(map[string]any{"role": "assistant"}, nil)
		}
	case "content_block_delta":
		delta, _ := ev["delta"].(map[string]any)
		if delta == nil {
			return
		}
		if dt, _ := delta["type"].(string); dt != "text_delta" {
			return
		}
		text, _ := delta["text"].(string)
		if text == "" {
			return
		}
		t.emitChunk(map[string]any{"content": text}, nil)
	case "message_delta":
		if u, _ := ev["usage"].(map[string]any); u != nil {
			t.outputTokens = num(u["output_tokens"])
			if v := num(u["cache_creation_input_tokens"]); v > t.cacheCreate {
				t.cacheCreate = v
			}
			if v := num(u["cache_read_input_tokens"]); v > t.cacheRead {
				t.cacheRead = v
			}
		}
		if delta, _ := ev["delta"].(map[string]any); delta != nil {
			if sr, _ := delta["stop_reason"].(string); sr != "" {
				t.finish = mapFinishReason(sr)
				fr := t.finish
				t.emitChunk(map[string]any{}, &fr)
			}
		}
	case "message_stop":
		t.emitUsageAndDone()
	}
}

func num(v any) int {
	f, _ := v.(float64)
	return int(f)
}

// finishStream flushes a terminal usage chunk if upstream ended abruptly.
func (t *streamTranslator) finishStream() {
	if !t.done {
		t.emitUsageAndDone()
	}
}

func (t *streamTranslator) Read(p []byte) (int, error) {
	for t.out.Len() == 0 {
		if t.done {
			return 0, io.EOF
		}
		line, err := t.r.ReadBytes('\n')
		trimmed := bytes.TrimRight(line, "\r\n")
		switch {
		case bytes.HasPrefix(trimmed, []byte("event:")):
			t.eventType = string(bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("event:"))))
		case bytes.HasPrefix(trimmed, []byte("data:")):
			t.dataLines = append(t.dataLines, bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:"))))
		case len(trimmed) == 0:
			t.dispatch()
		}
		if err != nil {
			t.dispatch()
			t.finishStream()
			if t.out.Len() == 0 {
				return 0, io.EOF
			}
		}
	}
	return t.out.Read(p)
}

// ---- ChatComplete ----

// Embed is unsupported: Anthropic has no embeddings endpoint.
func (p *Provider) Embed(context.Context, *provider.Request) (*http.Response, error) {
	return provider.UnsupportedEmbed(), nil
}

func (p *Provider) ChatComplete(ctx context.Context, preq *provider.Request) (*http.Response, error) {
	key, ok := p.pool.Pick()
	if !ok {
		return nil, fmt.Errorf("all API keys in cooldown")
	}
	body, err := translateRequest(preq.Body, preq.Model)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", apiVersion)
	if key != "" {
		req.Header.Set("x-api-key", key)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream request: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests {
		p.pool.Cool(key)
	} else {
		p.pool.RecordUse(key)
	}
	if resp.StatusCode != http.StatusOK {
		return resp, nil // relay upstream errors unmodified (gateway handles failover)
	}
	if isStream(preq.Body) {
		resp.Body = newStreamTranslator(resp.Body, preq.Model)
		resp.Header.Del("Content-Length")
		return resp, nil
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read upstream response: %w", err)
	}
	out, err := translateResponse(raw)
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Type", "application/json")
	return resp, nil
}

func isStream(body []byte) bool {
	var m struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &m)
	return m.Stream
}
