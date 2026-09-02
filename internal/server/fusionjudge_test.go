package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jarvisagentic/tokenroute/internal/provider"
	"github.com/Jarvisagentic/tokenroute/internal/provider/openai"
	"github.com/Jarvisagentic/tokenroute/internal/router"
	"github.com/Jarvisagentic/tokenroute/internal/usage"
)

// judgeUpstream answers with an assistant message carrying `answer`, after
// `delay`, recording every request body it saw.
type judgeUpstream struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
}

func newJudgeUpstream(t *testing.T, answer string, status int, delay time.Duration) *judgeUpstream {
	t.Helper()
	u := &judgeUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.bodies = append(u.bodies, raw)
		u.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status != 200 {
			_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
			return
		}
		resp := map[string]any{
			"id": answer,
			"choices": []any{map[string]any{
				"message": map[string]any{"role": "assistant", "content": answer},
			}},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *judgeUpstream) requests() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][]byte(nil), u.bodies...)
}

// judgeSetup builds a fusion_judge route over the given upstreams; the judge is
// the LAST provider so panel-vs-judge traffic is distinguishable.
func judgeSetup(t *testing.T, cfg router.FusionJudgeConfig, ups ...*judgeUpstream) (http.Handler, *usage.Logger) {
	t.Helper()
	provs := make([]provider.Provider, 0, len(ups))
	cands := make([]router.Candidate, 0, len(ups))
	for i, u := range ups {
		name := string(rune('a' + i))
		p := openai.New(openai.Config{Name: name, BaseURL: u.srv.URL, Priority: i + 1, TimeoutMs: 10000})
		provs = append(provs, p)
		cands = append(cands, router.Candidate{Provider: p, Model: "m-" + name})
	}
	rt := router.New(provs, []*router.Route{{
		Model: "panel", Strategy: router.StrategyFusionJudge,
		FusionJudge: cfg, Candidates: cands,
	}})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	return NewWithOptions(Options{Router: rt, Usage: ul}), ul
}

func postPanel(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	return rec
}

const panelReq = `{"model":"panel","messages":[{"role":"user","content":"why is the sky blue"}]}`

// Two fast panel answers reach quorum; the slow third lands within the grace
// window; the judge (provider c) synthesizes from all of them.
func TestFusionJudge_QuorumAndGrace(t *testing.T) {
	a := newJudgeUpstream(t, "ANSWER-A", 200, 0)
	b := newJudgeUpstream(t, "ANSWER-B", 200, 0)
	slow := newJudgeUpstream(t, "ANSWER-C", 200, 120*time.Millisecond)
	h, ul := judgeSetup(t, router.FusionJudgeConfig{
		Judge: "a/m-a", MinPanel: 2, GraceMs: 1500, TimeoutMs: 10000,
	}, a, b, slow)

	rec := postPanel(t, h, panelReq)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	dec := rec.Header().Get("X-TokenRoute-Decision")
	if !strings.Contains(dec, ";fusion=judge") || !strings.Contains(dec, ";panel=3") || !strings.Contains(dec, ";answers=3") {
		t.Fatalf("decision %q, want ;fusion=judge;panel=3;answers=3", dec)
	}
	// The judge is provider a, which therefore saw two requests: its panel call
	// and the judge call. The judge call carries all three sources.
	var judgeBody []byte
	for _, raw := range a.requests() {
		if strings.Contains(string(raw), "Source 1") {
			judgeBody = raw
		}
	}
	if judgeBody == nil {
		t.Fatalf("judge never received a synthesis prompt; a saw %d requests", len(a.requests()))
	}
	for _, want := range []string{"Source 1", "Source 2", "Source 3", "ANSWER-A", "ANSWER-B", "ANSWER-C"} {
		if !strings.Contains(string(judgeBody), want) {
			t.Fatalf("judge body missing %q: %s", want, judgeBody)
		}
	}
	// Sources are anonymized: no provider or model names leak into the prompt.
	for _, leak := range []string{"m-a", "m-b", "m-c", "provider"} {
		var msgs struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(judgeBody, &msgs); err != nil {
			t.Fatal(err)
		}
		last := msgs.Messages[len(msgs.Messages)-1]
		if last.Role != "user" {
			t.Fatalf("judge turn role = %q, want user", last.Role)
		}
		if strings.Contains(last.Content, leak) {
			t.Fatalf("judge prompt leaks %q: %s", leak, last.Content)
		}
	}

	// Metering: one row per panel call (3) plus the judge call (4 total).
	entries, err := ul.QueryRecent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("logged %d usage rows, want 4 (3 panel + 1 judge)", len(entries))
	}
	for _, e := range entries {
		if e.Provider == "" || e.Model == "" || e.TotalTokens != 8 {
			t.Fatalf("bad usage row: %+v", e)
		}
	}
}

