package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/config"
	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// Phase 6a: rolling history cache breakpoint (REQ-6) and the structured
// router call (REQ-7) of the Anthropic adapter, against an httptest fake.

func p6aCacheCount(v any) int {
	return strings.Count(mustJSON(v), `"cache_control"`)
}

// p6aBlocks returns message i's content blocks.
func p6aBlocks(t *testing.T, body map[string]any, i int) []map[string]any {
	t.Helper()
	msgs := body["messages"].([]any)
	content := msgs[i].(map[string]any)["content"].([]any)
	out := make([]map[string]any, 0, len(content))
	for _, c := range content {
		out = append(out, c.(map[string]any))
	}
	return out
}

func p6aHasCache(b map[string]any) bool {
	cc, ok := b["cache_control"].(map[string]any)
	return ok && cc["type"] == "ephemeral"
}

// p6aOnlyCachedBlock checks that exactly one message block carries a cache
// breakpoint and that it is block j of message i.
func p6aOnlyCachedBlock(t *testing.T, body map[string]any, i, j int) {
	t.Helper()
	msgs := body["messages"].([]any)
	for mi := range msgs {
		for bj, b := range p6aBlocks(t, body, mi) {
			want := mi == i && bj == j
			if p6aHasCache(b) != want {
				t.Fatalf("message %d block %d cache=%v, want %v; messages=%s", mi, bj, p6aHasCache(b), want, mustJSON(msgs))
			}
		}
	}
}

