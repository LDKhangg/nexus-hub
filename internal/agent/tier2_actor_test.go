package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/memory"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// scriptedLLM returns responses in order and records deep copies of requests.
type scriptedLLM struct {
	mu        sync.Mutex
	responses []*domain.ToolResponse
	errs      []error
	reqs      []domain.ToolRequest
}

func (s *scriptedLLM) GenerateWithTools(_ context.Context, req *domain.ToolRequest) (*domain.ToolResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *req
	cp.Turns = nil
	for _, t := range req.Turns {
		tc := t
		tc.Results = append([]domain.ToolResult(nil), t.Results...)
		tc.Calls = append([]domain.ToolCall(nil), t.Calls...)
		cp.Turns = append(cp.Turns, tc)
	}
	s.reqs = append(s.reqs, cp)
	i := len(s.reqs) - 1
	if i < len(s.errs) && s.errs[i] != nil {
		return nil, s.errs[i]
	}
	if i >= len(s.responses) {
		return s.responses[len(s.responses)-1], nil
	}
	return s.responses[i], nil
}

func toolUse(calls ...domain.ToolCall) *domain.ToolResponse {
	return &domain.ToolResponse{Stop: domain.StopToolUse, Calls: calls, Usage: domain.Usage{Input: 10, Output: 5}}
}

func call(id, name, input string) domain.ToolCall {
	return domain.ToolCall{ID: id, Name: name, Input: json.RawMessage(input)}
}

func answer(text string) *domain.ToolResponse {
	return &domain.ToolResponse{Stop: domain.StopEndTurn, Text: text, Usage: domain.Usage{Input: 10, Output: 5}}
}

type actorTool struct {
	spec tools.ToolSpec
	out  string
	mu   sync.Mutex
	n    int
}

func (a *actorTool) Spec() tools.ToolSpec { return a.spec }
func (a *actorTool) Run(context.Context, tools.Call) (tools.ToolOutput, error) {
	a.mu.Lock()
	a.n++
	a.mu.Unlock()
	return tools.ToolOutput{Text: a.out}, nil
}

type memRuns struct {
	mu   sync.Mutex
	next int64
}

func (m *memRuns) Begin(context.Context, tools.RunStart) (int64, *tools.PriorRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	return m.next, nil, nil
}
func (m *memRuns) Finish(context.Context, tools.RunFinish) error      { return nil }
func (m *memRuns) Deny(context.Context, tools.RunStart, string) error { return nil }

type actorEnv struct {
	exec   *tools.Executor
	search *actorTool
	specs  []tools.ToolSpec
}

func newActorEnv(t *testing.T) actorEnv {
	t.Helper()
	search := &actorTool{spec: tools.ToolSpec{Name: "code_search", Description: "search code", Tier: tenant.TierRead, InputSchema: json.RawMessage(`{"type":"object"}`)}, out: strings.Repeat("hit ", 50)}
	logT := &actorTool{spec: tools.ToolSpec{Name: "git_log", Description: "log", Tier: tenant.TierRead, InputSchema: json.RawMessage(`{"type":"object"}`)}, out: "abc123 fix"}
	push := &actorTool{spec: tools.ToolSpec{Name: "github_pr_comment", Description: "comment", Tier: tenant.TierWrite, InputSchema: json.RawMessage(`{"type":"object"}`)}, out: "commented"}
	reg, err := tools.NewRegistry(search, logT, push)
	if err != nil {
		t.Fatal(err)
	}
	policy := tenant.ToolPolicyConfig{Enabled: []string{"code_search", "git_log", "github_pr_comment"}, MaxWritesPerJob: 2}
	exec := tools.NewExecutor(reg, policy, tools.DefaultLimits(), false, &memRuns{}, NewRedactor(nil), 7, "ws")
	return actorEnv{exec: exec, search: search, specs: reg.Specs([]string{"code_search", "git_log", "github_pr_comment"})}
}

func packet() memory.ContextPacket {
	return memory.ContextPacket{Title: "Login broken", TaskURL: "https://app.clickup.com/t/1",
		NewBatch: []memory.Message{{ID: 1, SenderRole: memory.SenderRoleExternalUser, SenderName: "ann", Content: "login fails after deploy"}}}
}

func decision() TriageDecision {
	return TriageDecision{Category: CategoryInvestigate, Urgency: UrgencyHigh, Confidence: 0.9, Summary: "login bug", Goal: "find the commit that broke login"}
}

func budget() TokenBudget { return TokenBudget{MaxInput: 60000, MaxOutput: 6000} }

