package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// Phase 10 fix-B3 regression R-B-9 (commit a507263): when a failed /task
// message's approval expires, the sweep keeps "📤 Đăng lên ClickUp" and only
// removes the other buttons. T-1: a reply to a linked ClickUp comment, plain
// or through /comment, reaches the assistant and the draft its model makes is
// threaded under that comment. Fakes only.

func p10B3Engine(m Messenger) *Engine {
	return &Engine{
		d:   ActionDeps{Chats: p6bChats{chat: ChatRef{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: b1Chat, ThreadID: b1Thread}}, Messenger: m},
		s:   ActionSettings{RequestTimeout: time.Second},
		now: time.Now,
	}
}

func p10B3Approval(t *testing.T, kind ApprovalKind, p workerJobPayload) Approval {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return Approval{ID: 9, WorkspaceID: p4WS, Kind: kind, ExecutionID: 41, TelegramMessageID: 701, Payload: raw}
}

func p10B3Payload() workerJobPayload {
	return workerJobPayload{ChatID: b1Chat, JobID: "j1", ExecutionID: 41, Attempt: 1, ReportButton: true}
}

func p10B3Removed(m *b3Msgr) []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int64(nil), m.removed...)
}

// R-B-9 case 1: ReportButton and the owner chat: only 📤 stays, nothing is removed.
func TestP10FixB3_ExpiryKeepsOnlyReportButton(t *testing.T) {
	k := &p10BKbd{b3Msgr: &b3Msgr{}}
	p10B3Engine(k).expired(context.Background(), p10B3Approval(t, ApprovalWorkerJob, p10B3Payload()))
	if k.setCount() != 1 {
		t.Fatalf("keyboard edits %d", k.setCount())
	}
	s := k.sets[0]
	if s.id != 701 || len(s.rows) != 1 || len(s.rows[0]) != 1 || s.rows[0][0].Text != "📤 Đăng lên ClickUp" || s.rows[0][0].Data != "xr:41" {
		t.Fatalf("keyboard %+v", s)
	}
	if rm := p10B3Removed(k.b3Msgr); len(rm) != 0 {
		t.Fatalf("buttons removed: %v", rm)
	}
}

// R-B-9 case 2: a failed keyboard edit is not followed by a removal.
func TestP10FixB3_ExpiryEditFailureDoesNotRemove(t *testing.T) {
	k := &p10BKbd{b3Msgr: &b3Msgr{}, setErr: errors.New("telegram down")}
	p10B3Engine(k).expired(context.Background(), p10B3Approval(t, ApprovalWorkerJob, p10B3Payload()))
	if k.setCount() != 1 {
		t.Fatalf("keyboard edits %d", k.setCount())
	}
	if rm := p10B3Removed(k.b3Msgr); len(rm) != 0 {
		t.Fatalf("buttons removed: %v", rm)
	}
}

// R-B-9 case 3: no usable report button: all buttons are removed, as before.
func TestP10FixB3_ExpiryWithoutUsableReportButtonRemovesAll(t *testing.T) {
	noFlag := p10B3Payload()
	noFlag.ReportButton = false
	otherChat := p10B3Payload()
	otherChat.ChatID = b1Chat + 1
	noExec := p10B3Payload()
	noExec.ExecutionID = 0
	negExec := p10B3Payload()
	negExec.ExecutionID = -3
	bad := p10B3Approval(t, ApprovalWorkerJob, p10B3Payload())
	bad.Payload = json.RawMessage(`{"chat_id": not json`)
	for _, tc := range []struct {
		name string
		a    Approval
	}{
		{"flag off", p10B3Approval(t, ApprovalWorkerJob, noFlag)},
		{"bad payload", bad},
		{"other chat", p10B3Approval(t, ApprovalWorkerJob, otherChat)},
		{"execution 0", p10B3Approval(t, ApprovalWorkerJob, noExec)},
		{"execution negative", p10B3Approval(t, ApprovalWorkerJob, negExec)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := &p10BKbd{b3Msgr: &b3Msgr{}}
			p10B3Engine(k).expired(context.Background(), tc.a)
			if rm := p10B3Removed(k.b3Msgr); len(rm) != 1 || rm[0] != 701 || k.setCount() != 0 {
				t.Fatalf("removed %v edits %d", rm, k.setCount())
			}
		})
	}
}

