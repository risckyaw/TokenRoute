package anthropic

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

func TestTranslateRequest_SystemExtraction(t *testing.T) {
	body := []byte(`{
		"model": "auto",
		"messages": [
			{"role": "system", "content": "Be terse."},
			{"role": "system", "content": [{"type": "text", "text": "No emoji."}]},
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "hello"},
			{"role": "user", "content": [{"type": "text", "text": "how "}, {"type": "text", "text": "are you"}]}
		]
	}`)
	out := decode(t, tr(t)(translateRequest(body, "claude-x")))
	if out["system"] != "Be terse.\n\nNo emoji." {
		t.Fatalf("system join wrong: %v", out["system"])
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("system not stripped: %v", msgs)
	}
	last := msgs[2].(map[string]any)
	if last["role"] != "user" || last["content"] != "how are you" {
		t.Fatalf("array content not concatenated: %v", last)
	}
}

func TestTranslateRequest_MaxTokensDefault(t *testing.T) {
	out := decode(t, tr(t)(translateRequest([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "m")))
	if out["max_tokens"] != float64(1024) {
		t.Fatalf("max_tokens default wrong: %v", out["max_tokens"])
	}
	out = decode(t, tr(t)(translateRequest([]byte(`{"max_tokens":50,"messages":[]}`), "m")))
	if out["max_tokens"] != float64(50) {
		t.Fatalf("max_tokens not passed: %v", out["max_tokens"])
	}
}

func TestTranslateRequest_TemperatureTopP(t *testing.T) {
	out := decode(t, tr(t)(translateRequest(
		[]byte(`{"temperature":0.5,"top_p":0.9,"messages":[]}`), "m")))
	if out["temperature"] != 0.5 || out["top_p"] != 0.9 {
		t.Fatalf("params dropped: %v", out)
	}
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

const antNonStream = `{
	"id": "msg_123",
	"model": "claude-x",
	"content": [{"type": "text", "text": "Hello "}, {"type": "text", "text": "world"}],
	"stop_reason": "end_turn",
	"usage": {"input_tokens": 10, "output_tokens": 5}
}`

func TestChatComplete_NonStream(t *testing.T) {
	var gotAuth, gotVersion, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(antNonStream))
	}))
	defer srv.Close()

	p := New(Config{Name: "anth", BaseURL: srv.URL, APIKey: "sk-ant", Priority: 1})
	resp, err := p.ChatComplete(context.Background(), &provider.Request{
		Model: "claude-x",
		Body:  []byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotPath != "/messages" {
		t.Fatalf("path: %s", gotPath)
	}
	if gotAuth != "sk-ant" || gotVersion != apiVersion {
		t.Fatalf("headers: auth=%q version=%q", gotAuth, gotVersion)
	}
	out := decode(t, tr(t)(io.ReadAll(resp.Body)))
	if out["object"] != "chat.completion" || out["id"] != "msg_123" {
		t.Fatalf("envelope wrong: %v", out)
	}
	ch := out["choices"].([]any)[0].(map[string]any)
	msg := ch["message"].(map[string]any)
	if msg["role"] != "assistant" || msg["content"] != "Hello world" {
		t.Fatalf("message wrong: %v", msg)
	}
	if ch["finish_reason"] != "stop" {
		t.Fatalf("finish_reason wrong: %v", ch)
	}
	u := out["usage"].(map[string]any)
	if u["prompt_tokens"] != float64(10) || u["completion_tokens"] != float64(5) || u["total_tokens"] != float64(15) {
		t.Fatalf("usage wrong: %v", u)
	}
}

func TestTranslateResponse_FinishReasonMaxTokens(t *testing.T) {
	out := decode(t, tr(t)(translateResponse([]byte(
		`{"id":"m","model":"x","content":[{"type":"text","text":"a"}],"stop_reason":"max_tokens","usage":{"input_tokens":1,"output_tokens":2}}`))))
	ch := out["choices"].([]any)[0].(map[string]any)
	if ch["finish_reason"] != "length" {
		t.Fatalf("max_tokens mapping wrong: %v", ch)
	}
}

const antStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"claude-x","usage":{"input_tokens":12}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`

func TestChatComplete_Stream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["stream"] != true {
			t.Errorf("stream not forwarded: %v", req)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(antStream))
	}))
	defer srv.Close()

	p := New(Config{Name: "anth", BaseURL: srv.URL, APIKey: "k"})
	resp, err := p.ChatComplete(context.Background(), &provider.Request{
		Model: "claude-x",
		Body:  []byte(`{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw := tr(t)(io.ReadAll(resp.Body))
	s := string(raw)

	if !strings.Contains(s, `"role":"assistant"`) {
		t.Fatalf("missing role chunk:\n%s", s)
	}
	if !strings.Contains(s, `"content":"Hel"`) || !strings.Contains(s, `"content":"lo"`) {
		t.Fatalf("missing text chunks:\n%s", s)
	}
	if !strings.Contains(s, `"finish_reason":"stop"`) {
		t.Fatalf("missing finish chunk:\n%s", s)
	}
	if !strings.HasSuffix(strings.TrimSpace(s), "data: [DONE]") {
		t.Fatalf("missing [DONE]:\n%s", s)
	}

	// Verify the usage chunk parses into gateway shape.
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
	if usage["prompt_tokens"] != float64(12) || usage["completion_tokens"] != float64(7) || usage["total_tokens"] != float64(19) {
		t.Fatalf("usage wrong: %v", usage)
	}
}

func TestModels_Static(t *testing.T) {
	p := New(Config{Name: "a"})
	ms, err := p.Models(context.Background())
	if err != nil || len(ms) == 0 {
		t.Fatalf("models: %v %v", ms, err)
	}
}

func TestChatComplete_UpstreamErrorRelayed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
	}))
	defer srv.Close()
	p := New(Config{Name: "a", BaseURL: srv.URL})
	resp, err := p.ChatComplete(context.Background(), &provider.Request{Model: "m", Body: []byte(`{"messages":[]}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status not relayed: %d", resp.StatusCode)
	}
}
