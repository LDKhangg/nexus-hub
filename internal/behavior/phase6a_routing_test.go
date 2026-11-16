package behavior

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// Phase 6a: assistant routing, history window, stable system prompt, usage
// log, model footer and /deep. Fakes only.

const (
	p6aWork = "claude-sonnet-5-5"
	p6aFast = "claude-haiku-5-5"
	p6aDeep = "claude-opus-5-5"
)

type p6aJSONStep struct {
	resp *domain.JSONResponse
	err  error
}

// p6aModel is an API-like provider: plain, tool-calling and structured.
type p6aModel struct {
	mu        sync.Mutex
	json      []p6aJSONStep
	jsonHook  func(ctx context.Context)
	jsonReqs  []domain.JSONRequest
	tools     []*domain.ToolResponse
	toolErr   error
	toolReqs  []domain.ToolRequest
	plainReqs []domain.LLMRequest
}

var p6aToolUsage = domain.Usage{Input: 100, Output: 20, CacheRead: 30, CacheWrite: 40}

func (m *p6aModel) Generate(_ context.Context, req *domain.LLMRequest) (*domain.LLMResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *req
	cp.Messages = append([]domain.Message(nil), req.Messages...)
	m.plainReqs = append(m.plainReqs, cp)
	return &domain.LLMResponse{Text: "plain"}, nil
}

func (m *p6aModel) GenerateWithTools(_ context.Context, req *domain.ToolRequest) (*domain.ToolResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *req
	cp.Tools = append([]domain.ToolDef(nil), req.Tools...)
	cp.Turns = append([]domain.Turn(nil), req.Turns...)
	m.toolReqs = append(m.toolReqs, cp)
	if m.toolErr != nil {
		return nil, m.toolErr
	}
	i := len(m.toolReqs) - 1
	if i < len(m.tools) {
		return m.tools[i], nil
	}
	return &domain.ToolResponse{Text: "xong", Stop: domain.StopEndTurn, Usage: p6aToolUsage}, nil
}

func (m *p6aModel) GenerateJSON(ctx context.Context, req *domain.JSONRequest) (*domain.JSONResponse, error) {
	m.mu.Lock()
	cp := *req
	cp.Messages = append([]domain.Message(nil), req.Messages...)
	m.jsonReqs = append(m.jsonReqs, cp)
	i, hook := len(m.jsonReqs)-1, m.jsonHook
	var step p6aJSONStep
	if i < len(m.json) {
		step = m.json[i]
	} else if len(m.json) > 0 {
		step = m.json[len(m.json)-1]
	} else {
		step = p6aJSONStep{resp: p6aRouter(`{"route":"escalate","tier":"work"}`)}
	}
	m.mu.Unlock()
	if hook != nil {
		hook(ctx)
	}
	return step.resp, step.err
}

func p6aRouter(raw string) *domain.JSONResponse {
	return &domain.JSONResponse{JSON: json.RawMessage(raw), Stop: domain.StopEndTurn, Usage: domain.Usage{Input: 10, Output: 2, CacheRead: 3, CacheWrite: 4}}
}

func p6aEnv(m domain.LLMProvider) *p4Env {
	e := newP4Env()
	e.assistant.Model, e.assistant.FastModel, e.assistant.DeepModel = p6aWork, p6aFast, p6aDeep
	e.assistant.Models = func(context.Context, string) (domain.LLMProvider, error) { return m, nil }
	return e
}

func p6aJob(text string, workflowID int64, tier string) assistantJob {
	return assistantJob{u: InboundUpdate{WorkspaceID: p4WS, IntegrationID: "bot-a", ChatID: 77},
		to: ChatRef{WorkspaceID: p4WS, IntegrationID: "bot-a", ChatID: 77}, actor: "owner", corr: "corr",
		text: text, workflowID: workflowID, tier: tier}
}

// p6aLog captures slog (and the log package) for one test.
type p6aLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *p6aLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *p6aLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func p6aCaptureLog(t *testing.T) *p6aLog {
	t.Helper()
	prev, prevW, prevF := slog.Default(), log.Writer(), log.Flags()
	l := &p6aLog{}
	slog.SetDefault(slog.New(slog.NewJSONHandler(l, nil)))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevW)
		log.SetFlags(prevF)
	})
	return l
}

// turns returns the "assistant turn" lines.
func (l *p6aLog) turns(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue // a log-package line routed through the handler
		}
		if m["msg"] == "assistant turn" {
			out = append(out, m)
		}
	}
	return out
}

