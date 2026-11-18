package behavior

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/orchestrator"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Phase 7b Fix Brief B2 (REQ-6, 7, 11, 13): /plan, the start_task tool,
// /help, reserved commands and the /status extras. Fakes only.

var b2Now = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

// ---- fakes ----

// b2Starter starts orchestrated tasks; a repeated update returns the same
// execution like the real store.
type b2Starter struct {
	mu    sync.Mutex
	calls int
	reqs  []orchestrator.TaskRequest
	ids   map[int64]int64 // update id -> execution id
	err   error
}

func (s *b2Starter) StartOrchestratedTask(_ context.Context, r orchestrator.TaskRequest) (workflow.Execution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return workflow.Execution{}, s.err
	}
	if s.ids == nil {
		s.ids = map[int64]int64{}
	}
	id, ok := s.ids[r.UpdateID]
	if !ok {
		id = 40 + int64(len(s.ids)) + 1
		s.ids[r.UpdateID] = id
	}
	s.reqs = append(s.reqs, r)
	return workflow.Execution{ID: id, WorkspaceID: r.WorkspaceID}, nil
}

func (s *b2Starter) started() []orchestrator.TaskRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]orchestrator.TaskRequest(nil), s.reqs...)
}

func (s *b2Starter) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// b2Repos is the registered-repo source; the error is optional.
type b2Repos struct {
	b3Starter
	repoErr error
}

func (r *b2Repos) RegisteredRepos(ctx context.Context, ws string) ([]string, error) {
	if r.repoErr != nil {
		return nil, r.repoErr
	}
	return r.b3Starter.RegisteredRepos(ctx, ws)
}

// b2Links resolves custom ids; resolveErr fails every resolution.
type b2Links struct {
	b3Links
	resolveErr error
}

func (l *b2Links) ResolveTask(ctx context.Context, ws string, ref TaskRef) (string, error) {
	if l.resolveErr != nil {
		return "", l.resolveErr
	}
	return l.b3Links.ResolveTask(ctx, ws, ref)
}

// b2Engine records Cancel only.
type b2Engine struct {
	workflow.Engine
	mu      sync.Mutex
	cancels []workflow.Command
}

func (e *b2Engine) Cancel(_ context.Context, c workflow.Command) (workflow.Execution, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cancels = append(e.cancels, c)
	return workflow.Execution{ID: c.ExecutionID}, nil
}

type b2Env struct {
	w     *InboxWorker
	m     *p4Messenger
	st    *b2Starter
	repos *b2Repos
	conv  *phase6BConversations
	links *b2Links
	eng   *b2Engine
}

func newB2Env(repos ...string) *b2Env {
	e := &b2Env{m: &p4Messenger{}, st: &b2Starter{}, repos: &b2Repos{b3Starter: b3Starter{repos: repos}},
		conv: &phase6BConversations{}, links: &b2Links{}, eng: &b2Engine{}}
	e.w = NewInboxWorker(InboxDeps{
		Messenger: e.m, Conversations: e.conv, LocalTasks: e.repos, OrchestratedTasks: e.st, Links: e.links, WorkflowCommands: e.eng,
		Topics: &b1Topics{rows: []Topic{b1TaskTopic(b1Task, b1Thread)}},
	}, InboxSettings{})
	e.w.now = func() time.Time { return b2Now }
	return e
}

func (e *b2Env) update(command, args string, thread, updateID int64) InboundUpdate {
	return InboundUpdate{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: b1Chat, ThreadID: thread, UpdateID: updateID, MessageID: 31,
		Kind: UpdateCommand, Command: command, Args: args}
}

func (e *b2Env) plan(args string, thread int64) outcome {
	return e.w.dispatch(context.Background(), e.update("/plan", args, thread, 9001), "corr")
}

func (e *b2Env) noText(t *testing.T) {
	t.Helper()
	for _, s := range e.m.texts() {
		for _, leak := range []string{"boom", "secret-token", "zebra-unicorn"} {
			if strings.Contains(s, leak) {
				t.Fatalf("reply leaks %q: %q", leak, s)
			}
		}
	}
}

// ---- /plan ----

// B2-P1 (REQ-6): /plan inside a task topic starts the orchestrated task for the topic's task, answers with the WF reference only, and stores the request as the first instruction.
func TestB2Plan_InTopicStartsTask(t *testing.T) {
	e := newB2Env("acme/api")
	out := e.plan("kiểm tra US và liệt kê OQ zebra-unicorn", b1Thread)
	if out.code != "" {
		t.Fatalf("code %q replies %v", out.code, e.m.texts())
	}
	reqs := e.st.started()
	if len(reqs) != 1 || reqs[0].ClickUpTaskID != b1Task || reqs[0].TaskRef != b1Task || reqs[0].Repo != "" ||
		reqs[0].UpdateID != 9001 || reqs[0].ThreadID != b1Thread || reqs[0].ChatID != b1Chat || reqs[0].IntegrationID != b1Bot {
		t.Fatalf("start request %+v", reqs)
	}
	got := e.m.texts()
	if len(got) != 1 || got[0] != "WF-41: đã nhận, đang lập kế hoạch…" {
		t.Fatalf("reply %v", got)
	}
	ins := e.conv.instructions[41]
	if len(ins) != 1 || ins[0].Text != "kiểm tra US và liệt kê OQ zebra-unicorn" || ins[0].TelegramUpdateID != 9001 || ins[0].ConversationID != 77 {
		t.Fatalf("instruction %+v", ins)
	}
	if len(e.conv.resources) != 1 || e.conv.resources[0].Kind != "telegram.message" || e.conv.resources[0].ExecutionID != 41 {
		t.Fatalf("message not bound: %+v", e.conv.resources)
	}
	if len(e.eng.cancels) != 0 {
		t.Fatal("a good start was cancelled")
	}
}

