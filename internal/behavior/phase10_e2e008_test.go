package behavior

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/orchestrator"
)

// Phase 10 Batch B, REQ-13: E2E-008. A ClickUp task with a Telegram topic, a
// /plan that completes, the final report with "📤 Đăng lên ClickUp", the
// owner's press and "Đăng": exactly one comment on the right task with the PR
// link, the summary in the topic, and nothing anywhere else. Fakes only:
// the real orchestrator owner, the real report source and button handler, the
// real draft service; the ClickUp tool caller, the messenger, the topics and
// the stores are fakes.

// p10XTopics gives each ClickUp task its topic thread.
type p10XTopics struct {
	mu     sync.Mutex
	byTask map[string]int64
	asked  []string
}

func (t *p10XTopics) Destination(_ context.Context, base ChatRef, in TopicIntent) ChatRef {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.asked = append(t.asked, in.TaskID)
	base.ThreadID = t.byTask[in.TaskID]
	return base
}
func (t *p10XTopics) Recover(context.Context, ChatRef, error) (ChatRef, bool) {
	return ChatRef{}, false
}
func (t *p10XTopics) NoteSent(ChatRef) {}

const (
	e2ePR      = "https://github.com/acme/api/pull/9"
	e2eSummary = "Đã xong phần API: thêm endpoint /v2/orders và test"
)

type e2eEnv struct {
	*p10REnv
	owner  *OrchestratorOwner
	topics *p10XTopics
}

func newE2EEnv(t *testing.T) *e2eEnv {
	t.Helper()
	r := newP10REnv(t)
	topics := &p10XTopics{byTask: map[string]int64{p10Task: p10Thread}}
	o := NewOrchestratorOwner(r.x, topics, nil)
	o.ReportButton = true
	return &e2eEnv{p10REnv: r, owner: o, topics: topics}
}

// finish registers a completed /plan of a task and sends its final report the
// way the orchestrator does; it returns the report message id and its button.
func (e *e2eEnv) finish(t *testing.T, ws string, chat, exec int64, integration, task string) (int64, Button) {
	t.Helper()
	k := p10XKey(ws, exec)
	e.exec.kinds[k] = orchestrator.WorkflowSourceOrchestratedTask
	route := orchestrator.Route{WorkspaceID: ws, ExecutionID: exec, IntegrationID: integration, ChatID: chat, TaskRef: task, ClickUpTaskID: task}
	e.proute.routes[k] = route
	e.plans.tasks[k] = []orchestrator.PlanTaskRecord{p10PlanTask(1, "api", "Sửa API", e2eSummary, "feat/orders-v2", e2ePR)}
	text := "WF-" + itoa(int(exec)) + " hoàn tất.\n" + e2eSummary + "\nPR: " + e2ePR
	if err := e.owner.SayFinal(context.Background(), route, text); err != nil {
		t.Fatal(err)
	}
	id, sent := e.msg.lastSent(t)
	if len(sent.msg.Buttons) != 1 || len(sent.msg.Buttons[0]) != 1 || sent.msg.Buttons[0][0].Text != "📤 Đăng lên ClickUp" {
		t.Fatalf("final report of %s lacks the button: %+v", task, sent.msg.Buttons)
	}
	return id, sent.msg.Buttons[0][0]
}

func e2eAsk(data string, chat int64, thread, msgID int64) InboundUpdate {
	return InboundUpdate{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: chat, ThreadID: thread, UpdateID: 8100 + msgID, Kind: UpdateCallback,
		Command: data, Args: "q1 " + itoa(int(msgID))}
}

// sentTo returns every message sent to the chat and thread so far.
func (e *e2eEnv) sentTo(chat, thread int64) []ttSent {
	e.msg.mu.Lock()
	all := append([]ttSent(nil), e.msg.sent...)
	e.msg.mu.Unlock()
	var out []ttSent
	for _, s := range all {
		if s.to.ChatID == chat && s.to.ThreadID == thread {
			out = append(out, s)
		}
	}
	return out
}

func (e *e2eEnv) allSent() []ttSent {
	e.msg.mu.Lock()
	defer e.msg.mu.Unlock()
	return append([]ttSent(nil), e.msg.sent...)
}