// lastTurn returns the single new usage line and checks its route and model.
func (l *p6aLog) requireTurn(t *testing.T, n int, route, model string) map[string]any {
	t.Helper()
	turns := l.turns(t)
	if len(turns) != n {
		t.Fatalf("usage lines=%d, want %d: %s", len(turns), n, l.String())
	}
	got := turns[n-1]
	if got["route"] != route || got["model"] != model {
		t.Fatalf("usage line route=%v model=%v, want %s %s", got["route"], got["model"], route, model)
	}
	return got
}

// TC-6A30 (REQ-7, REQ-5, REQ-4): a simple message is answered by the fast
// model alone: one structured call with the router prompt, schema,
// MaxTokens 600, CacheHistory, history plus the owner's text; no tools; the
// sent answer ends with "· haiku"; the next turn's history has no footer.
func TestPhase6aRouter_FastAnswerUsesOnlyTheFastModel(t *testing.T) {
	logs := p6aCaptureLog(t)
	m := &p6aModel{json: []p6aJSONStep{{resp: p6aRouter(`{"route":"answer","reply":"  Không có gì! 🙂  "}`)}, {resp: p6aRouter(`{"route":"answer","reply":"Chào bạn"}`)}}}
	e := p6aEnv(m)
	if out := e.say(t, "cảm ơn nhé"); out.code != "assistant_answer" {
		t.Fatalf("outcome=%+v", out)
	}
	if got := e.msg.texts(); len(got) != 1 || got[0] != "Không có gì! 🙂\n· haiku" {
		t.Fatalf("sent=%q", got)
	}
	if len(m.toolReqs) != 0 || len(m.plainReqs) != 0 || len(m.jsonReqs) != 1 {
		t.Fatalf("calls: tools=%d plain=%d json=%d", len(m.toolReqs), len(m.plainReqs), len(m.jsonReqs))
	}
	r := m.jsonReqs[0]
	if r.Model != p6aFast || r.MaxTokens != 600 || !r.CacheHistory || r.System != assistantRouterSystem || string(r.Schema) != assistantRouterSchema {
		t.Fatalf("router request model=%q max=%d cache=%v", r.Model, r.MaxTokens, r.CacheHistory)
	}
	if len(r.Messages) != 1 || r.Messages[0].Role != domain.UserRole || r.Messages[0].Text != "cảm ơn nhé" {
		t.Fatalf("router messages=%+v", r.Messages)
	}
	line := logs.requireTurn(t, 1, routeAnswer, p6aFast)
	if line["steps"] != float64(1) || line["input_tokens"] != float64(10) || line["output_tokens"] != float64(2) || line["cache_read_tokens"] != float64(3) || line["cache_write_tokens"] != float64(4) {
		t.Fatalf("usage line=%v", line)
	}

	if out := e.say(t, "chào"); out.code != "assistant_answer" {
		t.Fatalf("second=%+v", out)
	}
	msgs := m.jsonReqs[1].Messages
	if len(msgs) != 3 || msgs[0].Text != "cảm ơn nhé" || msgs[1].Text != "Không có gì! 🙂" || msgs[2].Text != "chào" {
		t.Fatalf("second router messages=%+v", msgs)
	}
	for _, msg := range msgs {
		if strings.Contains(msg.Text, "· haiku") {
			t.Fatalf("footer reached the history: %q", msg.Text)
		}
	}
}

// TC-6A31 (REQ-7, REQ-5, REQ-4): escalate work runs the tool loop on
// assistant_model with tools, escalate deep on assistant_deep_model; the
// footer names the model used; router usage and step are added.
func TestPhase6aRouter_EscalationPicksTheModel(t *testing.T) {
	for _, tc := range []struct {
		tier, model, short, route string
	}{
		{"work", p6aWork, "sonnet", routeWork},
		{"deep", p6aDeep, "opus", routeDeep},
	} {
		t.Run(tc.tier, func(t *testing.T) {
			logs := p6aCaptureLog(t)
			m := &p6aModel{json: []p6aJSONStep{{resp: p6aRouter(`{"route":"escalate","tier":"` + tc.tier + `"}`)}}}
			e := p6aEnv(m)
			if out := e.say(t, "task 86xyz đang thế nào?"); out.code != "assistant_answer" {
				t.Fatalf("outcome=%+v", out)
			}
			if len(m.toolReqs) != 1 || m.toolReqs[0].Model != tc.model || len(m.toolReqs[0].Tools) == 0 || !m.toolReqs[0].CacheHistory || !m.toolReqs[0].CacheStatic {
				t.Fatalf("tool requests=%d model=%q", len(m.toolReqs), m.toolReqs[0].Model)
			}
			if got := e.msg.texts(); len(got) != 1 || got[0] != "xong\n· "+tc.short {
				t.Fatalf("sent=%q", got)
			}
			line := logs.requireTurn(t, 1, tc.route, tc.model)
			if line["steps"] != float64(2) || line["input_tokens"] != float64(110) || line["output_tokens"] != float64(22) || line["cache_read_tokens"] != float64(33) || line["cache_write_tokens"] != float64(44) {
				t.Fatalf("usage line=%v", line)
			}
		})
	}
}

