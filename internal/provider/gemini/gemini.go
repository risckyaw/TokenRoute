// Package gemini implements the Google Gemini generateContent API provider,
// translating [OI] chat.completions format at the boundary so clients
// always speak [OI] to the gateway.
package gemini

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
)

const defaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

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
func (p *Provider) ModelsURL() string { return p.baseURL + "/models" }

func (p *Provider) Models(ctx context.Context) ([]string, error) {
	key, _ := p.pool.Pick()
	u := p.baseURL + "/models?key=" + url.QueryEscape(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(out.Models))
	for _, m := range out.Models {
		name := strings.TrimPrefix(m.Name, "models/")
		if strings.Contains(name, "gemini") {
			models = append(models, name)
		}
	}
	return models, nil
}

// ---- request translation ([OI] -> Gemini) ----

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

type gemPart struct {
	Text string `json:"text"`
}

type gemContent struct {
	Role  string    `json:"role,omitempty"`
	Parts []gemPart `json:"parts"`
}

type gemRequest struct {
	Contents          []gemContent  `json:"contents"`
	SystemInstruction *gemContent   `json:"systemInstruction,omitempty"`
	GenerationConfig  *gemGenConfig `json:"generationConfig,omitempty"`
}

type gemGenConfig struct {
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
}

// translateRequest converts an [OI] body to a Gemini generateContent body.
// System messages are joined into systemInstruction; assistant -> "model".
func translateRequest(body []byte, _ string) ([]byte, error) {
	var oi oiRequest
	if err := json.Unmarshal(body, &oi); err != nil {
		return nil, fmt.Errorf("parse request: %w", err)
	}
	out := gemRequest{}
	var sys []string
	for _, m := range oi.Messages {
		txt := contentText(m.Content)
		if m.Role == "system" {
			sys = append(sys, txt)
			continue
		}
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		out.Contents = append(out.Contents, gemContent{Role: role, Parts: []gemPart{{Text: txt}}})
	}
	if len(sys) > 0 {
		out.SystemInstruction = &gemContent{Parts: []gemPart{{Text: strings.Join(sys, "\n\n")}}}
	}
	if oi.MaxTokens != nil || oi.Temperature != nil || oi.TopP != nil {
		out.GenerationConfig = &gemGenConfig{
			MaxOutputTokens: oi.MaxTokens,
			Temperature:     oi.Temperature,
			TopP:            oi.TopP,
		}
	}
	return json.Marshal(out)
}

// ---- response translation (Gemini -> [OI]) ----

type gemUsage struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

type gemResponse struct {
	Candidates []struct {
		Content struct {
			Parts []gemPart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *gemUsage `json:"usageMetadata"`
}

func mapFinishReason(fr string) string {
	switch fr {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	}
	return strings.ToLower(fr)
}

func translateResponse(body []byte, model string) ([]byte, error) {
	var gr gemResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		return nil, fmt.Errorf("parse upstream response: %w", err)
	}
	var sb strings.Builder
	finish := "stop"
	if len(gr.Candidates) > 0 {
		for _, pt := range gr.Candidates[0].Content.Parts {
			sb.WriteString(pt.Text)
		}
		if fr := gr.Candidates[0].FinishReason; fr != "" {
			finish = mapFinishReason(fr)
		}
	}
	var usage map[string]any
	if u := gr.UsageMetadata; u != nil {
		usage = map[string]any{
			"prompt_tokens":     u.PromptTokenCount,
			"completion_tokens": u.CandidatesTokenCount,
			"total_tokens":      u.TotalTokenCount,
		}
	}
	out := map[string]any{
		"id":      "chatcmpl-gemini",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": sb.String()},
			"finish_reason": finish,
		}},
		"usage": usage,
	}
	return json.Marshal(out)
}

// ---- streaming ----

type streamTranslator struct {
	r    *bufio.Reader
	body io.Closer
	out  bytes.Buffer

	model     string
	created   int64
	usage     *gemUsage
	sentRole  bool
	finish    string
	done      bool
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
		"id": "chatcmpl-gemini", "object": "chat.completion.chunk", "created": t.created,
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
	var pt, ct, tt int
	if t.usage != nil {
		pt, ct, tt = t.usage.PromptTokenCount, t.usage.CandidatesTokenCount, t.usage.TotalTokenCount
	}
	ch := map[string]any{
		"id": "chatcmpl-gemini", "object": "chat.completion.chunk", "created": t.created,
		"model": t.model, "choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     pt,
			"completion_tokens": ct,
			"total_tokens":      tt,
		},
	}
	b, _ := json.Marshal(ch)
	t.out.WriteString("data: ")
	t.out.Write(b)
	t.out.WriteString("\n\ndata: [DONE]\n\n")
	t.done = true
}

func (t *streamTranslator) dispatch() {
	data := bytes.Join(t.dataLines, []byte("\n"))
	t.dataLines = nil
	if len(data) == 0 {
		return
	}
	var gr gemResponse
	if err := json.Unmarshal(data, &gr); err != nil {
		return
	}
	if gr.UsageMetadata != nil {
		t.usage = gr.UsageMetadata
	}
	for _, c := range gr.Candidates {
		if !t.sentRole {
			t.sentRole = true
			t.emitChunk(map[string]any{"role": "assistant"}, nil)
		}
		for _, pt := range c.Content.Parts {
			if pt.Text != "" {
				t.emitChunk(map[string]any{"content": pt.Text}, nil)
			}
		}
		if c.FinishReason != "" {
			t.finish = mapFinishReason(c.FinishReason)
			fr := t.finish
			t.emitChunk(map[string]any{}, &fr)
		}
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
		case bytes.HasPrefix(trimmed, []byte("data:")):
			t.dataLines = append(t.dataLines, bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:"))))
		case len(trimmed) == 0:
			t.dispatch()
		}
		if err != nil {
			t.dispatch()
			if !t.done {
				t.emitUsageAndDone()
			}
			if t.out.Len() == 0 {
				return 0, io.EOF
			}
		}
	}
	return t.out.Read(p)
}

// ---- ChatComplete ----

// Embed is unsupported here: the gateway exposes only the [OI] embeddings
// shape; Gemini's embedContent differs, so return 501 rather than translate.
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
	stream := isStream(preq.Body)
	action := ":generateContent?key="
	if stream {
		action = ":streamGenerateContent?alt=sse&key="
	}
	u := p.baseURL + "/models/" + url.PathEscape(preq.Model) + action + url.QueryEscape(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
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
	if stream {
		resp.Body = newStreamTranslator(resp.Body, preq.Model)
		resp.Header.Del("Content-Length")
		return resp, nil
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read upstream response: %w", err)
	}
	out, err := translateResponse(raw, preq.Model)
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