// B2-P2 (REQ-6): outside a topic /plan needs a task id; a custom id is resolved; a plain id needs no resolver.
func TestB2Plan_OutsideTopicNeedsAndResolvesTaskID(t *testing.T) {
	e := newB2Env()
	if out := e.plan("FC-6583 cập nhật tài liệu", 0); out.code != "" {
		t.Fatalf("code %q: %v", out.code, e.m.texts())
	}
	reqs := e.st.started()
	if len(reqs) != 1 || reqs[0].TaskRef != "FC-6583" || reqs[0].ClickUpTaskID != "86resolved" || reqs[0].ThreadID != 0 {
		t.Fatalf("custom id: %+v", reqs)
	}
	if len(e.links.calls) != 1 || e.links.calls[0] != "FC-6583" {
		t.Fatalf("resolver %v", e.links.calls)
	}
	if got := e.conv.instructions[41]; len(got) != 1 || got[0].Text != "cập nhật tài liệu" {
		t.Fatalf("instruction %+v", got)
	}
	e2 := newB2Env()
	e2.plan("86plain do it", 0)
	if r := e2.st.started(); len(r) != 1 || r[0].ClickUpTaskID != "86plain" || len(e2.links.calls) != 0 {
		t.Fatalf("plain id: %+v %v", r, e2.links.calls)
	}
	// A ClickUp link works like an id.
	e3 := newB2Env()
	e3.plan("https://app.clickup.com/t/9008218621/86link do it", 0)
	if r := e3.st.started(); len(r) != 1 || r[0].ClickUpTaskID != "86link" {
		t.Fatalf("link: %+v", r)
	}
}

// B2-P3 (REQ-6, human): no id outside a topic, a foreign link, a bare word, no text, or whitespace: usage, nothing started.
func TestB2Plan_UsageCases(t *testing.T) {
	for name, tc := range map[string]struct {
		args   string
		thread int64
		code   string
		text   string
	}{
		"empty outside topic":    {"", 0, "usage", usagePlan},
		"foreign link":           {"https://evil.example/t/86abc do it", 0, "usage", usagePlan},
		"bare word, no digit":    {"please investigate this", 0, "usage", usagePlan},
		"id without request":     {"FC-1", 0, "usage", msgTaskTooLong},
		"topic, empty request":   {"", b1Thread, "usage", msgTaskTooLong},
		"topic, spaces only":     {"    ", b1Thread, "usage", msgTaskTooLong},
		"topic, control only":    {"\x07\x00", b1Thread, "usage", msgTaskTooLong},
		"request over 2000":      {strings.Repeat("é", 2001), b1Thread, "usage", msgTaskTooLong},
		"id and request over 2k": {"FC-1 " + strings.Repeat("x", 2001), 0, "usage", msgTaskTooLong},
	} {
		e := newB2Env("acme/api")
		out := e.plan(tc.args, tc.thread)
		if out.code != tc.code || len(e.st.started()) != 0 {
			t.Errorf("%s: code %q started %v", name, out.code, e.st.started())
			continue
		}
		if got := e.m.texts(); len(got) != 1 || got[0] != tc.text {
			t.Errorf("%s: reply %v", name, got)
		}
	}
	if !strings.Contains(usagePlan, "/plan") {
		t.Fatal("usage does not name the command")
	}
}

// B2-P4 (REQ-6): request bounds and cleaning.
func TestB2Plan_RequestBoundsAndCleaning(t *testing.T) {
	e := newB2Env()
	if out := e.plan(strings.Repeat("é", 2000), b1Thread); out.code != "" || len(e.st.started()) != 1 {
		t.Fatalf("2000 runes refused: %q", out.code)
	}
	e2 := newB2Env()
	e2.plan("a\x07b\x00c", b1Thread)
	if got := e2.conv.instructions[41]; len(got) != 1 || got[0].Text != "abc" {
		t.Fatalf("control characters kept: %+v", got)
	}
}

// B2-P5 (REQ-6): a registered owner/repo first word (any case) is the repo and not part of the request.
func TestB2Plan_ExplicitRegisteredRepo(t *testing.T) {
	e := newB2Env("acme/api", "acme/web")
	if out := e.plan("ACME/Web add a test", b1Thread); out.code != "" {
		t.Fatalf("code %q: %v", out.code, e.m.texts())
	}
	reqs := e.st.started()
	if len(reqs) != 1 || reqs[0].Repo != "acme/web" {
		t.Fatalf("repo: %+v", reqs)
	}
	if got := e.conv.instructions[41]; len(got) != 1 || got[0].Text != "add a test" {
		t.Fatalf("repo word in the instruction: %+v", got)
	}
	// With a task id first, then the repo.
	e2 := newB2Env("acme/web")
	e2.plan("FC-9 acme/web fix it", 0)
	if r := e2.st.started(); len(r) != 1 || r[0].Repo != "acme/web" || r[0].TaskRef != "FC-9" {
		t.Fatalf("id then repo: %+v", r)
	}
	// No repo word: the request keeps its first word and no repo is chosen (the hub resolves it later).
	e3 := newB2Env("acme/web")
	e3.plan("kiểm tra repo", b1Thread)
	if r := e3.st.started(); len(r) != 1 || r[0].Repo != "" {
		t.Fatalf("no repo word: %+v", r)
	}
	if got := e3.conv.instructions[41]; len(got) != 1 || got[0].Text != "kiểm tra repo" {
		t.Fatalf("first word lost: %+v", got)
	}
}