// TC-6A32 (REQ-7, REQ-9): every router failure fails upward to
// assistant_model; the fast reply is never sent.
func TestPhase6aRouter_FailuresFallBackToWork(t *testing.T) {
	long := `{"route":"answer","reply":"` + strings.Repeat("á", assistantRouterMaxReplyRunes+1) + `"}`
	answer := p6aRouter(`{"route":"answer","reply":"FAST-REPLY"}`)
	stop := func(s domain.StopReason) *domain.JSONResponse {
		r := *answer
		r.Stop = s
		return &r
	}
	cases := map[string]p6aJSONStep{
		"provider error":      {err: errors.New("boom")},
		"timeout":             {err: context.DeadlineExceeded},
		"nil response":        {},
		"refusal":             {resp: stop(domain.StopRefusal)},
		"max tokens":          {resp: stop(domain.StopMaxTokens)},
		"tool use stop":       {resp: stop(domain.StopToolUse)},
		"invalid json":        {resp: p6aRouter(`{"route":"answer","reply":"FAST-REPLY"`)},
		"empty":               {resp: p6aRouter(``)},
		"null":                {resp: p6aRouter(`null`)},
		"array":               {resp: p6aRouter(`[{"route":"answer","reply":"FAST-REPLY"}]`)},
		"trailing object":     {resp: p6aRouter(`{"route":"answer","reply":"FAST-REPLY"} {"route":"answer"}`)},
		"trailing text":       {resp: p6aRouter(`{"route":"answer","reply":"FAST-REPLY"} ok`)},
		"trailing brace":      {resp: p6aRouter(`{"route":"answer","reply":"FAST-REPLY"}}`)},
		"unknown field":       {resp: p6aRouter(`{"route":"answer","reply":"FAST-REPLY","confidence":0.9}`)},
		"missing route":       {resp: p6aRouter(`{"reply":"FAST-REPLY"}`)},
		"null route":          {resp: p6aRouter(`{"route":null,"reply":"FAST-REPLY"}`)},
		"unknown route":       {resp: p6aRouter(`{"route":"maybe","reply":"FAST-REPLY"}`)},
		"answer no reply":     {resp: p6aRouter(`{"route":"answer"}`)},
		"answer empty reply":  {resp: p6aRouter(`{"route":"answer","reply":" \n\t "}`)},
		"answer long reply":   {resp: p6aRouter(long)},
		"answer with tier":    {resp: p6aRouter(`{"route":"answer","reply":"FAST-REPLY","tier":"work"}`)},
		"escalate no tier":    {resp: p6aRouter(`{"route":"escalate"}`)},
		"escalate bad tier":   {resp: p6aRouter(`{"route":"escalate","tier":"fast"}`)},
		"escalate tier case":  {resp: p6aRouter(`{"route":"escalate","tier":"Deep"}`)},
		"route case":          {resp: p6aRouter(`{"route":"Answer","reply":"FAST-REPLY"}`)},
		"duplicate key trick": {resp: p6aRouter(`{"route":"escalate","route":"answer","reply":"FAST-REPLY","tier":"work"}`)},
	}
	for name, step := range cases {
		t.Run(name, func(t *testing.T) {
			logs := p6aCaptureLog(t)
			m := &p6aModel{json: []p6aJSONStep{step}}
			e := p6aEnv(m)
			if out := e.say(t, "cảm ơn nhé"); out.code != "assistant_answer" {
				t.Fatalf("outcome=%+v", out)
			}
			if len(m.toolReqs) != 1 || m.toolReqs[0].Model != p6aWork {
				t.Fatalf("tool requests=%d, want one on %s", len(m.toolReqs), p6aWork)
			}
			got := e.msg.texts()
			if len(got) != 1 || got[0] != "xong\n· sonnet" || strings.Contains(strings.Join(got, ""), "FAST-REPLY") {
				t.Fatalf("sent=%q", got)
			}
			logs.requireTurn(t, 1, routeWorkFallback, p6aWork)
		})
	}
}