// TC-33 (REQ-13, requirement): E2E-008 on a single ClickUp task.
func TestP10E2E008_PlanReportToClickUpComment(t *testing.T) {
	e := newE2EEnv(t)
	reportID, button := e.finish(t, p10WS, p10Chat, 41, p10Bot, p10Task)

	// The topic holds the final report with the summary and the PR link.
	topic := e.sentTo(p10Chat, p10Thread)
	if len(topic) != 1 || !strings.Contains(topic[0].msg.Text, e2eSummary) || !strings.Contains(topic[0].msg.Text, e2ePR) {
		t.Fatalf("topic messages %+v", topic)
	}
	if button.Data != "xr:41" {
		t.Fatalf("button %+v", button)
	}

	// 📤: a preview in the same topic; nothing is posted yet.
	if out := e.w.dispatch(context.Background(), e2eAsk(button.Data, p10Chat, p10Thread, reportID), "corr"); out.code != "" || e.x.lastToast(t) != msgReportDraftMade {
		t.Fatalf("📤 press %+v toast %q", out, e.x.lastToast(t))
	}
	if n := len(e.tool.calls); n != 0 {
		t.Fatalf("%d ClickUp calls before Đăng", n)
	}
	previewID, preview := e.msg.lastSent(t)
	if preview.to.ChatID != p10Chat || preview.to.ThreadID != p10Thread || !strings.Contains(preview.msg.Text, e2ePR) || !strings.Contains(preview.msg.Text, "Task: "+p10Task) {
		t.Fatalf("preview %+v", preview)
	}

	// Đăng: exactly one comment on the right task, with the PR link.
	if out := e.w.dispatch(context.Background(), p10CallbackUpdateIn("Đăng", preview, previewID, p10Thread), "corr"); out.code != "" {
		t.Fatalf("Đăng %+v", out)
	}
	calls := e.tool.callsOf(ToolPostComment)
	if len(calls) != 1 || calls[0].input["task_id"] != p10Task {
		t.Fatalf("post calls %+v", calls)
	}
	text, _ := calls[0].input["text"].(string)
	if !strings.HasPrefix(text, "🤖 ") || !strings.Contains(text, e2ePR) || !strings.Contains(text, e2eSummary) || !strings.Contains(text, "feat/orders-v2") {
		t.Fatalf("comment text %q", text)
	}
	if _, has := calls[0].input["parent_comment_id"]; has {
		t.Fatal("a plain report comment must not be a reply")
	}
	rows := e.drafts()
	if len(rows) != 1 || rows[0].State != DraftPosted || rows[0].PostedCommentID != "9001" || rows[0].Body != "" || rows[0].TaskID != p10Task || rows[0].ExecutionID != 41 {
		t.Fatalf("drafts %+v", rows)
	}
	if ed := e.msg.lastEdit(t); ed.msgID != previewID || !strings.Contains(ed.text, "✅ Đã đăng") {
		t.Fatalf("preview edit %+v", ed)
	}

	// Nowhere else: every message went to this chat and topic, and the
	// orchestrator's topic lookups were only for this task.
	for _, s := range e.allSent() {
		if s.to.WorkspaceID != p10WS || s.to.IntegrationID != p10Bot || s.to.ChatID != p10Chat || s.to.ThreadID != p10Thread {
			t.Fatalf("a message went elsewhere: %+v", s.to)
		}
	}
	for _, task := range e.topics.asked {
		if task != p10Task {
			t.Fatalf("topic asked for %q", task)
		}
	}
	e.noWrites(t)
	if o := e.outcomes(); len(o) != 1 || o[0].State != DraftPosted || o[0].TaskID != p10Task {
		t.Fatalf("outcomes %+v", o)
	}
}

// p10CallbackUpdateIn is p10CallbackUpdate inside a topic.
func p10CallbackUpdateIn(label string, m ttSent, msgID, thread int64) InboundUpdate {
	u := p10CallbackUpdate(label, m, msgID, 1)
	u.ThreadID = thread
	return u
}