// R-B-2 / B2-P6 (REQ-6, human): an owner/repo first word that is not registered is refused with the repo message; the plan never falls through to another repo and nothing is started.
func TestB2Plan_UnregisteredRepoWordIsRefused(t *testing.T) {
	for name, repos := range map[string][]string{
		"one other repo registered": {"acme/api"},
		"several registered":        {"acme/api", "acme/web"},
		"none registered":           nil,
	} {
		e := newB2Env(repos...)
		out := e.plan("acme/unknown sửa validate", b1Thread)
		if out.code != "no_repo" || e.st.callCount() != 0 || len(e.conv.instructions) != 0 {
			t.Errorf("%s: code %q starter calls %d", name, out.code, e.st.callCount())
			continue
		}
		if got := e.m.texts(); len(got) != 1 || got[0] != msgPlanRepoUnknown {
			t.Errorf("%s: reply %v", name, got)
		}
	}
	// No repo source wired at all: still the repo message, never a start.
	e := newB2Env("acme/api")
	e.w.d.LocalTasks = nil
	if out := e.plan("acme/api fix", b1Thread); out.code != "no_repo" || e.st.callCount() != 0 {
		t.Fatalf("without a repo source: %q calls %d", out.code, e.st.callCount())
	}
}

// R-B-2 / B2-P7 (REQ-6): a registered-repos read error is the not-ready reply, nothing is started, no error text leaks.
func TestB2Plan_RegisteredReposReadErrorIsNotReady(t *testing.T) {
	e := newB2Env("acme/api")
	e.repos.repoErr = errors.New("boom secret-token")
	out := e.plan("acme/api sửa validate", b1Thread)
	if out.code != "handler_failed" || e.st.callCount() != 0 {
		t.Fatalf("code %q calls %d", out.code, e.st.callCount())
	}
	if got := e.m.texts(); len(got) != 1 || got[0] != msgPlanNotReady {
		t.Fatalf("reply %v", got)
	}
	e.noText(t)
	// A request whose first word is not owner/repo never reads the repos, so the error does not matter.
	e2 := newB2Env("acme/api")
	e2.repos.repoErr = errors.New("boom")
	if out := e2.plan("sửa validate", b1Thread); out.code != "" || e2.st.callCount() != 1 {
		t.Fatalf("plain request: %q calls %d", out.code, e2.st.callCount())
	}
}

// B2-P8 (REQ-6): not enabled, or the workspace is not on the durable runtime.
func TestB2Plan_NotEnabled(t *testing.T) {
	e := newB2Env("acme/api")
	e.w.d.OrchestratedTasks = nil
	if out := e.plan("do it", b1Thread); out.code != "not_available" {
		t.Fatalf("code %q", out.code)
	}
	if got := e.m.texts(); len(got) != 1 || got[0] != msgPlanNotEnabled {
		t.Fatalf("reply %v", got)
	}
	e2 := newB2Env()
	e2.st.err = ErrNotAvailable
	if out := e2.plan("do it", b1Thread); out.code != "not_available" || e2.m.last(t).Text != msgPlanNotEnabled {
		t.Fatalf("store not available: %q %q", out.code, e2.m.last(t).Text)
	}
	if len(e2.conv.instructions) != 0 {
		t.Fatal("instruction stored without an execution")
	}
}

// B2-P9 (REQ-6, human): the task cannot be read (404, wrong team): the unreadable answer, nothing started.
func TestB2Plan_UnreadableTask(t *testing.T) {
	e := newB2Env()
	e.links.resolveErr = errors.New("404 boom")
	out := e.plan("FC-1 do it", 0)
	if out.code != "task_unreadable" || e.st.callCount() != 0 {
		t.Fatalf("code %q calls %d", out.code, e.st.callCount())
	}
	if got := e.m.texts(); len(got) != 1 || got[0] != msgTopicUnread {
		t.Fatalf("reply %v", got)
	}
	e.noText(t)
}

// B2-P10 (REQ-6): when the instruction cannot be stored the execution is cancelled with a deterministic key and the owner gets the not-ready reply.
func TestB2Plan_AppendInstructionFailureCancels(t *testing.T) {
	e := newB2Env()
	e.conv.appendErr = errors.New("boom")
	out := e.plan("do it zebra-unicorn", b1Thread)
	if out.code != "handler_failed" {
		t.Fatalf("code %q", out.code)
	}
	if got := e.m.texts(); len(got) != 1 || got[0] != msgPlanNotReady {
		t.Fatalf("reply %v", got)
	}
	e.noText(t)
	if len(e.eng.cancels) != 1 || e.eng.cancels[0].ExecutionID != 41 || e.eng.cancels[0].WorkspaceID != p4WS ||
		e.eng.cancels[0].IdempotencyKey != "telegram:plan-abort:"+p4WS+":41" {
		t.Fatalf("cancels %+v", e.eng.cancels)
	}
	if len(e.conv.resources) != 0 {
		t.Fatalf("message bound to a cancelled execution: %+v", e.conv.resources)
	}
	// Without an engine the reply is the same and nothing panics.
	e2 := newB2Env()
	e2.conv.appendErr = errors.New("boom")
	e2.w.d.WorkflowCommands = nil
	if out := e2.plan("do it", b1Thread); out.code != "handler_failed" {
		t.Fatalf("no engine: %q", out.code)
	}
}