// TC-6A33 (REQ-7, REQ-9): the reply limit is inclusive at 2,000 runes.
func TestPhase6aRouter_ReplyLimitBoundary(t *testing.T) {
	reply := strings.Repeat("á", assistantRouterMaxReplyRunes)
	m := &p6aModel{json: []p6aJSONStep{{resp: p6aRouter(`{"route":"answer","reply":"` + reply + `"}`)}}}
	e := p6aEnv(m)
	if out := e.say(t, "kể một câu chuyện ngắn"); out.code != "assistant_answer" || len(m.toolReqs) != 0 {
		t.Fatalf("outcome=%+v tools=%d", out, len(m.toolReqs))
	}
	if got := strings.Join(e.msg.texts(), ""); !strings.HasPrefix(got, reply) || !strings.HasSuffix(got, "\n· haiku") {
		t.Fatalf("sent runes=%d", utf8.RuneCountInString(got))
	}
}

// TC-6A34 (REQ-7): the router call has its own deadline shorter than the
// turn, so a slow fast model leaves time for the work model.
func TestPhase6aRouter_HasItsOwnDeadline(t *testing.T) {
	var remaining time.Duration
	m := &p6aModel{json: []p6aJSONStep{{err: context.DeadlineExceeded}}}
	m.jsonHook = func(ctx context.Context) {
		if d, ok := ctx.Deadline(); ok {
			remaining = time.Until(d)
		}
	}
	e := p6aEnv(m)
	e.assistant.Timeout = 10 * time.Minute
	if out := e.say(t, "chào"); out.code != "assistant_answer" || len(m.toolReqs) != 1 {
		t.Fatalf("outcome=%+v tools=%d", out, len(m.toolReqs))
	}
	if remaining <= 0 || remaining > assistantRouterTimeout || assistantRouterTimeout >= assistantDefaultTimeout {
		t.Fatalf("router deadline in %v (router timeout %v)", remaining, assistantRouterTimeout)
	}
}

// TC-6A35 (REQ-7 skip rules): a CLI-like provider without structured output,
// a tool-only provider, or an unset fast model never calls the router and
// keep today's path on assistant_model (route skip).
func TestPhase6aRouter_SkipRules(t *testing.T) {
	t.Run("plain provider", func(t *testing.T) {
		logs := p6aCaptureLog(t)
		e := p6aEnv(nil)
		e.llm.replies = []string{"trả lời"}
		e.assistant.Models = func(context.Context, string) (domain.LLMProvider, error) { return e.llm, nil }
		if out := e.say(t, "cảm ơn nhé"); out.code != "assistant_answer" {
			t.Fatalf("outcome=%+v", out)
		}
		if len(e.llm.reqs) != 1 || e.llm.reqs[0].Model != p6aWork || e.llm.reqs[0].System != assistantSystem || !e.llm.reqs[0].CacheHistory {
			t.Fatalf("plain requests=%+v", e.llm.reqs)
		}
		if got := e.msg.texts(); len(got) != 1 || got[0] != "trả lời\n· sonnet" {
			t.Fatalf("sent=%q", got)
		}
		logs.requireTurn(t, 1, routeSkip, p6aWork)
	})
	t.Run("tool provider without json", func(t *testing.T) {
		logs := p6aCaptureLog(t)
		tm := &d5ToolModel{}
		e := p6aEnv(tm)
		if out := e.say(t, "cảm ơn nhé"); out.code != "assistant_answer" || len(tm.reqs) != 1 || tm.reqs[0].Model != p6aWork {
			t.Fatalf("outcome=%+v reqs=%d", out, len(tm.reqs))
		}
		logs.requireTurn(t, 1, routeSkip, p6aWork)
	})
	t.Run("no fast model", func(t *testing.T) {
		logs := p6aCaptureLog(t)
		m := &p6aModel{json: []p6aJSONStep{{resp: p6aRouter(`{"route":"answer","reply":"FAST-REPLY"}`)}}}
		e := p6aEnv(m)
		e.assistant.FastModel = ""
		if out := e.say(t, "cảm ơn nhé"); out.code != "assistant_answer" || len(m.jsonReqs) != 0 || len(m.toolReqs) != 1 || m.toolReqs[0].Model != p6aWork {
			t.Fatalf("outcome=%+v json=%d tools=%d", out, len(m.jsonReqs), len(m.toolReqs))
		}
		logs.requireTurn(t, 1, routeSkip, p6aWork)
	})
}