// R-B-9 case 4: a messenger that cannot edit a keyboard falls back to removing all buttons.
func TestP10FixB3_ExpiryWithoutKeyboardEditorRemovesAll(t *testing.T) {
	m := &b3Msgr{}
	p10B3Engine(m).expired(context.Background(), p10B3Approval(t, ApprovalWorkerJob, p10B3Payload()))
	if rm := p10B3Removed(m); len(rm) != 1 || rm[0] != 701 {
		t.Fatalf("removed %v", rm)
	}
}

// R-B-9 case 5: another approval kind with the same payload shape is not touched by the new path.
func TestP10FixB3_ExpiryOtherKindRemovesAll(t *testing.T) {
	k := &p10BKbd{b3Msgr: &b3Msgr{}}
	p10B3Engine(k).expired(context.Background(), p10B3Approval(t, ApprovalConfirmAction, p10B3Payload()))
	if rm := p10B3Removed(k.b3Msgr); len(rm) != 1 || rm[0] != 701 || k.setCount() != 0 {
		t.Fatalf("removed %v edits %d", rm, k.setCount())
	}
}

// R-B-9 case 6: after the real failure message's approval expired, the kept 📤 still makes a draft, and a second press reuses it.
func TestP10FixB3_ReportButtonWorksAfterExpiry(t *testing.T) {
	f := newP10BFail(t, true, false)
	if len(f.n.ap.created) != 1 {
		t.Fatalf("approvals %d", len(f.n.ap.created))
	}
	a := Approval{ID: 9, WorkspaceID: p4WS, Kind: f.n.ap.created[0].Kind, ExecutionID: 41, TelegramMessageID: 701, Payload: f.n.ap.created[0].Payload}
	p10B3Engine(f.kbd).expired(context.Background(), a)
	if f.kbd.setCount() != 1 || len(f.removed()) != 0 {
		t.Fatalf("edits %d removed %v", f.kbd.setCount(), f.removed())
	}
	f.requireOnlyReportButton(t, f.kbd.sets[0])
	for i := 0; i < 2; i++ {
		if out := f.press("xr:41", b1Chat); out.code != "" {
			t.Fatalf("📤 press %d: %+v", i, out)
		}
		rows := f.drafts()
		if len(rows) != 1 || rows[0].ExecutionID != 41 || rows[0].TaskID != "86abc" {
			t.Fatalf("press %d drafts %+v", i, rows)
		}
	}
}

// --- T-1 ---

// p10T1 runs the queued assistant job of a worker on the p10 draft environment through the real tool loop with a
// scripted model that calls draft_clickup_comment once.
func p10T1Env(t *testing.T) *p10TEnv {
	t.Helper()
	x := newP10TEnv(t)
	x.w.d.Assistant = &Assistant{Inbox: NewAssistantInbox(),
		Models: func(context.Context, string) (domain.LLMProvider, error) { return nil, errors.New("unused") }}
	x.links.put(900, p10Task, "90017")
	return x
}

func p10T1Update(thread, reply int64) InboundUpdate {
	return InboundUpdate{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: p10Chat, ThreadID: thread, UpdateID: 7001, MessageID: 31,
		ReplyToMessageID: reply, Kind: UpdateText}
}

func (x *p10TEnv) runJob(t *testing.T, job assistantJob) {
	t.Helper()
	m := &d5ToolModel{responses: []*domain.ToolResponse{
		{Stop: domain.StopToolUse, Calls: []domain.ToolCall{d5Call("c1", toolDraftClickUpComment, `{"text":"mai xong"}`)}},
		{Text: "đã soạn", Stop: domain.StopEndTurn},
	}}
	text := job.text
	if job.topicContext != "" {
		text = job.topicContext + "\n" + text
	}
	if job.replyContext != "" {
		text = job.replyContext + "\n" + text
	}
	if _, err := x.w.toolLoop(context.Background(), m, job, nil, text, "m"); err != nil {
		t.Fatalf("tool loop: %v", err)
	}
	if m.calls() != 2 {
		t.Fatalf("model calls %d", m.calls())
	}
}