// B2-P11 (REQ-6, human): the store failing is the not-ready reply without its text; no conversation also fails before any start.
func TestB2Plan_StoreAndConversationFailures(t *testing.T) {
	e := newB2Env()
	e.st.err = errors.New("boom secret-token")
	if out := e.plan("do it", b1Thread); out.code != "handler_failed" || e.m.last(t).Text != msgPlanNotReady {
		t.Fatalf("store error: %q %q", out.code, e.m.last(t).Text)
	}
	e.noText(t)
	e2 := newB2Env()
	e2.w.d.Conversations = nil
	if out := e2.plan("do it", b1Thread); out.code != "handler_failed" || e2.st.callCount() != 0 {
		t.Fatalf("no conversations: %q calls %d", out.code, e2.st.callCount())
	}
}

// B2-P12 (REQ-6, human): the same update delivered twice starts one execution and stores one instruction; both replies name the same WF.
func TestB2Plan_RedeliveryIsIdempotentPerUpdate(t *testing.T) {
	e := newB2Env()
	u := e.update("/plan", "do it once", b1Thread, 9001)
	e.w.dispatch(context.Background(), u, "c1")
	e.w.dispatch(context.Background(), u, "c2")
	texts := e.m.texts()
	if len(texts) != 2 || texts[0] != texts[1] || !strings.HasPrefix(texts[0], "WF-41: ") {
		t.Fatalf("replies %v", texts)
	}
	for _, r := range e.st.started() {
		if r.UpdateID != 9001 || r.TriggerKey() != e.st.started()[0].TriggerKey() {
			t.Fatalf("request %+v", r)
		}
	}
	if got := e.conv.instructions[41]; len(got) != 1 {
		t.Fatalf("instructions %+v", got)
	}
	// A different update is a different task.
	e.w.dispatch(context.Background(), e.update("/plan", "do it twice", b1Thread, 9002), "c3")
	if len(e.conv.instructions[42]) != 1 {
		t.Fatalf("second update: %+v", e.conv.instructions)
	}
}

// B2-P13 (REQ-6): /plan is its own command; it never reaches the behavior command trigger.
func TestB2Plan_DoesNotReachBehaviorCommands(t *testing.T) {
	e := newB2Env()
	e.w.d.Commands = nil // a behavior dispatch would panic on a nil handler
	if out := e.plan("do it", b1Thread); out.code != "" {
		t.Fatalf("code %q", out.code)
	}
}

// ---- reserved commands, /help ----

// B2-R1 (REQ-6): /plan, /task and /retry are built-in: a behavior cannot take them, a similar name is fine.
func TestB2ReservedCommands(t *testing.T) {
	for _, c := range []string{"/plan", "/task", "/retry", "/topic", "/deep", "/help", "/start", "/trace", "/undo"} {
		if !reservedCommands[c] {
			t.Errorf("%s is not reserved", c)
		}
	}
	doc := func(cmd string) Doc {
		return Doc{Schema: 1, Name: "d", Triggers: []TriggerDoc{{Type: TriggerTelegramCommand, Command: cmd}},
			Instructions: "x", Actions: map[string][]ActionDoc{string(ClassNotify): {{Type: ActionTelegramMessage}}}}
	}
	for _, ok := range []string{"/planner", "/plans", "/plan2"} {
		if errs := Validate(doc(ok), nil, nil); len(errs) != 0 {
			t.Errorf("%s rejected: %v", ok, errs)
		}
	}
	errs := Validate(doc("/plan"), nil, nil)
	found := false
	for _, fe := range errs {
		if strings.HasSuffix(fe.Path, ".command") && strings.Contains(fe.Msg, "built-in") {
			found = true
		}
	}
	if !found {
		t.Fatalf("/plan accepted for a behavior: %v", errs)
	}
}

// B2-R2 (REQ-6): /help lists /plan with its syntax next to /task.
func TestB2Help_ListsPlan(t *testing.T) {
	e := newB2Env()
	help := e.w.helpText(context.Background(), p4WS)
	if !strings.Contains(help, "/plan [TASK-ID] <yêu cầu>") || !strings.Contains(help, "/task [TASK-ID] <yêu cầu>") {
		t.Fatalf("help=%q", help)
	}
	if out := e.w.dispatch(context.Background(), e.update("/help", "", 0, 5), "c"); out.code != "" || !strings.Contains(e.m.last(t).Text, "/plan") {
		t.Fatalf("/help: %+v %q", out, e.m.last(t).Text)
	}
}

// ---- start_task tool ----

func (e *b2Env) turn(thread int64) *assistantToolTurn {
	u := e.update("", "", thread, 7001)
	u.Kind = UpdateText
	return &assistantToolTurn{u: u, to: ChatRef{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: b1Chat, ThreadID: thread}, actor: "owner", corr: "corr", proposed: map[string]bool{}}
}

func (e *b2Env) startTool(tt *assistantToolTurn, in map[string]any) (map[string]any, string, bool) {
	text, isErr := e.w.runAssistantTool(context.Background(), tt, h1Tool(toolStartTaskName, in))
	var m map[string]any
	if !isErr {
		_ = json.Unmarshal([]byte(text), &m)
	}
	return m, text, isErr
}