func TestActor_SingleToolThenAnswer(t *testing.T) {
	e := newActorEnv(t)
	llm := &scriptedLLM{responses: []*domain.ToolResponse{toolUse(call("t1", "code_search", `{"query":"login"}`)), answer("- cause: commit abc")}}
	r, err := NewTier2Actor(llm, "claude-sonnet-5-5").Investigate(context.Background(), packet(), decision(), "", e.specs, e.exec, budget())
	if err != nil {
		t.Fatal(err)
	}
	if r.Text != "- cause: commit abc" || r.Steps != 2 || len(r.Calls) != 1 || r.Calls[0].Name != "code_search" || r.Calls[0].Status != "ok" || r.Stopped != "" {
		t.Fatalf("%+v", r)
	}
	if e.search.n != 1 || r.Usage != (domain.Usage{Input: 20, Output: 10}) {
		t.Fatalf("runs %d usage %+v", e.search.n, r.Usage)
	}
	res := llm.reqs[1].Turns[2]
	if res.Role != domain.UserRole || len(res.Results) != 1 || res.Results[0].CallID != "t1" || res.Results[0].IsError {
		t.Fatalf("result turn %+v", res)
	}
}

func TestActor_ParallelCalls(t *testing.T) {
	e := newActorEnv(t)
	llm := &scriptedLLM{responses: []*domain.ToolResponse{toolUse(call("a", "code_search", `{}`), call("b", "git_log", `{}`)), answer("done")}}
	r, err := NewTier2Actor(llm, "m").Investigate(context.Background(), packet(), decision(), "", e.specs, e.exec, budget())
	if err != nil || len(r.Calls) != 2 {
		t.Fatalf("%+v %v", r, err)
	}
	res := llm.reqs[1].Turns[2].Results
	if len(res) != 2 || res[0].CallID != "a" || res[1].CallID != "b" || !strings.Contains(res[1].Content, "abc123") {
		t.Fatalf("results %+v", res)
	}
}

func TestActor_DeniedFedBackAsError(t *testing.T) {
	e := newActorEnv(t)
	llm := &scriptedLLM{responses: []*domain.ToolResponse{toolUse(call("w", "github_pr_comment", `{"repo":"o/r"}`)), answer("could not comment")}}
	r, err := NewTier2Actor(llm, "m").Investigate(context.Background(), packet(), decision(), "", e.specs, e.exec, budget())
	if err != nil {
		t.Fatal(err)
	}
	res := llm.reqs[1].Turns[2].Results[0]
	if !res.IsError || !strings.Contains(res.Content, "denied") || r.Calls[0].Status != "denied" {
		t.Fatalf("%+v %+v", res, r.Calls)
	}
}

func TestActor_HallucinatedTool(t *testing.T) {
	e := newActorEnv(t)
	llm := &scriptedLLM{responses: []*domain.ToolResponse{toolUse(call("x", "rm_rf", `{}`)), answer("ok")}}
	r, err := NewTier2Actor(llm, "m").Investigate(context.Background(), packet(), decision(), "", e.specs, e.exec, budget())
	if err != nil {
		t.Fatal(err)
	}
	if res := llm.reqs[1].Turns[2].Results[0]; !res.IsError || !strings.Contains(res.Content, "unknown_tool") || r.Calls[0].Status != "denied" {
		t.Fatalf("%+v", res)
	}
}

func TestActor_StepLimit(t *testing.T) {
	e := newActorEnv(t)
	llm := &scriptedLLM{responses: []*domain.ToolResponse{{Stop: domain.StopToolUse, Text: "still looking", Calls: []domain.ToolCall{call("s", "git_log", `{}`)}}}}
	r, err := NewTier2Actor(llm, "m").Investigate(context.Background(), packet(), decision(), "", e.specs, e.exec, budget())
	if err != nil {
		t.Fatal(err)
	}
	if r.Stopped != "step_limit" || r.Steps != MaxActorSteps || len(llm.reqs) != MaxActorSteps {
		t.Fatalf("%+v reqs %d", r, len(llm.reqs))
	}
	if r.Text == "" {
		t.Fatal("no partial text kept")
	}
}

