// Package openai implements an OpenAI-compatible provider
// (covers OpenAI, DeepSeek, OpenRouter, Ollama, etc.).
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
)

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
	keys := append([]string(nil), cfg.APIKeys...)
	if cfg.APIKey != "" {
		keys = append(keys, cfg.APIKey)
	}
	return &Provider{
		name:     cfg.Name,
		baseURL:  strings.TrimRight(cfg.BaseURL, "/"),
		pool:     provider.NewKeyPool(keys...),
		priority: cfg.Priority,
		client:   provider.NewHTTPClient(cfg.TimeoutMs, cfg.ResponseHeaderTimeoutMs),
	}
}

func (p *Provider) Name() string      { return p.name }
func (p *Provider) Priority() int     { return p.priority }
func (p *Provider) ModelsURL() string { return p.baseURL + "/models" }
func (p *Provider) CloseIdleConnections() {
	p.client.CloseIdleConnections()
}

func (p *Provider) Models(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.ModelsURL(), nil)
	if err != nil {
		return nil, err
	}
	key, _ := p.pool.Pick()
	p.setAuth(req, key)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		models = append(models, m.ID)
	}
	return models, nil
}

func (p *Provider) ChatComplete(ctx context.Context, preq *provider.Request) (*http.Response, error) {
	key, ok := p.pool.Pick()
	if !ok {
		return nil, fmt.Errorf("all API keys in cooldown")
	}
	body := rewriteModel(preq.Body, preq.Model)
	url := p.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	p.setAuth(req, key)
	for k, vs := range preq.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
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
	return resp, nil
}

// Embed posts to {base}/embeddings with the model field rewritten.
func (p *Provider) Embed(ctx context.Context, preq *provider.Request) (*http.Response, error) {
	key, ok := p.pool.Pick()
	if !ok {
		return nil, fmt.Errorf("all API keys in cooldown")
	}
	body := rewriteModel(preq.Body, preq.Model)
	url := p.baseURL + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	p.setAuth(req, key)
	for k, vs := range preq.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
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
	return resp, nil
}

func (p *Provider) setAuth(req *http.Request, key string) {
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

// rewriteModel replaces the top-level "model" field and, for streaming
// requests, merges stream_options.include_usage=true so the upstream emits
// a terminal usage chunk. A client-provided stream_options is merged, not
// clobbered. If the body is not valid JSON it is passed through unchanged.
func rewriteModel(body []byte, model string) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m["model"] = model
	if stream, _ := m["stream"].(bool); stream {
		so, _ := m["stream_options"].(map[string]any)
		if so == nil {
			so = map[string]any{}
		}
		so["include_usage"] = true
		m["stream_options"] = so
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
