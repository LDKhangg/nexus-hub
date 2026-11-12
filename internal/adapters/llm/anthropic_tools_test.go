package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/config"
	"github.com/LDKhangg/nexus-hub/internal/domain"
)

type captured struct{ body map[string]any }

func toolServer(t *testing.T, status int, resp string, cap *captured) *Anthropic {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if cap != nil {
			_ = json.Unmarshal(b, &cap.body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, resp)
	}))
	t.Cleanup(srv.Close)
	a, err := NewAnthropic(config.AnthropicConfig{APIKey: "test-key", BaseURL: srv.URL, MaxRetries: 1}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

const endTurnResp = `{"id":"m1","type":"message","role":"assistant","model":"claude-sonnet-5-5","stop_reason":"end_turn",
 "content":[{"type":"text","text":"final answer"}],
 "usage":{"input_tokens":120,"output_tokens":30,"cache_read_input_tokens":80,"cache_creation_input_tokens":40}}`

const toolUseResp = `{"id":"m2","type":"message","role":"assistant","model":"claude-sonnet-5-5","stop_reason":"tool_use",
 "content":[{"type":"text","text":"checking"},
  {"type":"tool_use","id":"tu_1","name":"code_search","input":{"repo":"o/r","query":"x"}},
  {"type":"tool_use","id":"tu_2","name":"git_log","input":{"repo":"o/r"}}],
 "usage":{"input_tokens":10,"output_tokens":5}}`

func sampleRequest() *domain.ToolRequest {
	return &domain.ToolRequest{
		Model: "claude-sonnet-5-5", System: "be terse", MaxTokens: 1024, CacheStatic: true,
		Tools: []domain.ToolDef{
			{Name: "code_search", Description: "search", InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"repo":{"type":"string"}},"required":["repo"]}`)},
			{Name: "git_log", Description: "log", InputSchema: json.RawMessage(`{"type":"object","properties":{"repo":{"type":"string"}}}`)},
		},
		Turns: []domain.Turn{
			{Role: domain.UserRole, Text: "investigate"},
			{Role: domain.AIRole, Text: "let me look", Calls: []domain.ToolCall{{ID: "tu_0", Name: "git_log", Input: json.RawMessage(`{"repo":"o/r"}`)}}},
			{Role: domain.UserRole, Results: []domain.ToolResult{{CallID: "tu_0", Content: "denied: tool_not_allowed", IsError: true}}},
		},
	}
}

func TestAnthropicTools_RequestShape(t *testing.T) {
	cap := &captured{}
	a := toolServer(t, 200, endTurnResp, cap)
	if _, err := a.GenerateWithTools(context.Background(), sampleRequest()); err != nil {
		t.Fatal(err)
	}
	b := cap.body
	if b["model"] != "claude-sonnet-5-5" || b["max_tokens"].(float64) != 1024 {
		t.Fatalf("model/max_tokens %v %v", b["model"], b["max_tokens"])
	}
	sys := b["system"].([]any)[0].(map[string]any)
	if sys["text"] != "be terse" || sys["cache_control"].(map[string]any)["type"] != "ephemeral" {
		t.Fatalf("system %v", sys)
	}
	tl := b["tools"].([]any)
	first, last := tl[0].(map[string]any), tl[1].(map[string]any)
	if _, ok := first["cache_control"]; ok {
		t.Fatal("cache_control on first tool")
	}
	if last["cache_control"].(map[string]any)["type"] != "ephemeral" {
		t.Fatalf("last tool %v", last)
	}
	schema := first["input_schema"].(map[string]any)
	if schema["type"] != "object" || schema["additionalProperties"] != false || schema["required"].([]any)[0] != "repo" {
		t.Fatalf("schema %v", schema)
	}
	msgs := b["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages %d", len(msgs))
	}
	asst := msgs[1].(map[string]any)["content"].([]any)
	if asst[1].(map[string]any)["type"] != "tool_use" || asst[1].(map[string]any)["id"] != "tu_0" {
		t.Fatalf("assistant %v", asst)
	}
	res := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if res["type"] != "tool_result" || res["tool_use_id"] != "tu_0" || res["is_error"] != true {
		t.Fatalf("result %v", res)
	}
	// Without CacheStatic no cache_control is sent.
	cap2 := &captured{}
	a2 := toolServer(t, 200, endTurnResp, cap2)
	req := sampleRequest()
	req.CacheStatic = false
	if _, err := a2.GenerateWithTools(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(mustJSON(cap2.body), "cache_control") {
		t.Fatal("cache_control sent without CacheStatic")
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestAnthropicTools_ParsesToolUse(t *testing.T) {
	a := toolServer(t, 200, toolUseResp, nil)
	r, err := a.GenerateWithTools(context.Background(), sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	if r.Stop != domain.StopToolUse || len(r.Calls) != 2 || r.Calls[0].ID != "tu_1" || r.Calls[0].Name != "code_search" || r.Text != "checking" {
		t.Fatalf("%+v", r)
	}
	var in map[string]string
	if err := json.Unmarshal(r.Calls[0].Input, &in); err != nil || in["query"] != "x" {
		t.Fatalf("input %s", r.Calls[0].Input)
	}
}

func TestAnthropicTools_EndTurnText(t *testing.T) {
	a := toolServer(t, 200, endTurnResp, nil)
	r, err := a.GenerateWithTools(context.Background(), sampleRequest())
	if err != nil || r.Stop != domain.StopEndTurn || r.Text != "final answer" || len(r.Calls) != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	a = toolServer(t, 200, strings.Replace(endTurnResp, `"end_turn"`, `"max_tokens"`, 1), nil)
	if r, err := a.GenerateWithTools(context.Background(), sampleRequest()); err != nil || r.Stop != domain.StopMaxTokens {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestAnthropicTools_Refusal(t *testing.T) {
	a := toolServer(t, 200, strings.Replace(endTurnResp, `"end_turn"`, `"refusal"`, 1), nil)
	r, err := a.GenerateWithTools(context.Background(), sampleRequest())
	if err != nil || r.Stop != domain.StopRefusal {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestAnthropicTools_Usage(t *testing.T) {
	a := toolServer(t, 200, endTurnResp, nil)
	r, err := a.GenerateWithTools(context.Background(), sampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	if r.Usage != (domain.Usage{Input: 120, Output: 30, CacheRead: 80, CacheWrite: 40}) {
		t.Fatalf("usage %+v", r.Usage)
	}
	var u domain.Usage
	u.Add(r.Usage)
	u.Add(domain.Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4})
	if u != (domain.Usage{Input: 121, Output: 32, CacheRead: 83, CacheWrite: 44}) {
		t.Fatalf("add %+v", u)
	}
}

func TestAnthropicTools_HTTPErrorNoBodyEcho(t *testing.T) {
	a := toolServer(t, 400, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt contained SECRET-PROMPT-TEXT"}}`, nil)
	_, err := a.GenerateWithTools(context.Background(), sampleRequest())
	if err == nil || strings.Contains(err.Error(), "SECRET-PROMPT-TEXT") || !strings.Contains(err.Error(), "400") {
		t.Fatalf("err %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 || apiErr.Retryable {
		t.Fatalf("api error %+v", apiErr)
	}
}