// B2-T1 (REQ-11): start_task is offered only when the orchestrator and the instruction store are wired.
func TestB2StartTask_Availability(t *testing.T) {
	e := newB2Env()
	u := e.update("", "", 0, 1)
	if !h1Names(e.w.assistantToolDefsFor(u))[toolStartTaskName] {
		t.Fatal("start_task not offered when wired")
	}
	e.w.d.OrchestratedTasks = nil
	if h1Names(e.w.assistantToolDefsFor(u))[toolStartTaskName] {
		t.Fatal("offered without the orchestrator")
	}
	if m, text, isErr := e.startTool(e.turn(b1Thread), map[string]any{"request": "do it"}); isErr || m["status"] != "not_available" {
		t.Fatalf("call without orchestrator: %q %v", text, isErr)
	}
	e2 := newB2Env()
	e2.w.d.Conversations = nil
	if h1Names(e2.w.assistantToolDefsFor(u))[toolStartTaskName] {
		t.Fatal("offered without the instruction store")
	}
	if e2.st.callCount() != 0 {
		t.Fatal("started without availability")
	}
}

// B2-T2 (REQ-11): in a topic the tool starts the plan; the model sees status and WF id only; a second call in the same turn is already_started.
func TestB2StartTask_StartsInTopicAndOncePerTurn(t *testing.T) {
	e := newB2Env("acme/api")
	tt := e.turn(b1Thread)
	m, text, isErr := e.startTool(tt, map[string]any{"request": "kiểm tra US zebra-unicorn"})
	if isErr || m["status"] != "started" || m["workflow"] != "WF-41" || len(m) != 2 {
		t.Fatalf("result %q", text)
	}
	if strings.Contains(text, "zebra-unicorn") || strings.Contains(text, b1Task) {
		t.Fatalf("result leaks request or task: %q", text)
	}
	reqs := e.st.started()
	if len(reqs) != 1 || reqs[0].ClickUpTaskID != b1Task || reqs[0].UpdateID != 7001 || reqs[0].Repo != "" {
		t.Fatalf("request %+v", reqs)
	}
	if got := e.conv.instructions[41]; len(got) != 1 || got[0].Text != "kiểm tra US zebra-unicorn" {
		t.Fatalf("instruction %+v", got)
	}
	m, _, isErr = e.startTool(tt, map[string]any{"request": "another one"})
	if isErr || m["status"] != "already_started" || e.st.callCount() != 1 {
		t.Fatalf("second call: %v calls %d", m, e.st.callCount())
	}
	// A request at the tool's argument limit (200 runes) works.
	e2 := newB2Env()
	if m, text, isErr := e2.startTool(e2.turn(b1Thread), map[string]any{"request": strings.Repeat("é", 200)}); isErr || m["status"] != "started" {
		t.Fatalf("200 runes: %q", text)
	}
}

// B2-T3 (REQ-11): outside a topic without a task: task_required; a task argument (id, custom id, link) works; a malformed one is a tool error.
func TestB2StartTask_TaskRequiredAndTaskArgument(t *testing.T) {
	e := newB2Env()
	m, text, isErr := e.startTool(e.turn(0), map[string]any{"request": "do it"})
	if isErr || m["status"] != "task_required" || e.st.callCount() != 0 {
		t.Fatalf("no task: %q calls %d", text, e.st.callCount())
	}
	m, text, isErr = e.startTool(e.turn(0), map[string]any{"request": "do it", "task": "FC-6583"})
	if isErr || m["status"] != "started" {
		t.Fatalf("custom id: %q", text)
	}
	if r := e.st.started(); len(r) != 1 || r[0].ClickUpTaskID != "86resolved" || r[0].TaskRef != "FC-6583" {
		t.Fatalf("custom id request: %+v", r)
	}
	e2 := newB2Env()
	e2.startTool(e2.turn(0), map[string]any{"request": "do it", "task": "https://app.clickup.com/t/9008218621/86link"})
	if r := e2.st.started(); len(r) != 1 || r[0].ClickUpTaskID != "86link" {
		t.Fatalf("link: %+v", r)
	}
	for _, bad := range []string{"https://evil.example/t/1/86abc", "FC 1", "../x", "FC-1?x=y"} {
		e3 := newB2Env()
		if _, text, isErr := e3.startTool(e3.turn(0), map[string]any{"request": "do it", "task": bad}); !isErr || e3.st.callCount() != 0 {
			t.Errorf("task %q: err=%v %q calls %d", bad, isErr, text, e3.st.callCount())
		}
	}
	// The task argument beats the topic's task.
	e4 := newB2Env()
	e4.startTool(e4.turn(b1Thread), map[string]any{"request": "do it", "task": "86other"})
	if r := e4.st.started(); len(r) != 1 || r[0].ClickUpTaskID != "86other" {
		t.Fatalf("explicit task in topic: %+v", r)
	}
}

// B2-T4 (REQ-11): repo_not_registered for an unregistered, malformed or unreadable repo; a registered repo (any case) is passed on.
func TestB2StartTask_RepoNotRegistered(t *testing.T) {
	e := newB2Env("acme/api")
	for _, repo := range []string{"acme/unknown", "../etc/passwd", "noslash", "acme/api/extra"} {
		m, text, isErr := e.startTool(e.turn(b1Thread), map[string]any{"request": "do it", "repo": repo})
		if isErr || m["status"] != "repo_not_registered" {
			t.Errorf("repo %q: %q", repo, text)
		}
	}
	if e.st.callCount() != 0 {
		t.Fatal("started with an unregistered repo")
	}
	e.repos.repoErr = errors.New("boom")
	if m, _, _ := e.startTool(e.turn(b1Thread), map[string]any{"request": "do it", "repo": "acme/api"}); m["status"] != "repo_not_registered" || e.st.callCount() != 0 {
		t.Fatalf("repo read error: %v", m)
	}
	e.repos.repoErr = nil
	if m, text, _ := e.startTool(e.turn(b1Thread), map[string]any{"request": "do it", "repo": "ACME/API"}); m["status"] != "started" {
		t.Fatalf("registered repo: %q", text)
	}
	if r := e.st.started(); len(r) != 1 || r[0].Repo != "acme/api" {
		t.Fatalf("repo not canonical: %+v", r)
	}
	// With no repo source nothing can be registered.
	e2 := newB2Env()
	e2.w.d.LocalTasks = nil
	if m, _, _ := e2.startTool(e2.turn(b1Thread), map[string]any{"request": "do it", "repo": "acme/api"}); m["status"] != "repo_not_registered" {
		t.Fatalf("no repo source: %v", m)
	}
}

