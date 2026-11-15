package behavior

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Regressions for 1db162b: explicit WF references in owner text win over the
// reply target; several distinct references always ask; a plain reply to a
// terminal workflow's message reaches the assistant instead of being refused.

const phase6RTForeignWS = "019a6690-abcd-7000-8000-0000000000ff"

type phase6RTEnv struct {
	w     *InboxWorker
	r     *phase6BReader
	c     *phase6BConversations
	m     *p4Messenger
	ap    *p4Approvals
	inbox *AssistantInbox
}

// phase6RTSetup builds a worker with WF-101..103 running, WF-44 completed and
// WF-45 running in another workspace. Every reply resolves to replyTarget
// (0 means no reply binding).
func phase6RTSetup(replyTarget int64, bound ...int64) *phase6RTEnv {
	r := &phase6BReader{details: map[int64]workflow.ExecutionDetail{
		101: phase6BDetail(101, p4WS, workflow.StateRunning),
		102: phase6BDetail(102, p4WS, workflow.StateRunning),
		103: phase6BDetail(103, p4WS, workflow.StateRunning),
		44:  phase6BDetail(44, p4WS, workflow.StateCompleted),
		45:  phase6BDetail(45, phase6RTForeignWS, workflow.StateRunning),
	}}
	c := &phase6BConversations{bound: bound}
	if replyTarget > 0 {
		c.byResource = []int64{replyTarget}
	}
	m := &p4Messenger{}
	ap := newP4Approvals()
	inbox := NewAssistantInbox()
	w := phase6BWorker(r, c, m)
	w.d.Approvals = ap
	w.d.Assistant = &Assistant{Inbox: inbox, Workflows: r, Conversations: c,
		Models: func(context.Context, string) (domain.LLMProvider, error) { return nil, errors.New("unused") }}
	return &phase6RTEnv{w: w, r: r, c: c, m: m, ap: ap, inbox: inbox}
}

// text sends staged free text, as a reply when reply is true.
func (e *phase6RTEnv) text(t *testing.T, text string, reply bool) outcome {
	t.Helper()
	key, err := e.inbox.StageAssistantText(p4WS, text, e.w.now())
	if err != nil {
		t.Fatal(err)
	}
	u := InboundUpdate{WorkspaceID: p4WS, IntegrationID: "bot-a", UpdateID: 501, ChatID: 5, MessageID: 600,
		Kind: UpdateText, Args: AssistantTextPrefix + key}
	if reply {
		u.ReplyToMessageID = 9
	}
	return e.w.workflowOrAssistant(withConversation(context.Background(), 77, UpdateText), u, ChatRef{WorkspaceID: p4WS, ChatID: 5}, "owner", "corr")
}

func (e *phase6RTEnv) note(args string, reply bool) outcome {
	u := InboundUpdate{WorkspaceID: p4WS, IntegrationID: "bot-a", UpdateID: 502, ChatID: 5, MessageID: 601,
		Kind: UpdateCommand, Command: "/note", Args: args}
	if reply {
		u.ReplyToMessageID = 9
	}
	return e.w.workflowControlCommand(withConversation(context.Background(), 77, UpdateCommand), u, ChatRef{WorkspaceID: p4WS, ChatID: 5}, "owner", "corr")
}

func (e *phase6RTEnv) total() int {
	n := 0
	for _, items := range e.c.instructions {
		n += len(items)
	}
	return n
}

func (e *phase6RTEnv) job(t *testing.T) (assistantJob, bool) {
	t.Helper()
	select {
	case j := <-e.w.assistantJobs:
		return j, true
	default:
		return assistantJob{}, false
	}
}

// requireClarified asserts a clarification with buttons and no stored note.
func (e *phase6RTEnv) requireClarified(t *testing.T, out outcome) {
	t.Helper()
	if out.code != "workflow_target" || e.total() != 0 || len(e.ap.created) != 1 {
		t.Fatalf("want clarification: code=%q instructions=%v approvals=%d texts=%q", out.code, e.c.instructions, len(e.ap.created), e.m.texts())
	}
	last := e.m.last(t)
	if last.Text != "Chọn workflow cần xử lý:" || len(last.Buttons) == 0 {
		t.Fatalf("clarification message=%+v", last)
	}
	if _, queued := e.job(t); queued {
		t.Fatal("clarification also queued an assistant job")
	}
}