// TC-6A20 (REQ-6): with CacheHistory and an owner text as newest turn, the
// text block of the newest message is the one extra breakpoint; system and
// last tool keep theirs; content and order are unchanged; ≤ 4 in total.
func TestPhase6aAnthropicTools_HistoryBreakpointOnNewestText(t *testing.T) {
	cap := &captured{}
	a := toolServer(t, 200, endTurnResp, cap)
	req := sampleRequest()
	req.CacheHistory = true
	req.Turns = append(req.Turns, domain.Turn{Role: domain.AIRole, Text: "nothing found"}, domain.Turn{Role: domain.UserRole, Text: "câu hỏi mới 🙂"})
	if _, err := a.GenerateWithTools(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	b := cap.body
	msgs := b["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("messages=%d", len(msgs))
	}
	p6aOnlyCachedBlock(t, b, 4, 0)
	last := p6aBlocks(t, b, 4)[0]
	if last["type"] != "text" || last["text"] != "câu hỏi mới 🙂" || msgs[4].(map[string]any)["role"] != "user" {
		t.Fatalf("newest message changed: %v", msgs[4])
	}
	if first := p6aBlocks(t, b, 0)[0]; first["text"] != "investigate" {
		t.Fatalf("order changed: %v", first)
	}
	sys := b["system"].([]any)[0].(map[string]any)
	tools := b["tools"].([]any)
	if !p6aHasCache(sys) || !p6aHasCache(tools[len(tools)-1].(map[string]any)) {
		t.Fatalf("static breakpoints lost: system=%v tools=%v", sys, tools)
	}
	if n := p6aCacheCount(b); n != 3 || n > 4 {
		t.Fatalf("cache breakpoints=%d, want 3 (system, last tool, newest turn)", n)
	}
}

// TC-6A21 (REQ-6): when the newest turn holds tool results, only its last
// tool_result block carries the breakpoint.
func TestPhase6aAnthropicTools_HistoryBreakpointOnLastToolResult(t *testing.T) {
	cap := &captured{}
	a := toolServer(t, 200, endTurnResp, cap)
	req := sampleRequest()
	req.CacheHistory = true
	req.Turns[1].Calls = append(req.Turns[1].Calls, domain.ToolCall{ID: "tu_9", Name: "code_search", Input: json.RawMessage(`{"repo":"o/r"}`)})
	req.Turns[2].Results = append(req.Turns[2].Results, domain.ToolResult{CallID: "tu_9", Content: "ok"})
	if _, err := a.GenerateWithTools(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	p6aOnlyCachedBlock(t, cap.body, 2, 1)
	blocks := p6aBlocks(t, cap.body, 2)
	if blocks[0]["tool_use_id"] != "tu_0" || blocks[1]["tool_use_id"] != "tu_9" || blocks[1]["type"] != "tool_result" {
		t.Fatalf("results changed: %v", blocks)
	}
	if n := p6aCacheCount(cap.body); n != 3 {
		t.Fatalf("cache breakpoints=%d, want 3", n)
	}
}

// TC-6A22 (REQ-6): a newest turn with results and text caches only the
// final (text) block; without CacheHistory no message block is cached and
// without CacheStatic only the history breakpoint is sent.
func TestPhase6aAnthropicTools_HistoryBreakpointVariants(t *testing.T) {
	cap := &captured{}
	a := toolServer(t, 200, endTurnResp, cap)
	req := sampleRequest()
	req.CacheHistory = true
	req.Turns[2].Text = "and also this"
	if _, err := a.GenerateWithTools(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	p6aOnlyCachedBlock(t, cap.body, 2, 1)
	if blocks := p6aBlocks(t, cap.body, 2); blocks[1]["type"] != "text" || blocks[0]["type"] != "tool_result" {
		t.Fatalf("blocks=%v", blocks)
	}

	cap = &captured{}
	a = toolServer(t, 200, endTurnResp, cap)
	if _, err := a.GenerateWithTools(context.Background(), sampleRequest()); err != nil {
		t.Fatal(err)
	}
	p6aOnlyCachedBlock(t, cap.body, -1, -1)
	if n := p6aCacheCount(cap.body); n != 2 {
		t.Fatalf("CacheStatic only: breakpoints=%d, want 2", n)
	}

	cap = &captured{}
	a = toolServer(t, 200, endTurnResp, cap)
	req = sampleRequest()
	req.CacheStatic, req.CacheHistory = false, true
	if _, err := a.GenerateWithTools(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if n := p6aCacheCount(cap.body); n != 1 {
		t.Fatalf("CacheHistory only: breakpoints=%d, want 1", n)
	}
	p6aOnlyCachedBlock(t, cap.body, 2, 0)

	// The assistant's last step offers no tools: system + history only.
	cap = &captured{}
	a = toolServer(t, 200, endTurnResp, cap)
	req = sampleRequest()
	req.CacheHistory, req.Tools = true, nil
	if _, err := a.GenerateWithTools(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if n := p6aCacheCount(cap.body); n != 2 {
		t.Fatalf("no tools: breakpoints=%d, want 2", n)
	}
}

func p6aPlainRequest(cache bool) *domain.LLMRequest {
	return &domain.LLMRequest{Model: "claude-sonnet-5-5", System: "sys", MaxTokens: 100, CacheHistory: cache, Messages: []domain.Message{
		{Role: domain.UserRole, Text: "một"}, {Role: domain.AIRole, Text: "hai"}, {Role: domain.UserRole, Text: "ba"},
	}}
}

// TC-6A23 (REQ-6): plain Generate caches its last message only when asked;
// content and order are unchanged.
func TestPhase6aAnthropicGenerate_HistoryBreakpoint(t *testing.T) {
	cap := &captured{}
	a := toolServer(t, 200, endTurnResp, cap)
	resp, err := a.Generate(context.Background(), p6aPlainRequest(true))
	if err != nil || resp.Text != "final answer" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	p6aOnlyCachedBlock(t, cap.body, 2, 0)
	for i, want := range []string{"một", "hai", "ba"} {
		if got := p6aBlocks(t, cap.body, i)[0]["text"]; got != want {
			t.Fatalf("message %d=%v, want %q", i, got, want)
		}
	}
	if n := p6aCacheCount(cap.body); n != 1 {
		t.Fatalf("breakpoints=%d, want 1", n)
	}

	cap = &captured{}
	a = toolServer(t, 200, endTurnResp, cap)
	if _, err := a.Generate(context.Background(), p6aPlainRequest(false)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(mustJSON(cap.body), "cache_control") {
		t.Fatal("cache_control sent without CacheHistory")
	}
}

const p6aRouterSchema = `{"type":"object","properties":{"route":{"type":"string","enum":["answer","escalate"]}},"required":["route"],"additionalProperties":false}`

func p6aJSONRequest() *domain.JSONRequest {
	return &domain.JSONRequest{Model: "claude-haiku-5-5", System: "router", MaxTokens: 600, CacheHistory: true,
		Schema: json.RawMessage(p6aRouterSchema), Messages: []domain.Message{{Role: domain.UserRole, Text: "cảm ơn nhé"}}}
}

// TC-6A24 (REQ-6, REQ-7): GenerateJSON sends output_config.format as a
// json_schema with the given schema, no tools, the history breakpoint, and
// returns the raw JSON, end_turn and the usage.
func TestPhase6aAnthropicGenerateJSON_RequestAndResponse(t *testing.T) {
	cap := &captured{}
	resp := strings.Replace(endTurnResp, `"final answer"`, `"{\"route\":\"escalate\",\"tier\":\"work\"}"`, 1)
	a := toolServer(t, 200, resp, cap)
	out, err := a.GenerateJSON(context.Background(), p6aJSONRequest())
	if err != nil {
		t.Fatal(err)
	}
	if string(out.JSON) != `{"route":"escalate","tier":"work"}` || out.Stop != domain.StopEndTurn || out.Usage != (domain.Usage{Input: 120, Output: 30, CacheRead: 80, CacheWrite: 40}) {
		t.Fatalf("out=%+v json=%s", out, out.JSON)
	}
	b := cap.body
	if b["model"] != "claude-haiku-5-5" || b["max_tokens"].(float64) != 600 {
		t.Fatalf("model/max_tokens %v %v", b["model"], b["max_tokens"])
	}
	if _, ok := b["tools"]; ok {
		t.Fatalf("tools sent on a JSON call: %v", b["tools"])
	}
	format, _ := b["output_config"].(map[string]any)["format"].(map[string]any)
	if format == nil || format["type"] != "json_schema" {
		t.Fatalf("output_config=%v", b["output_config"])
	}
	var want map[string]any
	_ = json.Unmarshal([]byte(p6aRouterSchema), &want)
	if mustJSON(format["schema"]) != mustJSON(want) {
		t.Fatalf("schema=%v", format["schema"])
	}
	if sys := b["system"].([]any)[0].(map[string]any); sys["text"] != "router" {
		t.Fatalf("system=%v", sys)
	}
	p6aOnlyCachedBlock(t, b, 0, 0)
}

// TC-6A25 (REQ-7): refusal and max_tokens come back as Stop reasons with
// usage, not as errors.
func TestPhase6aAnthropicGenerateJSON_StopReasons(t *testing.T) {
	for stop, want := range map[string]domain.StopReason{"refusal": domain.StopRefusal, "max_tokens": domain.StopMaxTokens} {
		a := toolServer(t, 200, strings.Replace(endTurnResp, `"end_turn"`, `"`+stop+`"`, 1), nil)
		out, err := a.GenerateJSON(context.Background(), p6aJSONRequest())
		if err != nil || out.Stop != want || out.Usage.Input != 120 || out.Usage.Output != 30 {
			t.Fatalf("%s: out=%+v err=%v", stop, out, err)
		}
	}
}

// TC-6A26 (REQ-7): a nil request, a bad schema or no messages fail before
// any HTTP call; an HTTP error does not echo the body.
func TestPhase6aAnthropicGenerateJSON_Errors(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"SECRET-PROMPT-TEXT"}}`)
	}))
	t.Cleanup(srv.Close)
	a, err := NewAnthropic(config.AnthropicConfig{APIKey: "test-key", BaseURL: srv.URL, MaxRetries: 1}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.GenerateJSON(context.Background(), nil); err == nil {
		t.Error("nil request accepted")
	}
	for _, schema := range []string{``, `not json`, `null`, `[1]`} {
		req := p6aJSONRequest()
		req.Schema = json.RawMessage(schema)
		if _, err := a.GenerateJSON(context.Background(), req); err == nil {
			t.Errorf("schema %q accepted", schema)
		}
	}
	req := p6aJSONRequest()
	req.Messages = nil
	if _, err := a.GenerateJSON(context.Background(), req); err == nil {
		t.Error("no messages accepted")
	}
	if hits.Load() != 0 {
		t.Fatalf("invalid requests reached the API %d times", hits.Load())
	}
	_, err = a.GenerateJSON(context.Background(), p6aJSONRequest())
	if err == nil || strings.Contains(err.Error(), "SECRET-PROMPT-TEXT") {
		t.Fatalf("http error=%v", err)
	}
}