// B2-T5 (REQ-11): the status codes of the shared start path reach the model as codes only.
func TestB2StartTask_StatusCodes(t *testing.T) {
	e := newB2Env()
	m, _, _ := e.startTool(e.turn(b1Thread), map[string]any{"request": "\x07\x00"})
	if m["status"] != "usage" || e.st.callCount() != 0 {
		t.Fatalf("empty after cleaning: %v", m)
	}
	e2 := newB2Env()
	e2.links.resolveErr = errors.New("boom")
	if m, text, _ := e2.startTool(e2.turn(0), map[string]any{"request": "do it", "task": "FC-1"}); m["status"] != "task_unreadable" || strings.Contains(text, "boom") {
		t.Fatalf("unreadable: %q", text)
	}
	e3 := newB2Env()
	e3.st.err = ErrNotAvailable
	if m, _, _ := e3.startTool(e3.turn(b1Thread), map[string]any{"request": "do it"}); m["status"] != "not_available" {
		t.Fatalf("not available: %v", m)
	}
	e4 := newB2Env()
	e4.st.err = errors.New("boom secret-token")
	if m, text, _ := e4.startTool(e4.turn(b1Thread), map[string]any{"request": "do it"}); m["status"] != "failed" || strings.Contains(text, "boom") {
		t.Fatalf("store error: %q", text)
	}
	// A failed start does not use up the once-per-turn slot.
	e5 := newB2Env()
	tt := e5.turn(b1Thread)
	e5.conv.appendErr = errors.New("boom")
	if m, _, _ := e5.startTool(tt, map[string]any{"request": "do it"}); m["status"] != "failed" || len(e5.eng.cancels) != 1 {
		t.Fatalf("append error: %v cancels %d", m, len(e5.eng.cancels))
	}
	e5.conv.appendErr = nil
	if m, _, _ := e5.startTool(tt, map[string]any{"request": "do it again"}); m["status"] != "started" {
		t.Fatalf("retry in the same turn after a failure: %v", m)
	}
	if m, _, _ := e5.startTool(tt, map[string]any{"request": "once more"}); m["status"] != "already_started" {
		t.Fatalf("after success: %v", m)
	}
}

// B2-T6 (REQ-11, abuse): a missing or non-text request is a tool error and starts nothing.
func TestB2StartTask_BadArguments(t *testing.T) {
	for name, in := range map[string]map[string]any{
		"no request":     {"task": "FC-1"},
		"empty request":  {"request": "   "},
		"number request": {"request": []any{"x"}},
		"object task":    {"request": "x", "task": map[string]any{"a": 1}},
		"list repo":      {"request": "x", "repo": []any{"a/b"}},
	} {
		e := newB2Env("acme/api")
		if _, text, isErr := e.startTool(e.turn(b1Thread), in); !isErr || e.st.callCount() != 0 {
			t.Errorf("%s: err=%v %q calls %d", name, isErr, text, e.st.callCount())
		}
	}
	e := newB2Env()
	if text, isErr := e.w.runAssistantTool(context.Background(), e.turn(b1Thread), domain.ToolCall{ID: "c", Name: toolStartTaskName, Input: json.RawMessage(`[1]`)}); !isErr || e.st.callCount() != 0 {
		t.Fatalf("non-object input: %q", text)
	}
}

// ---- /status extras ----

type b2Reader struct {
	phase6BReader
	steps   []workflow.Step
	stepErr error
}

func (r *b2Reader) ListSteps(context.Context, string, int64) ([]workflow.Step, error) {
	return r.steps, r.stepErr
}

type b2Plans struct {
	plans              []orchestrator.PlanRecord
	tasks              []orchestrator.PlanTaskRecord
	decisions          []orchestrator.DecisionRecord
	plansErr, tasksErr error
	decisionsErr       error
	calls              int
}

func (p *b2Plans) ListPlans(context.Context, string, int64) ([]orchestrator.PlanRecord, error) {
	p.calls++
	return p.plans, p.plansErr
}
func (p *b2Plans) ListTasks(context.Context, string, int64) ([]orchestrator.PlanTaskRecord, error) {
	p.calls++
	return p.tasks, p.tasksErr
}
func (p *b2Plans) ListDecisions(context.Context, string, int64) ([]orchestrator.DecisionRecord, error) {
	p.calls++
	return p.decisions, p.decisionsErr
}

func b2Snapshot(position int, used orchestrator.BudgetUsed, started time.Time) workflow.Step {
	snap := orchestrator.BudgetSnapshot{Caps: orchestrator.BudgetCaps{PlanIterations: 6, Replans: 3, RepairAttempts: 4, Tokens: 200000, Wall: 90 * time.Minute},
		Used: used, StartedAt: started}
	raw, _ := json.Marshal(snap)
	return workflow.Step{Position: position, Checkpoint: &workflow.Checkpoint{References: []workflow.CheckpointReference{
		{Kind: orchestrator.CheckpointBudget, ReferenceID: base64.RawURLEncoding.EncodeToString(raw)}}}}
}