// TC-6A36 (REQ-3, REQ-7 skip): the system prompt is byte-identical for a
// plain turn, a workflow-targeted turn and a /deep turn; the WF hint is the
// owner turn's prefix; a workflow turn skips the router; the stored history
// keeps the owner's original text.
func TestPhase6aSystemPromptStableAcrossWorkflowTurns(t *testing.T) {
	m := &p6aModel{}
	e := p6aEnv(m)
	ctx := context.Background()
	plain, err := e.w.converse(ctx, p6aJob("chào", 0, ""))
	if err != nil || plain.route != routeWork {
		t.Fatalf("plain: %+v %v", plain, err)
	}
	wf, err := e.w.converse(ctx, p6aJob("còn bao lâu nữa?", 101, ""))
	if err != nil || wf.route != routeSkip || wf.model != p6aWork {
		t.Fatalf("workflow: route=%q model=%q err=%v", wf.route, wf.model, err)
	}
	if len(m.jsonReqs) != 1 {
		t.Fatalf("router calls=%d; the workflow turn must skip it", len(m.jsonReqs))
	}
	deep, err := e.w.converse(ctx, p6aJob("phân tích", 0, assistantTierDeep))
	if err != nil || deep.route != routeDeep || deep.model != p6aDeep || len(m.jsonReqs) != 1 {
		t.Fatalf("deep: route=%q model=%q json=%d err=%v", deep.route, deep.model, len(m.jsonReqs), err)
	}
	if len(m.toolReqs) != 3 {
		t.Fatalf("tool requests=%d", len(m.toolReqs))
	}
	for i, r := range m.toolReqs {
		if r.System != assistantSystem {
			t.Fatalf("request %d system differs from the constant prompt", i)
		}
	}
	owner := m.toolReqs[1].Turns[len(m.toolReqs[1].Turns)-1].Text
	if !strings.HasPrefix(owner, "[Context] This message replies to WF-101.") || !strings.HasSuffix(owner, "\ncòn bao lâu nữa?") {
		t.Fatalf("workflow owner turn=%q", owner)
	}
	if strings.Contains(m.toolReqs[0].Turns[len(m.toolReqs[0].Turns)-1].Text, "[Context]") {
		t.Fatal("plain turn got the workflow prefix")
	}
	hist, _ := e.assistant.chatHistoryFor(p4WS, "bot-a", 77)
	for _, msg := range hist.messagesForTurns(20) {
		if strings.Contains(msg.Text, "[Context]") || strings.Contains(msg.Text, "WF-101") {
			t.Fatalf("stored history carries the hint: %q", msg.Text)
		}
	}

	// The plain-provider path uses the same constant system and prefix.
	pe := p6aEnv(nil)
	pe.assistant.Models = func(context.Context, string) (domain.LLMProvider, error) { return pe.llm, nil }
	if _, err := pe.w.converse(ctx, p6aJob("còn bao lâu nữa?", 101, "")); err != nil {
		t.Fatal(err)
	}
	r := pe.llm.reqs[0]
	if r.System != assistantSystem || !strings.HasPrefix(r.Messages[len(r.Messages)-1].Text, "[Context] This message replies to WF-101.") {
		t.Fatalf("plain workflow request system/prefix wrong: last=%q", r.Messages[len(r.Messages)-1].Text)
	}
}

// TC-6A37 (REQ-2): the router and the tool loop get the persisted summary
// plus the newest assistant_history_turns exchanges; the buffer keeps more.
func TestPhase6aHistoryWindowIsSummaryPlusLastTurns(t *testing.T) {
	store := &phase6ConversationFake{summary: "older fact"}
	m := &p6aModel{}
	e := p6aEnv(m)
	e.w.d.Conversations = store
	e.assistant.Conversations = store
	e.assistant.HistoryTurns = 2
	ctx := context.Background()
	if _, err := e.w.converse(ctx, p6aJob("q0", 0, "")); err != nil {
		t.Fatal(err)
	}
	hist, _ := e.assistant.chatHistoryFor(p4WS, "bot-a", 77)
	for _, q := range []string{"q1", "q2", "q3", "q4", "q5"} {
		hist.add(q, "a"+q[1:])
	}
	if _, err := e.w.converse(ctx, p6aJob("q6", 0, "")); err != nil {
		t.Fatal(err)
	}
	want := []string{"older fact", "q4", "a4", "q5", "a5", "q6"}
	check := func(what string, texts []string) {
		t.Helper()
		if len(texts) != len(want) {
			t.Fatalf("%s texts=%q, want %q", what, texts, want)
		}
		for i, w := range want {
			if i == 0 {
				if !strings.HasPrefix(texts[0], "[Hub note") || !strings.Contains(texts[0], w) {
					t.Fatalf("%s summary=%q", what, texts[0])
				}
				continue
			}
			if texts[i] != w {
				t.Fatalf("%s [%d]=%q, want %q (all %q)", what, i, texts[i], w, texts)
			}
		}
	}
	var routerTexts, toolTexts []string
	for _, msg := range m.jsonReqs[1].Messages {
		routerTexts = append(routerTexts, msg.Text)
	}
	tr := m.toolReqs[1]
	for _, turn := range tr.Turns {
		toolTexts = append(toolTexts, turn.Text)
	}
	check("router", routerTexts)
	check("tool loop", toolTexts)
	if n := len(hist.messages()); n < 2*7 {
		t.Fatalf("in-memory buffer shrank to %d messages", n)
	}
}