// Exactly one panel answer: no synthesis, the answer's model is re-issued with
// the original body and relayed directly.
func TestFusionJudge_SingleAnswerDirect(t *testing.T) {
	a := newJudgeUpstream(t, "ONLY-A", 200, 0)
	dead := newJudgeUpstream(t, "", 500, 0)
	h, ul := judgeSetup(t, router.FusionJudgeConfig{MinPanel: 2, GraceMs: 100, TimeoutMs: 5000}, a, dead)

	rec := postPanel(t, h, panelReq)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ONLY-A") {
		t.Fatalf("body %q, want the single answer relayed", rec.Body.String())
	}
	dec := rec.Header().Get("X-TokenRoute-Decision")
	if !strings.Contains(dec, ";answers=1") {
		t.Fatalf("decision %q, want ;answers=1", dec)
	}
	// The direct re-issue must not carry a synthesis prompt.
	for _, raw := range a.requests() {
		if strings.Contains(string(raw), "Source 1") {
			t.Fatalf("single-answer path sent a judge prompt: %s", raw)
		}
	}
	// 2 rows: the successful panel call plus the direct re-issue.
	entries, _ := ul.QueryRecent(10)
	if len(entries) != 2 {
		t.Fatalf("logged %d rows, want 2 (panel answer + direct re-issue)", len(entries))
	}
}

// Every panel model fails: the last upstream error relays as-is, like the
// sequential all-fail path.
func TestFusionJudge_NoAnswersRelaysFailure(t *testing.T) {
	a := newJudgeUpstream(t, "", 503, 0)
	b := newJudgeUpstream(t, "", 500, 0)
	h, _ := judgeSetup(t, router.FusionJudgeConfig{MinPanel: 2, TimeoutMs: 5000}, a, b)

	rec := postPanel(t, h, panelReq)
	if rec.Code != 503 && rec.Code != 500 {
		t.Fatalf("status %d, want a relayed upstream error (500/503)", rec.Code)
	}
	dec := rec.Header().Get("X-TokenRoute-Decision")
	if !strings.Contains(dec, ";answers=0") {
		t.Fatalf("decision %q, want ;answers=0", dec)
	}
}

// Transport failures across the whole panel produce the standard 502.
func TestFusionJudge_AllTransportErrors502(t *testing.T) {
	p1 := openai.New(openai.Config{Name: "a", BaseURL: deadURL(t), Priority: 1, TimeoutMs: 2000})
	p2 := openai.New(openai.Config{Name: "b", BaseURL: deadURL(t), Priority: 2, TimeoutMs: 2000})
	rt := router.New([]provider.Provider{p1, p2}, []*router.Route{{
		Model: "panel", Strategy: router.StrategyFusionJudge,
		Candidates: []router.Candidate{{Provider: p1, Model: "m-a"}, {Provider: p2, Model: "m-b"}},
	}})
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	h := NewWithOptions(Options{Router: rt, Usage: ul})

	rec := postPanel(t, h, panelReq)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "upstream_error") {
		t.Fatalf("body %q, want upstream_error", rec.Body.String())
	}
}