type b2Status struct {
	w      *InboxWorker
	m      *p4Messenger
	reader *b2Reader
	plans  *b2Plans
}

func newB2Status(source string, state workflow.State) *b2Status {
	d := phase6BDetail(42, p4WS, state)
	d.References.SourceKind = source
	r := &b2Reader{phase6BReader: phase6BReader{details: map[int64]workflow.ExecutionDetail{42: d}}}
	m := &p4Messenger{}
	p := &b2Plans{}
	w := NewInboxWorker(InboxDeps{Workflows: r, Conversations: &phase6BConversations{}, Messenger: m, OrchestratorPlans: p}, InboxSettings{})
	w.now = func() time.Time { return b2Now }
	return &b2Status{w: w, m: m, reader: r, plans: p}
}

func (s *b2Status) status(t *testing.T) string {
	t.Helper()
	s.w.workflowStatus(context.Background(), InboundUpdate{WorkspaceID: p4WS}, ChatRef{}, 42)
	return s.m.last(t).Text
}

func b2Task(plan int64, key, title, executor, state string) orchestrator.PlanTaskRecord {
	return orchestrator.PlanTaskRecord{PlanID: plan, Key: key, Title: title, Executor: executor, State: state}
}

// B2-S1 (REQ-14): /status of an orchestrated task shows the newest plan, its tasks (key, title, executor, state), the last decision and the budget.
func TestB2Status_PlanTasksDecisionBudget(t *testing.T) {
	s := newB2Status(orchestrator.WorkflowSourceOrchestratedTask, workflow.StateRunning)
	s.plans.plans = []orchestrator.PlanRecord{{ID: 1, Version: 1, Status: "superseded"}, {ID: 2, Version: 2, Status: "active"}}
	s.plans.tasks = []orchestrator.PlanTaskRecord{
		b2Task(1, "old", "Old task", "investigate", "SUCCEEDED"),
		b2Task(2, "t1", "Đọc US", "investigate", "SUCCEEDED"),
		b2Task(2, "t2", "Sửa validate", "repo.write", "RUNNING"),
		b2Task(2, "t3", "Chưa có executor", "", "PENDING"),
	}
	s.plans.decisions = []orchestrator.DecisionRecord{
		{ID: 3, Decision: "REPLAN", PlanVersion: 1},
		{ID: 9, Decision: "COMPLETE", ReasonCode: "missing_evidence", TaskKey: "t2", ConvertedFrom: "COMPLETE"},
		{ID: 5, Decision: "FAIL", PlanVersion: 2},
	}
	s.reader.steps = []workflow.Step{
		b2Snapshot(1, orchestrator.BudgetUsed{PlanIterations: 1, Tokens: 100}, b2Now.Add(-30*time.Minute)),
		b2Snapshot(4, orchestrator.BudgetUsed{PlanIterations: 2, Replans: 1, RepairAttempts: 3, Tokens: 4500}, b2Now.Add(-30*time.Minute)),
		{Position: 7}, // a later step without a snapshot does not hide the newest one
	}
	text := s.status(t)
	for _, want := range []string{
		"WF-42 · review · RUNNING",
		"Kế hoạch: v2 (active) · 3 việc",
		"- t1 · Đọc US · investigate · SUCCEEDED",
		"- t2 · Sửa validate · repo.write · RUNNING",
		"- t3 · Chưa có executor · - · PENDING",
		"Quyết định cuối: COMPLETE (missing_evidence) · t2 · converted_from COMPLETE",
		"Ngân sách: lập kế hoạch 2/6 · lập lại 1/3 · sửa 3/4 · token 4500/200000 · thời gian 30/90 phút",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("status lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Old task") || strings.Contains(text, "FAIL") {
		t.Fatalf("old plan task or a lower decision id shown:\n%s", text)
	}
}

// B2-S2 (REQ-14): at most 30 task lines, then the count of the rest.
func TestB2Status_MaxThirtyTasks(t *testing.T) {
	s := newB2Status(orchestrator.WorkflowSourceOrchestratedTask, workflow.StateRunning)
	s.plans.plans = []orchestrator.PlanRecord{{ID: 1, Version: 1, Status: "active"}}
	for i := 1; i <= 35; i++ {
		s.plans.tasks = append(s.plans.tasks, b2Task(1, fmt.Sprintf("t%d", i), "T", "investigate", "PENDING"))
	}
	text := s.status(t)
	if got := strings.Count(text, "\n- t"); got != 30 {
		t.Fatalf("task lines %d", got)
	}
	if !strings.Contains(text, "Kế hoạch: v1 (active) · 35 việc") || !strings.Contains(text, "… và 5 việc nữa") || strings.Contains(text, "- t31 ") {
		t.Fatalf("status:\n%s", text)
	}
}

// B2-S3 (REQ-14, abuse): only metadata is shown: no task result, evidence, instruction text; titles are one line and bounded.
func TestB2Status_MetadataOnly(t *testing.T) {
	s := newB2Status(orchestrator.WorkflowSourceOrchestratedTask, workflow.StateRunning)
	s.plans.plans = []orchestrator.PlanRecord{{ID: 1, Version: 1, Status: "active", Intent: "secret-intent"}}
	long := "line one\nline two " + strings.Repeat("x", 200)
	s.plans.tasks = []orchestrator.PlanTaskRecord{{PlanID: 1, Key: "t1", Title: long, Executor: "investigate", State: "SUCCEEDED",
		Result: json.RawMessage(`{"summary":"secret-result"}`), EvidenceRefs: []string{"secret-evidence"}}}
	s.plans.decisions = []orchestrator.DecisionRecord{{ID: 1, Decision: "COMPLETE", EvidenceRefs: []string{"secret-decision-ref"}, ReasonCode: strings.Repeat("r", 200)}}
	s.w.d.Conversations = &phase6BConversations{instructions: map[int64][]workflow.Instruction{42: {{WorkspaceID: p4WS, ExecutionID: 42, Version: 1, Text: "secret-instruction", CreatedAt: b2Now}}}}
	text := s.status(t)
	for _, secret := range []string{"secret-result", "secret-evidence", "secret-decision-ref", "secret-instruction", "secret-intent"} {
		if strings.Contains(text, secret) {
			t.Errorf("status leaks %q:\n%s", secret, text)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "- t1") && (strings.Contains(line, "\n") || len([]rune(line)) > 40+60+60) {
			t.Errorf("task line not bounded: %q", line)
		}
		if strings.HasPrefix(line, "Quyết định cuối") && len([]rune(line)) > 40+60+40+40 {
			t.Errorf("decision line not bounded: %q", line)
		}
	}
	if strings.Contains(text, "line two") && !strings.Contains(text, "line one line two") {
		t.Fatalf("title not collapsed to one line:\n%s", text)
	}
}

// B2-S4 (REQ-14): read errors are silent: the base status is still sent, and no error text is shown.
func TestB2Status_ReadErrorsAreSilent(t *testing.T) {
	setup := func() *b2Status {
		s := newB2Status(orchestrator.WorkflowSourceOrchestratedTask, workflow.StateRunning)
		s.plans.plans = []orchestrator.PlanRecord{{ID: 1, Version: 1, Status: "active"}}
		s.plans.tasks = []orchestrator.PlanTaskRecord{b2Task(1, "t1", "Task", "investigate", "PENDING")}
		s.plans.decisions = []orchestrator.DecisionRecord{{ID: 1, Decision: "COMPLETE"}}
		s.reader.steps = []workflow.Step{b2Snapshot(1, orchestrator.BudgetUsed{}, b2Now)}
		return s
	}
	s := setup()
	s.plans.plansErr = errors.New("boom")
	text := s.status(t)
	if !strings.HasPrefix(text, "WF-42 · review · RUNNING") || strings.Contains(text, "Kế hoạch") || strings.Contains(text, "Ngân sách") || strings.Contains(text, "boom") {
		t.Fatalf("plans error:\n%s", text)
	}
	s = setup()
	s.plans.tasksErr = errors.New("boom")
	if text := s.status(t); !strings.HasPrefix(text, "WF-42 · review · RUNNING") || strings.Contains(text, "Kế hoạch") || strings.Contains(text, "boom") {
		t.Fatalf("tasks error:\n%s", text)
	}
	s = setup()
	s.plans.decisionsErr = errors.New("boom")
	if text := s.status(t); !strings.Contains(text, "Kế hoạch: v1") || strings.Contains(text, "Quyết định cuối") || !strings.Contains(text, "Ngân sách") {
		t.Fatalf("decisions error:\n%s", text)
	}
	s = setup()
	s.reader.stepErr = errors.New("boom")
	if text := s.status(t); !strings.Contains(text, "Kế hoạch: v1") || strings.Contains(text, "Ngân sách") || strings.Contains(text, "boom") {
		t.Fatalf("steps error:\n%s", text)
	}
}

// B2-S5 (REQ-14): other executions and a hub without plan reading get no extra lines and no plan reads.
func TestB2Status_OnlyForOrchestratedTasks(t *testing.T) {
	s := newB2Status("clickup.comment", workflow.StateRunning)
	s.plans.plans = []orchestrator.PlanRecord{{ID: 1, Version: 1, Status: "active"}}
	text := s.status(t)
	if strings.Contains(text, "Kế hoạch") || s.plans.calls != 0 {
		t.Fatalf("other execution got plan lines (reads %d):\n%s", s.plans.calls, text)
	}
	s2 := newB2Status(orchestrator.WorkflowSourceOrchestratedTask, workflow.StateRunning)
	s2.w.d.OrchestratorPlans = nil
	if text := s2.status(t); strings.Contains(text, "Kế hoạch") || !strings.HasPrefix(text, "WF-42") {
		t.Fatalf("without plan reader:\n%s", text)
	}
}

// B2-S6 (REQ-14): no plan yet says so; no budget snapshot adds no budget line; a finished execution measures wall time to its end.
func TestB2Status_NoPlanNoBudgetAndFinishedWall(t *testing.T) {
	s := newB2Status(orchestrator.WorkflowSourceOrchestratedTask, workflow.StateRunning)
	text := s.status(t)
	if !strings.Contains(text, "Kế hoạch: chưa có") || strings.Contains(text, "Ngân sách") || strings.Contains(text, "Quyết định") {
		t.Fatalf("empty:\n%s", text)
	}
	s2 := newB2Status(orchestrator.WorkflowSourceOrchestratedTask, workflow.StateCompleted)
	finished := b2Now.Add(-10 * time.Minute)
	d := s2.reader.details[42]
	d.FinishedAt = &finished
	s2.reader.details[42] = d
	s2.reader.steps = []workflow.Step{b2Snapshot(1, orchestrator.BudgetUsed{Tokens: 10}, b2Now.Add(-25*time.Minute))}
	if text := s2.status(t); !strings.Contains(text, "thời gian 15/90 phút") {
		t.Fatalf("finished wall time:\n%s", text)
	}
}