// TC-RT01 (REQ-6, REQ-7): a plain reply to WF-101's message that names exactly
// one other ref stores the instruction on that ref only.
func TestPhase6ReplyTarget_SingleExplicitRefWinsOverReplyTarget(t *testing.T) {
	e := phase6RTSetup(101, 101, 102)
	out := e.text(t, "thêm integration test cho WF-102 nữa", true)
	if out.code != "" || len(e.c.instructions[102]) != 1 || len(e.c.instructions[101]) != 0 || e.total() != 1 {
		t.Fatalf("code=%q instructions=%v", out.code, e.c.instructions)
	}
	if got := e.c.instructions[102][0].Text; got != "thêm integration test cho WF-102 nữa" {
		t.Fatalf("stored text=%q", got)
	}
	if ack := e.m.last(t).Text; ack != "WF-102: đã nhận yêu cầu bổ sung (v1)" {
		t.Fatalf("ack=%q", ack)
	}
	if _, queued := e.job(t); queued {
		t.Fatal("instruction also queued an assistant job")
	}
}

// TC-RT02 (REQ-6): a reply naming two distinct refs asks which one; neither
// the refs nor the reply target receive a note.
func TestPhase6ReplyTarget_TwoRefsInReplyClarify(t *testing.T) {
	e := phase6RTSetup(101, 101, 102, 103)
	e.requireClarified(t, e.text(t, "thêm test cho WF-102 và WF-103", true))
}

// TC-RT03 (REQ-6): a leading ref plus another distinct ref clarifies, with or
// without a reply target, and never falls back to the reply target.
func TestPhase6ReplyTarget_LeadingRefPlusAnotherClarifies(t *testing.T) {
	for _, reply := range []bool{true, false} {
		name := "no reply"
		if reply {
			name = "reply"
		}
		t.Run(name, func(t *testing.T) {
			e := phase6RTSetup(101, 101, 102)
			e.requireClarified(t, e.text(t, "WF-101 thêm test giống WF-102", reply))
		})
	}
	t.Run("same ref repeated is one", func(t *testing.T) {
		e := phase6RTSetup(101, 101, 102)
		out := e.text(t, "WF-102 thêm test, wf-102 cần gấp", true)
		if out.code != "" || len(e.c.instructions[102]) != 1 || e.total() != 1 {
			t.Fatalf("code=%q instructions=%v", out.code, e.c.instructions)
		}
	})
}

// TC-RT04 (REQ-6, REQ-7): /note uses one embedded ref as the target (even as
// a reply to another workflow's message); several refs clarify and store
// nothing.
func TestPhase6ReplyTarget_NoteEmbeddedRefs(t *testing.T) {
	for _, reply := range []bool{false, true} {
		name := "plain"
		if reply {
			name = "reply to WF-101"
		}
		t.Run("one ref "+name, func(t *testing.T) {
			e := phase6RTSetup(101, 101, 102)
			out := e.note("thêm test cho WF-102", reply)
			if out.code != "" || len(e.c.instructions[102]) != 1 || e.total() != 1 {
				t.Fatalf("code=%q instructions=%v", out.code, e.c.instructions)
			}
			if got := e.c.instructions[102][0].Text; got != "thêm test cho WF-102" {
				t.Fatalf("stored text=%q", got)
			}
			if ack := e.m.last(t).Text; ack != "WF-102: đã nhận yêu cầu bổ sung (v1)" {
				t.Fatalf("ack=%q", ack)
			}
		})
	}
	for _, args := range []string{"so sánh WF-101 và WF-102", "WF-101 thêm test giống WF-102"} {
		t.Run("two refs "+args, func(t *testing.T) {
			e := phase6RTSetup(101, 101, 102)
			e.requireClarified(t, e.note(args, true))
		})
	}
}

// TC-RT05 (REQ-7): a plain reply (no ref, no question mark) to a terminal
// workflow's message is a conversation: it is queued for the assistant with
// that workflow as context, nothing is stored and no refusal is sent.
func TestPhase6ReplyTarget_TerminalReplyGoesToAssistant(t *testing.T) {
	e := phase6RTSetup(44, 44)
	e.c.appendErr = workflow.ErrNotEligible
	out := e.text(t, "cảm ơn, báo cáo rất tốt", true)
	if out.code != "assistant_queued" || e.total() != 0 {
		t.Fatalf("code=%q instructions=%v", out.code, e.c.instructions)
	}
	j, ok := e.job(t)
	if !ok || j.workflowID != 44 || j.text != "cảm ơn, báo cáo rất tốt" {
		t.Fatalf("job=%+v queued=%v", j, ok)
	}
	for _, text := range e.m.texts() {
		if strings.Contains(text, "đã kết thúc") {
			t.Fatalf("terminal refusal sent: %q", text)
		}
	}
}