func TestActor_MaxTokensKeepsPartial(t *testing.T) {
	e := newActorEnv(t)
	llm := &scriptedLLM{responses: []*domain.ToolResponse{{Stop: domain.StopMaxTokens, Text: "partial findings", Usage: domain.Usage{Output: 1024}}}}
	r, err := NewTier2Actor(llm, "m").Investigate(context.Background(), packet(), decision(), "", e.specs, e.exec, budget())
	if err != nil || r.Stopped != "max_tokens" || r.Text != "partial findings" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestActor_TokenBudgetStops(t *testing.T) {
	e := newActorEnv(t)
	resp := toolUse(call("s", "git_log", `{}`))
	resp.Usage = domain.Usage{Input: 50, CacheRead: 30, Output: 5}
	llm := &scriptedLLM{responses: []*domain.ToolResponse{resp}}
	r, err := NewTier2Actor(llm, "m").Investigate(context.Background(), packet(), decision(), "", e.specs, e.exec, TokenBudget{MaxInput: 100, MaxOutput: 6000})
	if err != nil || r.Stopped != "token_budget" || len(llm.reqs) != 2 {
		t.Fatalf("%+v reqs %d err %v", r, len(llm.reqs), err)
	}
	// A budget already spent by an earlier attempt stops before any call.
	llm2 := &scriptedLLM{responses: []*domain.ToolResponse{answer("x")}}
	r2, _ := NewTier2Actor(llm2, "m").Investigate(context.Background(), packet(), decision(), "", e.specs, e.exec, TokenBudget{MaxInput: 100, MaxOutput: 6000, Used: domain.Usage{Output: 6000}})
	if r2.Stopped != "token_budget" || len(llm2.reqs) != 0 {
		t.Fatalf("%+v reqs %d", r2, len(llm2.reqs))
	}
}

func TestActor_LoopCompaction(t *testing.T) {
	e := newActorEnv(t)
	llm := &scriptedLLM{responses: []*domain.ToolResponse{
		toolUse(call("s1", "code_search", `{"q":1}`)), toolUse(call("s2", "code_search", `{"q":2}`)),
		toolUse(call("s3", "code_search", `{"q":3}`)), answer("done"),
	}}
	if _, err := NewTier2Actor(llm, "m").Investigate(context.Background(), packet(), decision(), "", e.specs, e.exec, budget()); err != nil {
		t.Fatal(err)
	}
	req := llm.reqs[3] // step 4
	var results []domain.ToolResult
	for _, turn := range req.Turns {
		results = append(results, turn.Results...)
	}
	if len(results) != 3 {
		t.Fatalf("results %d", len(results))
	}
	if results[0].CallID != "s1" || !strings.HasPrefix(results[0].Content, "[elided: code_search ok,") {
		t.Fatalf("step 1 not elided: %q", results[0].Content)
	}
	if strings.HasPrefix(results[1].Content, "[elided") || strings.HasPrefix(results[2].Content, "[elided") {
		t.Fatal("recent results elided")
	}
}

func TestActor_RequestSettings(t *testing.T) {
	e := newActorEnv(t)
	llm := &scriptedLLM{responses: []*domain.ToolResponse{answer("done")}}
	if _, err := NewTier2Actor(llm, "claude-sonnet-5-5").Investigate(context.Background(), packet(), decision(), "Be formal.", e.specs, e.exec, budget()); err != nil {
		t.Fatal(err)
	}
	req := llm.reqs[0]
	if req.Model != "claude-sonnet-5-5" || req.MaxTokens != ActorMaxTokens || !req.CacheStatic || len(req.Tools) != 3 {
		t.Fatalf("%+v", req)
	}
	if !strings.Contains(req.System, "untrusted") || !strings.Contains(req.System, "Be formal.") {
		t.Fatalf("system %q", req.System)
	}
	first := req.Turns[0].Text
	if !strings.Contains(first, "<ticket_content>") || !strings.Contains(first, "find the commit that broke login") {
		t.Fatalf("first turn %q", first)
	}
}

func TestActor_Refusal(t *testing.T) {
	e := newActorEnv(t)
	llm := &scriptedLLM{responses: []*domain.ToolResponse{{Stop: domain.StopRefusal}}}
	_, err := NewTier2Actor(llm, "m").Investigate(context.Background(), packet(), decision(), "", e.specs, e.exec, budget())
	var te *TriageError
	if !errors.As(err, &te) || te.Code != "llm_refused" || te.Retryable {
		t.Fatalf("err %v", err)
	}
	llm2 := &scriptedLLM{errs: []error{errors.New("boom")}, responses: []*domain.ToolResponse{answer("x")}}
	_, err = NewTier2Actor(llm2, "m").Investigate(context.Background(), packet(), decision(), "", e.specs, e.exec, budget())
	if !errors.As(err, &te) || te.Code != "llm_error" || !te.Retryable {
		t.Fatalf("err %v", err)
	}
}

func TestActor_ReportClipped(t *testing.T) {
	e := newActorEnv(t)
	var b strings.Builder
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&b, "- finding %d %s\n", i, strings.Repeat("ữ", 150))
	}
	llm := &scriptedLLM{responses: []*domain.ToolResponse{answer(b.String())}}
	r, _ := NewTier2Actor(llm, "m").Investigate(context.Background(), packet(), decision(), "", e.specs, e.exec, budget())
	if n := strings.Count(r.Text, "- finding"); n > 8 {
		t.Fatalf("%d bullets", n)
	}
	if len([]rune(r.Text)) > ReportMaxRunes {
		t.Fatalf("%d runes", len([]rune(r.Text)))
	}
}
