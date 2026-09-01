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
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
)

type Config struct {
	Name      string
	BaseURL   string
	APIKey    string
	Priority  int
	TimeoutMs int
}

type Provider struct {
	name     string
	baseURL  string
	apiKey   string
	priority int
	client   *http.Client
}

func New(cfg Config) *Provider {
	timeout := time.Duration(cfg.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &Provider{
		name:     cfg.Name,
		baseURL:  strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:   cfg.APIKey,
		priority: cfg.Priority,
		client:   &http.Client{Timeout: timeout},
	}
}

func (p *Provider) Name() string      { return p.name }
func (p *Provider) Priority() int     { return p.priority }
func (p *Provider) ModelsURL() string { return p.baseURL + "/models" }

func (p *Provider) Models(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.ModelsURL(), nil)
	if err != nil {
		return nil, err
	}
	p.setAuth(req)
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
	body := rewriteModel(preq.Body, preq.Model)
	url := p.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	p.setAuth(req)
	for k, vs := range preq.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream request: %w", err)
	}
	return resp, nil
}

func (p *Provider) setAuth(req *http.Request) {
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
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