// TC-6A38 (REQ-2): the window defaults to 6, is clamped to the 20-exchange
// buffer, and the 64 KiB and per-answer bounds still hold.
func TestPhase6aHistoryWindowBounds(t *testing.T) {
	a := &Assistant{}
	if a.historyTurns() != 6 {
		t.Fatalf("default window=%d", a.historyTurns())
	}
	a.HistoryTurns = 50
	if a.historyTurns() != assistantHistoryTurns {
		t.Fatalf("clamped window=%d", a.historyTurns())
	}
	h := &assistantHistory{}
	for i := 0; i < 25; i++ {
		h.add("câu hỏi", "trả lời")
	}
	if got := h.messagesForTurns(a.historyTurns()); len(got) != 2*assistantHistoryTurns {
		t.Fatalf("messages=%d", len(got))
	}
	if got := h.messagesForTurns(0); len(got) != 2 {
		t.Fatalf("window 0 messages=%d, want the newest exchange", len(got))
	}
	big := &assistantHistory{}
	big.setSummary(strings.Repeat("s", 4000))
	for i := 0; i < 20; i++ {
		big.add(strings.Repeat("q", 2000), strings.Repeat("ả", 20000))
	}
	total := 0
	for _, msg := range big.messagesForTurns(20) {
		total += len(msg.Text)
		if msg.Role == domain.AIRole && utf8.RuneCountInString(msg.Text) > assistantHistoryAnswerRunes {
			t.Fatalf("answer runes=%d", utf8.RuneCountInString(msg.Text))
		}
	}
	if total > assistantHistoryBytes {
		t.Fatalf("history bytes=%d > %d", total, assistantHistoryBytes)
	}
}

// TC-6A39 (REQ-5): the footer is the family name, else the id without
// "claude-", at most 32 bytes and valid UTF-8.
func TestPhase6aModelShortName(t *testing.T) {
	for in, want := range map[string]string{
		"claude-haiku-5-5":                  "haiku",
		"claude-sonnet-4-5-20250929":        "sonnet",
		"claude-opus-5-5":                   "opus",
		"Claude-OPUS-5":                     "opus",
		"claude-fable-1":                    "fable-1",
		"mymodel":                           "mymodel",
		"claude-" + strings.Repeat("x", 40): strings.Repeat("x", 32),
	} {
		if got := assistantModelShortName(in); got != want {
			t.Errorf("short(%q)=%q, want %q", in, got, want)
		}
	}
	got := assistantModelShortName("claude-" + strings.Repeat("ế", 20))
	if len(got) > 32 || !utf8.ValidString(got) || got == "" {
		t.Fatalf("multibyte short=%q len=%d", got, len(got))
	}
}

// TC-6A40 (REQ-5): only a sent answer carries the footer; failure, refusal
// and empty-answer messages do not; a split answer carries it once, last.
func TestPhase6aFooterOnlyOnSentAnswers(t *testing.T) {
	for name, tc := range map[string]struct {
		model *p6aModel
		want  string
	}{
		"model error": {&p6aModel{toolErr: errors.New("down")}, msgFailed},
		"refusal":     {&p6aModel{tools: []*domain.ToolResponse{{Stop: domain.StopRefusal}}}, msgAssistantRefused},
		"empty":       {&p6aModel{tools: []*domain.ToolResponse{{Stop: domain.StopEndTurn}}}, msgAssistantNotUnderstood},
	} {
		t.Run(name, func(t *testing.T) {
			e := p6aEnv(tc.model)
			e.say(t, "task 1?")
			if got := e.msg.texts(); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("sent=%q, want %q", got, tc.want)
			}
		})
	}
	long := strings.Repeat("dài ", 3000)
	m := &p6aModel{tools: []*domain.ToolResponse{{Text: long, Stop: domain.StopEndTurn}}}
	e := p6aEnv(m)
	e.say(t, "viết dài")
	parts := e.msg.texts()
	if len(parts) < 2 || strings.Count(strings.Join(parts, ""), "· sonnet") != 1 || !strings.HasSuffix(parts[len(parts)-1], "\n· sonnet") {
		t.Fatalf("parts=%d last=%q", len(parts), parts[len(parts)-1])
	}
}

