package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
)

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func tr(t *testing.T) func([]byte, error) []byte {
	return func(b []byte, err error) []byte {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
}

func TestTranslateRequest_SystemAndRoles(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "system", "content": "Be terse."},
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [{"type": "text", "text": "hello"}]}
		],
		"max_tokens": 256, "temperature": 0.7, "top_p": 0.8
	}`)
	out := decode(t, tr(t)(translateRequest(body, "gemini-2.0-flash")))
	si := out["systemInstruction"].(map[string]any)
	parts := si["parts"].([]any)
	if parts[0].(map[string]any)["text"] != "Be terse." {
		t.Fatalf("systemInstruction wrong: %v", si)
	}
	contents := out["contents"].([]any)
	if len(contents) != 2 {
		t.Fatalf("contents wrong: %v", contents)
	}
	if contents[1].(map[string]any)["role"] != "model" {
		t.Fatalf("assistant->model mapping wrong: %v", contents[1])
	}
	gc := out["generationConfig"].(map[string]any)
	if gc["maxOutputTokens"] != float64(256) || gc["temperature"] != 0.7 || gc["topP"] != 0.8 {
		t.Fatalf("generationConfig wrong: %v", gc)
	}
}

const gemNonStream = `{
	"candidates": [{
		"content": {"role": "model", "parts": [{"text": "Hello "}, {"text": "world"}]},
		"finishReason": "STOP"
	}],
	"usageMetadata": {"promptTokenCount": 8, "candidatesTokenCount": 4, "totalTokenCount": 12}
}`

func TestChatComplete_NonStream(t *testing.T) {
	var gotPath, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.URL.Query().Get("key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(gemNonStream))
	}))
	defer srv.Close()

	p := New(Config{Name: "gem", BaseURL: srv.URL, APIKey: "gkey", Priority: 1})
	resp, err := p.ChatComplete(context.Background(), &provider.Request{
		Model: "gemini-2.0-flash",
		Body:  []byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotPath != "/models/gemini-2.0-flash:generateContent" {
		t.Fatalf("path: %s", gotPath)
	}
	if gotKey != "gkey" {
		t.Fatalf("key: %s", gotKey)
	}
	out := decode(t, tr(t)(io.ReadAll(resp.Body)))
	if out["object"] != "chat.completion" {
		t.Fatalf("envelope wrong: %v", out)
	}
	ch := out["choices"].([]any)[0].(map[string]any)
	msg := ch["message"].(map[string]any)
	if msg["content"] != "Hello world" || msg["role"] != "assistant" {
		t.Fatalf("message wrong: %v", msg)
	}
	if ch["finish_reason"] != "stop" {
		t.Fatalf("finish wrong: %v", ch)
	}
	u := out["usage"].(map[string]any)
	if u["prompt_tokens"] != float64(8) || u["completion_tokens"] != float64(4) || u["total_tokens"] != float64(12) {
		t.Fatalf("usage wrong: %v", u)
	}
}

func TestTranslateResponse_MaxTokens(t *testing.T) {
	out := decode(t, tr(t)(translateResponse([]byte(
		`{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2,"totalTokenCount":3}}`), "m")))
	ch := out["choices"].([]any)[0].(map[string]any)
	if ch["finish_reason"] != "length" {
		t.Fatalf("MAX_TOKENS mapping wrong: %v", ch)
	}
}

const gemStream = `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}]}

data: {"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]}}]}

data: {"candidates":[{"content":{"role":"model","parts":[{"text":"!"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8}}

`

func TestChatComplete_Stream(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(gemStream))
	}))
	defer srv.Close()

	p := New(Config{Name: "gem", BaseURL: srv.URL, APIKey: "k"})
	resp, err := p.ChatComplete(context.Background(), &provider.Request{
		Model: "gemini-2.0-flash",
		Body:  []byte(`{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !strings.Contains(gotURL, ":streamGenerateContent") || !strings.Contains(gotURL, "alt=sse") {
		t.Fatalf("stream URL wrong: %s", gotURL)
	}
	raw := tr(t)(io.ReadAll(resp.Body))
	s := string(raw)

	for _, want := range []string{`"content":"Hel"`, `"content":"lo"`, `"content":"!"`, `"finish_reason":"stop"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s:\n%s", want, s)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(s), "data: [DONE]") {
		t.Fatalf("missing [DONE]:\n%s", s)
	}

	var usage map[string]any
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		var c map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &c); err != nil {
			t.Fatalf("bad chunk %q: %v", line, err)
		}
		if u, ok := c["usage"].(map[string]any); ok {
			usage = u
			if ch := c["choices"].([]any); len(ch) != 0 {
				t.Fatalf("usage chunk must have empty choices: %v", c)
			}
		}
	}
	if usage == nil {
		t.Fatalf("no usage chunk:\n%s", s)
	}
	if usage["prompt_tokens"] != float64(5) || usage["completion_tokens"] != float64(3) || usage["total_tokens"] != float64(8) {
		t.Fatalf("usage wrong: %v", usage)
	}
}

func TestModels_FiltersGemini(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "k" {
			t.Errorf("key missing: %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-2.0-flash"},{"name":"models/gemini-1.5-pro"},{"name":"models/embedding-001"}]}`))
	}))
	defer srv.Close()
	p := New(Config{Name: "g", BaseURL: srv.URL, APIKey: "k"})
	ms, err := p.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0] != "gemini-2.0-flash" || ms[1] != "gemini-1.5-pro" {
		t.Fatalf("models: %v", ms)
	}
}

func TestChatComplete_UpstreamErrorRelayed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"overloaded"}}`))
	}))
	defer srv.Close()
	p := New(Config{Name: "g", BaseURL: srv.URL})
	resp, err := p.ChatComplete(context.Background(), &provider.Request{Model: "m", Body: []byte(`{"messages":[]}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status not relayed: %d", resp.StatusCode)
	}
}