// Panel bodies are stripped: non-stream, no tools, tool history flattened.
func TestFusionJudge_PanelBodyStripped(t *testing.T) {
	a := newJudgeUpstream(t, "A", 200, 0)
	b := newJudgeUpstream(t, "B", 200, 0)
	h, _ := judgeSetup(t, router.FusionJudgeConfig{Judge: "a/m-a", MinPanel: 2, TimeoutMs: 5000}, a, b)

	req := `{"model":"panel","stream":true,"stream_options":{"include_usage":true},
		"tools":[{"type":"function","function":{"name":"get_weather"}}],
		"tool_choice":"auto",
		"messages":[
			{"role":"user","content":"weather?"},
			{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"c1","content":"18C and sunny"}
		]}`
	rec := postPanel(t, h, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	// b only ever gets a panel call: inspect it.
	reqs := b.requests()
	if len(reqs) != 1 {
		t.Fatalf("panel provider saw %d requests, want 1", len(reqs))
	}
	var got map[string]any
	if err := json.Unmarshal(reqs[0], &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"tools", "tool_choice", "stream_options"} {
		if _, ok := got[key]; ok {
			t.Fatalf("panel body still carries %q: %s", key, reqs[0])
		}
	}
	if got["stream"] != false {
		t.Fatalf("panel stream = %v, want false", got["stream"])
	}
	if got["model"] != "m-b" {
		t.Fatalf("panel model = %v, want m-b", got["model"])
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("want 3 flattened messages, got %d: %s", len(msgs), reqs[0])
	}
	flat := reqs[0]
	if !strings.Contains(string(flat), "[Called tools: get_weather]") {
		t.Fatalf("tool_calls turn not flattened: %s", flat)
	}
	if !strings.Contains(string(flat), "[Tool result: 18C and sunny]") {
		t.Fatalf("tool result not flattened: %s", flat)
	}
	for _, m := range msgs {
		if role := m.(map[string]any)["role"]; role == "tool" || role == "function" {
			t.Fatalf("tool role survived flattening: %s", flat)
		}
	}
}

// The judge call honors the client's stream flag.
func TestFusionJudge_JudgeStreamsWhenClientDoes(t *testing.T) {
	a := newJudgeUpstream(t, "A", 200, 0)
	b := newJudgeUpstream(t, "B", 200, 0)
	h, _ := judgeSetup(t, router.FusionJudgeConfig{Judge: "a/m-a", MinPanel: 2, TimeoutMs: 5000}, a, b)

	rec := postPanel(t, h, `{"model":"panel","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var judgeBody []byte
	for _, raw := range a.requests() {
		if strings.Contains(string(raw), "Source 1") {
			judgeBody = raw
		}
	}
	if judgeBody == nil {
		t.Fatal("no judge request seen")
	}
	var got map[string]any
	if err := json.Unmarshal(judgeBody, &got); err != nil {
		t.Fatal(err)
	}
	if got["stream"] != true {
		t.Fatalf("judge stream = %v, want true (client asked to stream)", got["stream"])
	}
}

// A circuit-open panel member is excluded from the fan-out entirely.
func TestFusionJudge_CircuitOpenMemberExcluded(t *testing.T) {
	a := newJudgeUpstream(t, "A", 200, 0)
	b := newJudgeUpstream(t, "B", 200, 0)
	c := newJudgeUpstream(t, "C", 200, 0)
	p1 := openai.New(openai.Config{Name: "a", BaseURL: a.srv.URL, Priority: 1, TimeoutMs: 5000})
	p2 := openai.New(openai.Config{Name: "b", BaseURL: b.srv.URL, Priority: 2, TimeoutMs: 5000})
	p3 := openai.New(openai.Config{Name: "c", BaseURL: c.srv.URL, Priority: 3, TimeoutMs: 5000})
	rt := router.New([]provider.Provider{p1, p2, p3}, []*router.Route{{
		Model: "panel", Strategy: router.StrategyFusionJudge,
		FusionJudge: router.FusionJudgeConfig{Judge: "a/m-a", MinPanel: 2, TimeoutMs: 5000},
		Candidates: []router.Candidate{
			{Provider: p1, Model: "m-a"}, {Provider: p2, Model: "m-b"}, {Provider: p3, Model: "m-c"},
		},
	}})
	rt.SetCircuit("c", router.CircuitConfig{FailureThreshold: 1, CooldownMs: 600000, AutoDisableAfter: 99})
	rt.RecordResult("c", time.Millisecond, false) // opens c
	ul, err := usage.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	h := NewWithOptions(Options{Router: rt, Usage: ul})

	rec := postPanel(t, h, panelReq)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if dec := rec.Header().Get("X-TokenRoute-Decision"); !strings.Contains(dec, ";panel=2") {
		t.Fatalf("decision %q, want ;panel=2 (c excluded)", dec)
	}
	if n := len(c.requests()); n != 0 {
		t.Fatalf("circuit-open member received %d requests, want 0", n)
	}
}

func TestFlattenToolHistory(t *testing.T) {
	in := []any{
		map[string]any{"role": "system", "content": "be terse"},
		map[string]any{"role": "user", "content": "weather?"},
		map[string]any{"role": "assistant", "content": "checking", "tool_calls": []any{
			map[string]any{"function": map[string]any{"name": "get_weather"}},
			map[string]any{"function": map[string]any{"name": "get_time"}},
		}},
		map[string]any{"role": "tool", "content": "18C"},
		map[string]any{"role": "function", "content": "12:00"},
		map[string]any{"role": "assistant", "content": "18C at noon"},
	}
	out := flattenToolHistory(in)
	if len(out) != len(in) {
		t.Fatalf("got %d messages, want %d", len(out), len(in))
	}
	want := []string{"be terse", "weather?", "[Called tools: get_weather, get_time]\nchecking",
		"[Tool result: 18C]", "[Tool result: 12:00]", "18C at noon"}
	for i, w := range want {
		m := out[i].(map[string]any)
		if got, _ := m["content"].(string); got != w {
			t.Fatalf("message %d content = %q, want %q", i, got, w)
		}
		if role := m["role"]; role == "tool" || role == "function" {
			t.Fatalf("message %d kept role %v", i, role)
		}
	}
	// A plain assistant turn with no tool calls passes through untouched.
	plain := []any{map[string]any{"role": "assistant", "content": "hi", "name": "bot"}}
	if got := flattenToolHistory(plain)[0].(map[string]any); got["name"] != "bot" {
		t.Fatalf("plain assistant turn was rewritten: %+v", got)
	}
}

func TestFusionJudgeResolveDefaults(t *testing.T) {
	minPanel, grace, timeout := FusionJudge{}.resolve(5)
	if minPanel != defaultMinPanel || grace != defaultGraceMs*time.Millisecond || timeout != defaultJudgeTimeout*time.Millisecond {
		t.Fatalf("defaults = %d/%v/%v", minPanel, grace, timeout)
	}
	// Quorum is clamped to the panel size, else it could never be reached.
	if got, _, _ := (FusionJudge{MinPanel: 9}).resolve(3); got != 3 {
		t.Fatalf("clamped quorum = %d, want 3", got)
	}
}

func TestContentTextFlattening(t *testing.T) {
	if got := contentText(json.RawMessage(`"plain"`)); got != "plain" {
		t.Fatalf("string content = %q", got)
	}
	blocks := json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)
	if got := contentText(blocks); got != "ab" {
		t.Fatalf("block content = %q, want ab", got)
	}
	if got := contentText(nil); got != "" {
		t.Fatalf("nil content = %q", got)
	}
}