func (x *p10TEnv) queuedJob(t *testing.T) assistantJob {
	t.Helper()
	select {
	case j := <-x.w.assistantJobs:
		return j
	default:
		t.Fatal("no assistant job queued")
		return assistantJob{}
	}
}

func (x *p10TEnv) requireThreadedDraft(t *testing.T, parent string) {
	t.Helper()
	rows := p10Rows(x.p10Env)
	if len(rows) != 1 || rows[0].ParentCommentID != parent || rows[0].TaskID != p10Task {
		t.Fatalf("drafts %+v", rows)
	}
	if got := len(x.tool.callsOf(ToolPostComment)); got != 0 {
		t.Fatalf("%d comments posted before Đăng", got)
	}
}

// T-1 (plain text, R-B-1): a reply to a message of a linked comment queues a job, and the model's draft is a reply to that comment.
func TestP10T1_LinkedReplyDraftCarriesParentComment(t *testing.T) {
	x := p10T1Env(t)
	key, err := x.w.d.Assistant.Inbox.StageAssistantText(p10WS, "mai xong", x.w.now())
	if err != nil {
		t.Fatal(err)
	}
	u := p10T1Update(p10Thread, 900)
	u.Args = AssistantTextPrefix + key
	to := ChatRef{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: p10Chat, ThreadID: p10Thread}
	out := x.w.workflowOrAssistant(withConversation(context.Background(), 77, UpdateText), u, to, "owner", "corr")
	if out.code != "assistant_queued" {
		t.Fatalf("outcome %+v", out)
	}
	job := x.queuedJob(t)
	if job.replyContext != replyCommentContext(p10Task) || job.u.ReplyToMessageID != 900 {
		t.Fatalf("job %+v", job)
	}
	x.runJob(t, job)
	x.requireThreadedDraft(t, "90017")
	if _, sent := x.msg.lastSent(t); !strings.Contains(sent.msg.Text, "Trả lời bình luận: 90017") {
		t.Fatalf("preview %q", sent.msg.Text)
	}
}

// T-1 (/comment): the same reply with /comment queues a job whose draft is a reply to the linked comment.
func TestP10T1_CommentCommandDraftCarriesParentComment(t *testing.T) {
	x := p10T1Env(t)
	u := p10T1Update(p10Thread, 900)
	u.Kind, u.Command, u.Args, u.Attempts = UpdateCommand, "/comment", "mai xong", 1
	to := ChatRef{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: p10Chat, ThreadID: p10Thread}
	out := x.w.command(context.Background(), u, to, "owner", "corr")
	if out.code != "assistant_queued" {
		t.Fatalf("outcome %+v", out)
	}
	job := x.queuedJob(t)
	if job.replyContext != replyCommentContext(p10Task) || job.topicContext == "" {
		t.Fatalf("job %+v", job)
	}
	x.runJob(t, job)
	x.requireThreadedDraft(t, "90017")
}

// T-1 (control): /comment without a reply makes a plain, unthreaded draft on the topic's task.
func TestP10T1_CommentCommandWithoutReplyIsNotThreaded(t *testing.T) {
	x := p10T1Env(t)
	u := p10T1Update(p10Thread, 0)
	u.Kind, u.Command, u.Args, u.Attempts = UpdateCommand, "/comment", "mai xong", 1
	to := ChatRef{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: p10Chat, ThreadID: p10Thread}
	if out := x.w.command(context.Background(), u, to, "owner", "corr"); out.code != "assistant_queued" {
		t.Fatalf("outcome %+v", out)
	}
	x.runJob(t, x.queuedJob(t))
	x.requireThreadedDraft(t, "")
}