// TC-RT05A (REQ-7): the same plain reply to an active workflow's message is
// still an instruction on the reply target, not an assistant turn.
func TestPhase6ReplyTarget_ActiveReplyStillStores(t *testing.T) {
	e := phase6RTSetup(101, 101)
	out := e.text(t, "thêm integration test", true)
	if out.code != "" || len(e.c.instructions[101]) != 1 || e.total() != 1 {
		t.Fatalf("code=%q instructions=%v", out.code, e.c.instructions)
	}
	if _, queued := e.job(t); queued {
		t.Fatal("active reply queued an assistant job")
	}
}

// TC-RT06 (REQ-7): explicit WF-n <text> and /note WF-n <text> to a terminal
// workflow keep the terminal refusal, also when sent as a reply.
func TestPhase6ReplyTarget_ExplicitTerminalStillRefused(t *testing.T) {
	cases := map[string]func(e *phase6RTEnv, t *testing.T) outcome{
		"free text":          func(e *phase6RTEnv, t *testing.T) outcome { return e.text(t, "WF-44 thêm test", false) },
		"free text as reply": func(e *phase6RTEnv, t *testing.T) outcome { return e.text(t, "WF-44 thêm test", true) },
		"note":               func(e *phase6RTEnv, _ *testing.T) outcome { return e.note("WF-44 thêm test", false) },
		"note as reply":      func(e *phase6RTEnv, _ *testing.T) outcome { return e.note("WF-44 thêm test", true) },
		"embedded in reply":  func(e *phase6RTEnv, t *testing.T) outcome { return e.text(t, "thêm test cho WF-44", true) },
	}
	for name, send := range cases {
		t.Run(name, func(t *testing.T) {
			e := phase6RTSetup(101, 101)
			e.c.appendErr = workflow.ErrNotEligible
			out := send(e, t)
			if out.code != "workflow_terminal" || e.total() != 0 {
				t.Fatalf("code=%q instructions=%v", out.code, e.c.instructions)
			}
			if text := e.m.last(t).Text; text != "WF-44: workflow đã kết thúc, không thể nhận yêu cầu bổ sung." {
				t.Fatalf("refusal=%q", text)
			}
			if _, queued := e.job(t); queued {
				t.Fatal("refusal also queued an assistant job")
			}
		})
	}
}

// TC-RT07 (REQ-9): a bound reply ending in ? or ？ goes to the assistant with
// the single named ref, or else the reply target, as context; nothing stored.
func TestPhase6ReplyTarget_QuestionsGoToAssistant(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       int64
	}{
		{name: "reply target ascii", text: "đã chạy test chưa?", want: 101},
		{name: "reply target fullwidth", text: "đã chạy test chưa？", want: 101},
		{name: "one embedded ref", text: "còn WF-102 thì sao?", want: 102},
		{name: "terminal reply target", text: "kết quả cuối cùng là gì?", want: 101},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := phase6RTSetup(101, 101, 102)
			if tc.name == "terminal reply target" {
				e.r.details[101] = phase6BDetail(101, p4WS, workflow.StateCompleted)
			}
			out := e.text(t, tc.text, true)
			j, ok := e.job(t)
			if out.code != "assistant_queued" || !ok || j.workflowID != tc.want || e.total() != 0 {
				t.Fatalf("code=%q job=%+v queued=%v instructions=%v", out.code, j, ok, e.c.instructions)
			}
		})
	}
	t.Run("two refs question has no workflow context", func(t *testing.T) {
		e := phase6RTSetup(101, 101, 102, 103)
		out := e.text(t, "nên chọn WF-102 hay WF-103?", true)
		j, ok := e.job(t)
		if out.code != "assistant_queued" || !ok || j.workflowID != 0 || e.total() != 0 {
			t.Fatalf("code=%q job=%+v queued=%v instructions=%v", out.code, j, ok, e.c.instructions)
		}
	})
}