// TC-34 (REQ-13, abuse): the same journey with a second workspace and a
// second chat and topic present, a double 📤 and a double Đăng, and presses
// from the other chat: still exactly one comment, nothing leaks.
func TestP10E2E008_OtherWorkspaceChatTopicAndDoublePresses(t *testing.T) {
	e := newE2EEnv(t)
	const (
		otherChat   = p10Chat - 1
		otherThread = int64(88)
		otherTask   = "86other9"
	)
	e.topics.byTask[otherTask] = otherThread

	// Another workspace has its own execution 41 on another task, in the same chat id.
	k := p10XKey("ws-other", 41)
	e.exec.kinds[k] = WorkflowSourceLocalTask
	e.rts.routes[k] = LocalTaskRoute{WorkspaceID: "ws-other", ExecutionID: 41, IntegrationID: p10Bot, ChatID: p10Chat, ClickUpTaskID: "86wsother"}
	e.jobs.jobs[k] = WorkerJob{ID: "job-ws", WorkspaceID: "ws-other", ExecutionID: 41, StepID: StepKeyWorkerRun, State: WorkerJobSucceeded,
		Result: p10Result("việc của workspace khác", "x", "https://github.com/o/r/pull/77")}
	// Another chat has its own finished plan (execution 42) and topic.
	otherReportID, otherButton := e.finish(t, p10WS, otherChat, 42, p10Bot, otherTask)
	otherBefore := len(e.sentTo(otherChat, otherThread))
	if otherBefore != 1 {
		t.Fatalf("other chat setup %d", otherBefore)
	}

	reportID, button := e.finish(t, p10WS, p10Chat, 41, p10Bot, p10Task)

	// A press of this button from the other chat, and of the other button from
	// this chat, are stale: no draft, no message.
	sentBefore := len(e.allSent())
	for name, u := range map[string]InboundUpdate{
		"my button from the other chat": e2eAsk(button.Data, otherChat, otherThread, reportID),
		"other button from my chat":     e2eAsk(otherButton.Data, p10Chat, p10Thread, otherReportID),
	} {
		if out := e.w.dispatch(context.Background(), u, "corr"); out.code != "stale" || e.x.lastToast(t) != msgStale {
			t.Fatalf("%s: %+v toast %q", name, out, e.x.lastToast(t))
		}
	}
	if len(e.drafts()) != 0 || len(e.allSent()) != sentBefore {
		t.Fatalf("a stale press made a draft or a message: drafts %d", len(e.drafts()))
	}

	// The real journey, with a double 📤 and a double Đăng.
	for i := 0; i < 2; i++ {
		if out := e.w.dispatch(context.Background(), e2eAsk(button.Data, p10Chat, p10Thread, reportID), "corr"); out.code != "" {
			t.Fatalf("📤 press %d: %+v", i+1, out)
		}
	}
	if len(e.drafts()) != 1 || e.x.lastToast(t) != msgReportDraftOpen {
		t.Fatalf("double 📤: drafts %d toast %q", len(e.drafts()), e.x.lastToast(t))
	}
	previewID, preview := e.msg.lastSent(t)
	dang := p10CallbackUpdateIn("Đăng", preview, previewID, p10Thread)
	first := e.w.dispatch(context.Background(), dang, "corr")
	second := e.w.dispatch(context.Background(), dang, "corr")
	if first.code != "" || second.code != "stale" {
		t.Fatalf("double Đăng: %+v then %+v", first, second)
	}
	// A press of the same Đăng from the other chat does nothing either.
	intruder := dang
	intruder.ChatID, intruder.ThreadID = otherChat, otherThread
	e.w.dispatch(context.Background(), intruder, "corr")

	calls := e.tool.callsOf(ToolPostComment)
	if len(calls) != 1 || calls[0].input["task_id"] != p10Task || e.tool.realPosts() != 1 {
		t.Fatalf("post calls %+v real posts %d", calls, e.tool.realPosts())
	}
	if text, _ := calls[0].input["text"].(string); !strings.Contains(text, e2ePR) || strings.Contains(text, "pull/77") {
		t.Fatalf("comment text %q", text)
	}
	rows := e.drafts()
	if len(rows) != 1 || rows[0].State != DraftPosted || rows[0].TaskID != p10Task || rows[0].ChatID != p10Chat || rows[0].WorkspaceID != p10WS {
		t.Fatalf("drafts %+v", rows)
	}
	// The other chat and topic got nothing after their own final report.
	if got := len(e.sentTo(otherChat, otherThread)); got != otherBefore {
		t.Fatalf("other topic got %d messages, want %d", got, otherBefore)
	}
	for _, s := range e.allSent() {
		if s.to.ChatID == otherChat && s.to.ThreadID != otherThread {
			t.Fatalf("a message went to another topic of the other chat: %+v", s.to)
		}
		if s.to.ChatID != p10Chat && s.to.ChatID != otherChat {
			t.Fatalf("a message went to chat %d", s.to.ChatID)
		}
	}
	for _, task := range e.tool.callsOf(ToolPostComment) {
		if task.input["task_id"] == otherTask || task.input["task_id"] == "86wsother" {
			t.Fatalf("a comment went to %v", task.input["task_id"])
		}
	}
	e.noWrites(t)
}