// TC-6A41 (REQ-4): one usage line per finished turn with only metadata:
// no owner text, answer, reply or prompt in any log output.
func TestPhase6aUsageLogHasNoText(t *testing.T) {
	logs := p6aCaptureLog(t)
	m := &p6aModel{json: []p6aJSONStep{
		{resp: p6aRouter(`{"route":"answer","reply":"BIMAT-REPLY"}`)},
		{resp: p6aRouter(`{"route":"escalate","tier":"work"}`)},
	}, tools: nil}
	e := p6aEnv(m)
	e.say(t, "BIMAT-OWNER-1 chào")
	m.tools = []*domain.ToolResponse{{Text: "BIMAT-ANSWER", Stop: domain.StopEndTurn, Usage: p6aToolUsage}}
	e.say(t, "BIMAT-OWNER-2 task?")
	m.toolErr = errors.New("BIMAT-ERR provider")
	e.say(t, "BIMAT-OWNER-3 task?")
	turns := logs.turns(t)
	if len(turns) != 3 {
		t.Fatalf("usage lines=%d: %s", len(turns), logs.String())
	}
	allowed := map[string]bool{"time": true, "level": true, "msg": true, "route": true, "model": true, "steps": true,
		"input_tokens": true, "output_tokens": true, "cache_read_tokens": true, "cache_write_tokens": true, "latency_ms": true}
	for i, line := range turns {
		for k := range allowed {
			if _, ok := line[k]; !ok {
				t.Errorf("line %d misses %s", i, k)
			}
		}
		for k := range line {
			if !allowed[k] {
				t.Errorf("line %d has extra field %s", i, k)
			}
		}
	}
	if turns[0]["route"] != routeAnswer || turns[1]["route"] != routeWork || turns[2]["route"] != routeWork {
		t.Fatalf("routes=%v %v %v", turns[0]["route"], turns[1]["route"], turns[2]["route"])
	}
	out := logs.String()
	for _, secret := range []string{"BIMAT", "chào", "first-pass router", assistantSystem[:40]} {
		if strings.Contains(out, secret) {
			t.Fatalf("log contains %q", secret)
		}
	}
}

// TC-6A42 (REQ-8): /deep runs the tool loop on assistant_deep_model without
// the router, through the queue; the staged form takes the text from the
// inbox; the footer is opus.
func TestPhase6aDeepCommand(t *testing.T) {
	logs := p6aCaptureLog(t)
	m := &p6aModel{}
	e := p6aEnv(m)
	if out := e.command("/deep", "phân tích kiến trúc hàng đợi"); out.code != "assistant_queued" || len(m.toolReqs) != 0 || e.msg.count() != 0 {
		t.Fatalf("queue step=%+v tools=%d sent=%d", out, len(m.toolReqs), e.msg.count())
	}
	if out := e.runQueued(t); out.code != "assistant_answer" {
		t.Fatalf("run=%+v", out)
	}
	if len(m.jsonReqs) != 0 || len(m.toolReqs) != 1 || m.toolReqs[0].Model != p6aDeep || len(m.toolReqs[0].Tools) == 0 {
		t.Fatalf("json=%d tools=%d", len(m.jsonReqs), len(m.toolReqs))
	}
	if last := m.toolReqs[0].Turns[len(m.toolReqs[0].Turns)-1].Text; last != "phân tích kiến trúc hàng đợi" {
		t.Fatalf("owner turn=%q", last)
	}
	if got := e.msg.texts(); len(got) != 1 || got[0] != "xong\n· opus" {
		t.Fatalf("sent=%q", got)
	}
	logs.requireTurn(t, 1, routeDeep, p6aDeep)

	key, err := e.inbox.StageAssistantText(p4WS, "câu hỏi khó 🙂", e.clock)
	if err != nil {
		t.Fatal(err)
	}
	if out := e.command("/deep", AssistantTextPrefix+key); out.code != "assistant_queued" {
		t.Fatalf("staged=%+v", out)
	}
	e.runQueued(t)
	if last := m.toolReqs[1].Turns[len(m.toolReqs[1].Turns)-1].Text; last != "câu hỏi khó 🙂" || m.toolReqs[1].Model != p6aDeep {
		t.Fatalf("staged owner turn=%q model=%q", last, m.toolReqs[1].Model)
	}
	if out := e.command("/deep", AssistantTextPrefix+key); out.code != "assistant_text_gone" || e.msg.last(t).Text != msgAssistantGone {
		t.Fatalf("reused key=%+v", out)
	}

	// Unset deep model: assistant_model, footer sonnet.
	m2 := &p6aModel{}
	e2 := p6aEnv(m2)
	e2.assistant.DeepModel = ""
	e2.command("/deep", "x")
	e2.runQueued(t)
	if m2.toolReqs[0].Model != p6aWork || e2.msg.texts()[0] != "xong\n· sonnet" {
		t.Fatalf("no deep model: model=%q sent=%q", m2.toolReqs[0].Model, e2.msg.texts())
	}
}

