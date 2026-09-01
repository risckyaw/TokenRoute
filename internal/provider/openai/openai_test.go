package openai

import (
	"encoding/json"
	"testing"
)

func unmarshal(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRewriteModel_StreamMerge(t *testing.T) {
	out := unmarshal(t, rewriteModel([]byte(`{"model":"auto","stream":true}`), "deepseek-chat"))
	if out["model"] != "deepseek-chat" {
		t.Fatalf("model not rewritten: %v", out["model"])
	}
	so, ok := out["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Fatalf("stream_options not merged: %v", out["stream_options"])
	}
}

func TestRewriteModel_NonStreamUntouched(t *testing.T) {
	in := []byte(`{"model":"auto","messages":[]}`)
	out := unmarshal(t, rewriteModel(in, "gpt-4o"))
	if _, exists := out["stream_options"]; exists {
		t.Fatalf("stream_options added to non-stream: %v", out)
	}
	if out["model"] != "gpt-4o" {
		t.Fatalf("model not rewritten: %v", out["model"])
	}
}

func TestRewriteModel_StreamFalseUntouched(t *testing.T) {
	out := unmarshal(t, rewriteModel([]byte(`{"model":"auto","stream":false}`), "gpt-4o"))
	if _, exists := out["stream_options"]; exists {
		t.Fatalf("stream_options added when stream=false: %v", out)
	}
}

func TestRewriteModel_ClientStreamOptionsPreserved(t *testing.T) {
	in := []byte(`{"model":"auto","stream":true,"stream_options":{"max_tokens":42,"include_usage":false}}`)
	out := unmarshal(t, rewriteModel(in, "deepseek-chat"))
	so := out["stream_options"].(map[string]any)
	if so["max_tokens"] != float64(42) {
		t.Fatalf("client key clobbered: %v", so)
	}
	if so["include_usage"] != true {
		t.Fatalf("include_usage not forced true: %v", so)
	}
}

func TestRewriteModel_InvalidJSONPassthrough(t *testing.T) {
	in := []byte("not json")
	if got := rewriteModel(in, "x"); string(got) != string(in) {
		t.Fatalf("invalid JSON modified: %s", got)
	}
}
