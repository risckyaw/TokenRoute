// Package search implements POST /v1/search: web search via pluggable
// upstream search APIs (Tavily, Brave, Exa), normalised to one shape.
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
)

// Result is one normalised search hit.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

// Backend queries one upstream search API.
type Backend interface {
	Name() string
	Search(ctx context.Context, query string, maxResults int) ([]Result, error)
}

// Config wires the /v1/search handler.
type Config struct {
	Backends []Backend // tried in order until one succeeds
}

func client() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// ---- Tavily ----

type Tavily struct {
	Pool *provider.KeyPool
}

func (t *Tavily) Name() string { return "tavily" }

func (t *Tavily) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	key, ok := t.Pool.Pick()
	if !ok {
		return nil, fmt.Errorf("all tavily keys in cooldown")
	}
	payload, _ := json.Marshal(map[string]any{
		"query": query, "max_results": maxResults,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.tavily.com/search", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests {
		t.Pool.Cool(key)
		return nil, fmt.Errorf("tavily upstream status %d", resp.StatusCode)
	}
	t.Pool.RecordUse(key)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tavily upstream status %d", resp.StatusCode)
	}
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	res := make([]Result, 0, len(out.Results))
	for _, r := range out.Results {
		res = append(res, Result{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return res, nil
}

// ---- Brave ----

type Brave struct {
	Pool *provider.KeyPool
}

func (b *Brave) Name() string { return "brave" }

func (b *Brave) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	key, ok := b.Pool.Pick()
	if !ok {
		return nil, fmt.Errorf("all brave keys in cooldown")
	}
	url := "https://api.search.brave.com/res/v1/web/search?q=" + url.QueryEscape(query) +
		"&count=" + strconv.Itoa(maxResults)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Subscription-Token", key)
	req.Header.Set("Accept", "application/json")
	resp, err := client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests {
		b.Pool.Cool(key)
		return nil, fmt.Errorf("brave upstream status %d", resp.StatusCode)
	}
	b.Pool.RecordUse(key)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("brave upstream status %d", resp.StatusCode)
	}
	var out struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	res := make([]Result, 0, len(out.Web.Results))
	for _, r := range out.Web.Results {
		res = append(res, Result{Title: r.Title, URL: r.URL, Snippet: r.Description})
	}
	return res, nil
}

// ---- Exa ----

type Exa struct {
	Pool *provider.KeyPool
}

func (e *Exa) Name() string { return "exa" }

func (e *Exa) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	key, ok := e.Pool.Pick()
	if !ok {
		return nil, fmt.Errorf("all exa keys in cooldown")
	}
	payload, _ := json.Marshal(map[string]any{
		"query": query, "numResults": maxResults,
		"contents": map[string]any{"text": map[string]any{"maxCharacters": 500}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.exa.ai/search", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", key)
	resp, err := client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests {
		e.Pool.Cool(key)
		return nil, fmt.Errorf("exa upstream status %d", resp.StatusCode)
	}
	e.Pool.RecordUse(key)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exa upstream status %d", resp.StatusCode)
	}
	var out struct {
		Results []struct {
			Title string `json:"title"`
			URL   string `json:"url"`
			Text  string `json:"text"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	res := make([]Result, 0, len(out.Results))
	for _, r := range out.Results {
		res = append(res, Result{Title: r.Title, URL: r.URL, Snippet: r.Text})
	}
	return res, nil
}