// TC-RT08 (REQ-6, REQ-10): a reply naming an unknown or another workspace's
// ref never writes to the reply target or the named ref, and both answer
// identically so the foreign workflow's existence is not revealed.
func TestPhase6ReplyTarget_UnknownOrForeignRefNeverWrites(t *testing.T) {
	type seen struct {
		code    string
		texts   []string
		buttons []string
	}
	run := func(t *testing.T, ref string, bound []int64) seen {
		e := phase6RTSetup(101, bound...)
		out := e.text(t, "thêm test cho "+ref, true)
		if e.total() != 0 {
			t.Fatalf("%s wrote instructions=%v", ref, e.c.instructions)
		}
		if _, queued := e.job(t); queued {
			t.Fatalf("%s queued an assistant job", ref)
		}
		s := seen{code: out.code, texts: e.m.texts()}
		for _, msg := range e.m.sent {
			for _, row := range msg.Buttons {
				for _, b := range row {
					s.buttons = append(s.buttons, b.Text)
				}
			}
		}
		for _, text := range append(append([]string(nil), s.texts...), s.buttons...) {
			if strings.Contains(text, "45") || strings.Contains(text, "999") {
				t.Fatalf("%s echoed in %q", ref, text)
			}
		}
		return s
	}
	for _, tc := range []struct {
		name  string
		bound []int64
		code  string
	}{
		{name: "no active binding", code: "workflow_target_not_found"},
		{name: "reply target bound", bound: []int64{101}, code: "workflow_target"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			foreign := run(t, "WF-45", tc.bound)
			unknown := run(t, "WF-999", tc.bound)
			if foreign.code != tc.code || unknown.code != tc.code ||
				strings.Join(foreign.texts, "|") != strings.Join(unknown.texts, "|") ||
				strings.Join(foreign.buttons, "|") != strings.Join(unknown.buttons, "|") {
				t.Fatalf("foreign=%+v unknown=%+v", foreign, unknown)
			}
		})
	}
}

// TC-RT09 (REQ-9; regression for 0a45234): free text that starts with WF-n
// but ends in ?/？ is a question for the assistant, plain or as a reply to
// another workflow's message, terminal or not. An unknown or foreign leading
// ref gets no workflow context and is not echoed. /note still forces an
// instruction.
func TestPhase6ReplyTarget_WFPrefixedQuestionGoesToAssistant(t *testing.T) {
	for _, tc := range []struct {
		name     string
		text     string
		reply    bool
		terminal bool
		want     int64
	}{
		{name: "plain", text: "WF-102 chạy tới đâu rồi?", want: 102},
		{name: "reply to WF-101", text: "WF-102 chạy tới đâu rồi?", reply: true, want: 102},
		{name: "fullwidth", text: "WF-102 chạy tới đâu rồi？", reply: true, want: 102},
		{name: "terminal plain", text: "WF-102 chạy tới đâu rồi?", terminal: true, want: 102},
		{name: "terminal reply", text: "WF-102 chạy tới đâu rồi?", reply: true, terminal: true, want: 102},
		{name: "foreign plain", text: "WF-45 chạy tới đâu rồi?"},
		{name: "foreign reply", text: "WF-45 chạy tới đâu rồi?", reply: true},
		{name: "unknown plain", text: "WF-999 chạy tới đâu rồi?"},
		{name: "unknown reply", text: "WF-999 chạy tới đâu rồi?", reply: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := phase6RTSetup(101, 101, 102)
			if tc.terminal {
				e.r.details[102] = phase6BDetail(102, p4WS, workflow.StateCompleted)
				e.c.appendErr = workflow.ErrNotEligible
			}
			out := e.text(t, tc.text, tc.reply)
			j, ok := e.job(t)
			if out.code != "assistant_queued" || !ok || j.workflowID != tc.want || j.text != tc.text || e.total() != 0 {
				t.Fatalf("code=%q job=%+v queued=%v instructions=%v", out.code, j, ok, e.c.instructions)
			}
			if len(e.ap.created) != 0 {
				t.Fatalf("question created %d approvals", len(e.ap.created))
			}
			for _, text := range e.m.texts() {
				if strings.Contains(text, "45") || strings.Contains(text, "999") || strings.Contains(text, "đã kết thúc") {
					t.Fatalf("unexpected reply %q", text)
				}
			}
		})
	}
	for _, reply := range []bool{false, true} {
		name := "note plain"
		if reply {
			name = "note reply to WF-101"
		}
		t.Run(name, func(t *testing.T) {
			e := phase6RTSetup(101, 101, 102)
			out := e.note("WF-102 chạy tới đâu rồi?", reply)
			if out.code != "" || len(e.c.instructions[102]) != 1 || e.total() != 1 {
				t.Fatalf("code=%q instructions=%v", out.code, e.c.instructions)
			}
			if got := e.c.instructions[102][0].Text; got != "chạy tới đâu rồi?" {
				t.Fatalf("stored text=%q", got)
			}
			if _, queued := e.job(t); queued {
				t.Fatal("/note queued an assistant job")
			}
		})
	}
}