// TC-6A43 (REQ-8): /deep alone, blank or a staged blank text gets the usage
// hint without the model; the rate limit and the queue apply; the assistant
// off gives the /help hint; /help lists /deep.
func TestPhase6aDeepCommandEdges(t *testing.T) {
	m := &p6aModel{}
	e := p6aEnv(m)
	blank, _ := e.inbox.StageAssistantText(p4WS, "   ", e.clock)
	for _, args := range []string{"", "   ", AssistantTextPrefix + blank} {
		out := e.command("/deep", args)
		if out.code != "usage" || e.msg.last(t).Text != usageDeep || len(m.toolReqs)+len(m.jsonReqs) != 0 {
			t.Fatalf("args %q: out=%+v text=%q", args, out, e.msg.last(t).Text)
		}
		select {
		case <-e.w.assistantJobs:
			t.Fatalf("args %q queued a job", args)
		default:
		}
	}
	if strings.Contains(usageDeep, "·") || !strings.Contains(usageDeep, "/deep") {
		t.Fatalf("usage=%q", usageDeep)
	}

	e.assistant.RatePerMinute = 1
	if out := e.command("/deep", "một"); out.code != "assistant_queued" {
		t.Fatalf("first=%+v", out)
	}
	if out := e.command("/deep", "hai"); out.code != "assistant_rate_limited" || e.msg.last(t).Text != msgAssistantBusy {
		t.Fatalf("second=%+v", out)
	}

	q := p6aEnv(&p6aModel{})
	q.assistant.RatePerMinute = 100
	for i := 0; i < assistantQueueSize; i++ {
		if out := q.command("/deep", "việc"); out.code != "assistant_queued" {
			t.Fatalf("fill %d=%+v", i, out)
		}
	}
	if out := q.command("/deep", "thêm"); out.code != "assistant_queue_full" || q.msg.last(t).Text != msgAssistantOverloaded {
		t.Fatalf("full=%+v", out)
	}

	off := p6aEnv(&p6aModel{})
	off.assistant.Models = nil
	if out := off.command("/deep", "x"); off.msg.last(t).Text != msgHint || out.code == "assistant_queued" {
		t.Fatalf("assistant off=%+v", out)
	}

	h := p6aEnv(&p6aModel{})
	h.command("/help", "")
	if !strings.Contains(h.msg.last(t).Text, "/deep") {
		t.Fatalf("/help=%q", h.msg.last(t).Text)
	}
}

// TC-6A44 (REQ-8): a behavior cannot take /deep (engine-reserved); a
// similar name is still allowed.
func TestPhase6aDeepIsReservedForBehaviors(t *testing.T) {
	doc := func(cmd string) Doc {
		return Doc{Schema: 1, Name: "d", Triggers: []TriggerDoc{{Type: TriggerTelegramCommand, Command: cmd}},
			Instructions: "x", Actions: map[string][]ActionDoc{string(ClassNotify): {{Type: ActionTelegramMessage}}}}
	}
	if errs := Validate(doc("/deeper"), nil, nil); len(errs) != 0 {
		t.Fatalf("/deeper rejected: %v", errs)
	}
	errs := Validate(doc("/deep"), nil, nil)
	found := false
	for _, fe := range errs {
		if strings.HasSuffix(fe.Path, ".command") && strings.Contains(fe.Msg, "built-in") {
			found = true
		}
	}
	if !found {
		t.Fatalf("/deep accepted for a behavior: %v", errs)
	}
}
