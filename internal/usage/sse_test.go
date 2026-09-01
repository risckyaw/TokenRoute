package usage

import "testing"

func TestSSEUsageTracker_MultiChunk(t *testing.T) {
	var tr SSEUsageTracker
	tr.Feed([]byte(`data: {"id":"1","choices":[{"delta":{"content":"hi"}}]}`))
	tr.Feed([]byte(""))
	tr.Feed([]byte(`data: {"id":"1","choices":[{"delta":{"content":" there"}}]}`))
	if tr.Usage() != nil {
		t.Fatalf("expected nil usage, got %+v", tr.Usage())
	}
	tr.Feed([]byte(`data: {"id":"1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	tr.Feed([]byte("data: [DONE]"))
	u := tr.Usage()
	if u == nil {
		t.Fatal("expected usage")
	}
	if u.PromptTokens != 10 || u.CompletionTokens != 5 || u.TotalTokens != 15 {
		t.Fatalf("bad usage: %+v", u)
	}
}

func TestSSEUsageTracker_LastWins(t *testing.T) {
	var tr SSEUsageTracker
	tr.Feed([]byte(`data: {"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	tr.Feed([]byte(`data: {"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
	if tr.Usage().TotalTokens != 7 {
		t.Fatalf("expected last usage, got %+v", tr.Usage())
	}
}

func TestSSEUsageTracker_Missing(t *testing.T) {
	var tr SSEUsageTracker
	tr.Feed([]byte(`data: {"choices":[{"delta":{"content":"x"}}]}`))
	tr.Feed([]byte("data: [DONE]"))
	if tr.Usage() != nil {
		t.Fatalf("expected nil, got %+v", tr.Usage())
	}
}

func TestSSEUsageTracker_Garbage(t *testing.T) {
	var tr SSEUsageTracker
	tr.Feed([]byte("not a data line"))
	tr.Feed([]byte("data: {invalid json"))
	tr.Feed([]byte("data: "))
	tr.Feed([]byte(":"))
	tr.Feed(nil)
	if tr.Usage() != nil {
		t.Fatalf("expected nil, got %+v", tr.Usage())
	}
}
