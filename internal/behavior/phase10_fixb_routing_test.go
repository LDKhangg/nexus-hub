package behavior

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Phase 10 Fix B regressions (review B, commits b317aa1 and 7c49cfc):
// R-B-1 (a reply to a linked ClickUp comment goes to the assistant) and R-B-3
// (/comment <text>). Fakes only; the worker is the real InboxWorker on the
// batch B1 environment (task topic thread 55, task 86abc).

// p10BLinks is a comment link store keyed by message id, scoped to b1's chat.
type p10BLinks struct {
	mu    sync.Mutex
	byMsg map[int64]CommentLink
	err   error
	calls int
}

func newP10BLinks() *p10BLinks { return &p10BLinks{byMsg: map[int64]CommentLink{}} }

func (l *p10BLinks) LookupCommentLink(_ context.Context, ws, integration string, chat, msg int64) (CommentLink, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.err != nil {
		return CommentLink{}, l.err
	}
	c, ok := l.byMsg[msg]
	if !ok || c.WorkspaceID != ws || c.IntegrationID != integration || c.ChatID != chat {
		return CommentLink{}, ErrNotFound
	}
	return c, nil
}

func (l *p10BLinks) put(msg int64, task, comment string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.byMsg[msg] = CommentLink{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: b1Chat, MessageID: msg, TaskID: task, CommentID: comment}
}

func (l *p10BLinks) setErr(err error) {
	l.mu.Lock()
	l.err = err
	l.mu.Unlock()
}

// p10BDrafts is a ReportDrafts that only has to exist: the draft tool is offered when it is wired.
type p10BDrafts struct{}

func (p10BDrafts) Create(context.Context, DraftRequest) (int64, error) {
	return 0, errors.New("unused")
}
func (p10BDrafts) OpenExecutionReportDraft(context.Context, string, int64) (ClickUpDraft, bool, error) {
	return ClickUpDraft{}, false, nil
}

const p10BReply = int64(600)

type p10BEnv struct {
	*b1Env
	links *p10BLinks
}

func newP10BEnv(running int) *p10BEnv {
	e := &p10BEnv{b1Env: newB1Env(b1States(running)), links: newP10BLinks()}
	e.w.d.CommentLinks = e.links
	e.w.d.ReportDrafts = p10BDrafts{}
	return e
}

// staged stores text in the assistant's in-memory stage and returns its Args.
func (e *p10BEnv) staged(t *testing.T, text string) string {
	t.Helper()
	key, err := e.inbox.StageAssistantText(p4WS, text, e.w.now())
	if err != nil {
		t.Fatal(err)
	}
	return AssistantTextPrefix + key
}

func (e *p10BEnv) stillStaged(args string) bool {
	key := strings.TrimPrefix(args, AssistantTextPrefix)
	_, ok := e.inbox.take(p4WS, key, e.w.now())
	return ok
}

// comment runs "/comment <args>" in thread as a reply to reply, with attempts claims done.
func (e *p10BEnv) comment(thread, reply int64, args string, attempts int) outcome {
	u := e.update(thread, reply)
	u.Kind, u.Command, u.Args, u.Attempts = UpdateCommand, "/comment", args, attempts
	return e.w.command(context.Background(), u, e.to(thread), "owner", "corr")
}

func (e *p10BEnv) noWork(t *testing.T) {
	t.Helper()
	if e.stored() != 0 || len(e.ap.created) != 0 {
		t.Fatalf("a note or clarification was made: %v approvals %d", e.c.instructions, len(e.ap.created))
	}
	if j, ok := e.job(); ok {
		t.Fatalf("an assistant job was queued: %+v", j)
	}
}

// --- R-B-1 ---

// R-B-1: with 0, 1 and 2 active executions a reply to a linked comment queues the assistant with the parent
// comment's task as context; it is never a worker note and never a clarification.
func TestP10FixB_LinkedReplyAlwaysReachesAssistant(t *testing.T) {
	for _, running := range []int{0, 1, 2} {
		t.Run(string(rune('0'+running))+" active", func(t *testing.T) {
			e := newP10BEnv(running)
			e.links.put(p10BReply, b1Task, "5001")
			out := e.say(t, b1Thread, p10BReply, "cảm ơn, đã rõ")
			if out.code != "assistant_queued" {
				t.Fatalf("outcome %+v texts %q", out, e.m.texts())
			}
			j, ok := e.job()
			if !ok || j.replyContext != replyCommentContext(b1Task) || j.text != "cảm ơn, đã rõ" {
				t.Fatalf("job %+v ok %v", j, ok)
			}
			if strings.Contains(j.replyContext, "cảm ơn") || e.stored() != 0 || len(e.ap.created) != 0 {
				t.Fatalf("context %q stored %v approvals %d", j.replyContext, e.c.instructions, len(e.ap.created))
			}
		})
	}
}

// R-B-1: a link wins over a workflow binding of the same message.
func TestP10FixB_LinkWinsOverWorkflowBinding(t *testing.T) {
	e := newP10BEnv(1)
	e.c.resourcesByID[workflow.TelegramMessageRef(b1Bot, b1Chat, p10BReply)] = []int64{101}
	// Control: without a link the reply is a note for execution 101.
	if out := e.say(t, b1Thread, p10BReply, "thêm test cho case Y"); out.code != "" || len(e.c.instructions[101]) != 1 {
		t.Fatalf("control: %+v %v", out, e.c.instructions)
	}
	if _, ok := e.job(); ok {
		t.Fatal("control queued a job")
	}
	e.links.put(p10BReply, b1Task, "5001")
	out := e.say(t, b1Thread, p10BReply, "thêm test cho case Z")
	j, ok := e.job()
	if out.code != "assistant_queued" || !ok || j.replyContext != replyCommentContext(b1Task) || j.workflowID != 0 {
		t.Fatalf("outcome %+v job %+v ok %v", out, j, ok)
	}
	if len(e.c.instructions[101]) != 1 {
		t.Fatalf("the linked reply became a note: %v", e.c.instructions)
	}
}

// R-B-1: a reply to an unlinked message keeps today's behaviour (one active: instruction).
func TestP10FixB_UnlinkedReplyKeepsRouting(t *testing.T) {
	e := newP10BEnv(1)
	e.links.put(p10BReply+1, b1Task, "5001") // another message is linked, not this one
	if out := e.say(t, b1Thread, p10BReply, "làm tiếp phần test"); out.code != "" || len(e.c.instructions[101]) != 1 {
		t.Fatalf("outcome %+v instructions %v", out, e.c.instructions)
	}
	if _, ok := e.job(); ok {
		t.Fatal("an unlinked reply queued the assistant")
	}
}

// R-B-1 (abuse): a link whose comment id is not numeric is not usable: the reply keeps its old routing.
func TestP10FixB_UnusableLinkIsIgnored(t *testing.T) {
	e := newP10BEnv(1)
	e.links.put(p10BReply, b1Task, "abc")
	if out := e.say(t, b1Thread, p10BReply, "làm tiếp"); out.code != "" || len(e.c.instructions[101]) != 1 {
		t.Fatalf("outcome %+v instructions %v", out, e.c.instructions)
	}
}

// R-B-1: a failed link lookup retries with the text still staged; the last attempt answers msgFailed and drops it.
func TestP10FixB_LinkLookupErrorRetriesThenFails(t *testing.T) {
	e := newP10BEnv(1)
	args := e.staged(t, "trả lời bình luận")
	e.links.setErr(errors.New("db down"))
	mk := func(attempts int) InboundUpdate {
		u := e.update(b1Thread, p10BReply)
		u.Args, u.Attempts = args, attempts
		return u
	}
	ctx := withConversation(context.Background(), 77, UpdateText)
	for attempt := 1; attempt < e.w.settings.MaxAttempts; attempt++ {
		out := e.w.workflowOrAssistant(ctx, mk(attempt), e.to(b1Thread), "owner", "corr")
		if !out.retry || out.code != "storage" || e.m.count() != 0 {
			t.Fatalf("attempt %d: %+v messages %q", attempt, out, e.m.texts())
		}
	}
	// The store is back: the same staged text is still there and routes to the assistant.
	e.links.setErr(nil)
	e.links.put(p10BReply, b1Task, "5001")
	if out := e.w.workflowOrAssistant(ctx, mk(2), e.to(b1Thread), "owner", "corr"); out.code != "assistant_queued" {
		t.Fatalf("after recovery: %+v", out)
	}
	if j, ok := e.job(); !ok || j.text != "trả lời bình luận" {
		t.Fatalf("job %+v", j)
	}

	// Last attempt: msgFailed, code comment_link_unreadable, text dropped, no note.
	args2 := e.staged(t, "một câu trả lời khác")
	e.links.setErr(errors.New("db down"))
	u := e.update(b1Thread, p10BReply)
	u.Args, u.Attempts = args2, e.w.settings.MaxAttempts
	out := e.w.workflowOrAssistant(ctx, u, e.to(b1Thread), "owner", "corr")
	if out.retry || out.code != "comment_link_unreadable" || e.m.last(t).Text != msgFailed {
		t.Fatalf("last attempt: %+v last %q", out, e.m.texts())
	}
	if e.stillStaged(args2) {
		t.Fatal("the text stayed staged after the last attempt")
	}
	if e.stored() != 0 {
		t.Fatalf("a failed lookup became a note: %v", e.c.instructions)
	}
}

// --- R-B-3: /comment ---

// R-B-3: with 0, 1 and 2 active executions /comment drafts: the assistant gets the task topic context and the
// fixed instruction; nothing becomes a worker note or a clarification.
func TestP10FixB_CommentCommandAlwaysDrafts(t *testing.T) {
	for _, running := range []int{0, 1, 2} {
		t.Run(string(rune('0'+running))+" active", func(t *testing.T) {
			e := newP10BEnv(running)
			out := e.comment(b1Thread, 0, e.staged(t, "Đã xong phần API"), 1)
			if out.code != "assistant_queued" || e.m.count() != 0 {
				t.Fatalf("outcome %+v texts %q", out, e.m.texts())
			}
			j, ok := e.job()
			if !ok || j.text != "Đã xong phần API" || j.topicContext != b1TaskLine+"\n"+commentInstruction || j.replyContext != "" || j.workflowID != 0 {
				t.Fatalf("job %+v ok %v", j, ok)
			}
			if e.stored() != 0 || len(e.ap.created) != 0 {
				t.Fatalf("note or clarification: %v approvals %d", e.c.instructions, len(e.ap.created))
			}
		})
	}
}

// R-B-3: the plain text form (no stage key) works the same and the text is not changed.
func TestP10FixB_CommentCommandPlainArgs(t *testing.T) {
	e := newP10BEnv(1)
	if out := e.comment(b1Thread, 0, "  nội dung thẳng  ", 1); out.code != "assistant_queued" {
		t.Fatalf("outcome %+v", out)
	}
	if j, ok := e.job(); !ok || j.text != "nội dung thẳng" {
		t.Fatalf("job %+v", j)
	}
	e.noWork(t)
}

// R-B-3: a reply to a linked comment of the same task is threaded; one of ANOTHER task, an unlinked one and a
// broken link give no reply context.
func TestP10FixB_CommentCommandReplyContext(t *testing.T) {
	e := newP10BEnv(0)
	e.links.put(600, b1Task, "5001")
	e.links.put(601, b1Other, "5002")
	e.links.put(602, b1Task, "not-a-number")
	for _, tc := range []struct {
		name  string
		reply int64
		want  string
	}{
		{"same task comment", 600, replyCommentContext(b1Task)},
		{"another task's comment", 601, ""},
		{"broken link", 602, ""},
		{"unlinked message", 603, ""},
		{"no reply", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if out := e.comment(b1Thread, tc.reply, e.staged(t, "ok"), 1); out.code != "assistant_queued" {
				t.Fatalf("outcome %+v", out)
			}
			j, ok := e.job()
			if !ok || j.replyContext != tc.want || j.topicContext != b1TaskLine+"\n"+commentInstruction {
				t.Fatalf("job %+v ok %v", j, ok)
			}
		})
	}
}

// R-B-3: empty text, and whitespace only, answer the usage line with no draft and no job.
func TestP10FixB_CommentCommandUsageWithoutText(t *testing.T) {
	for _, args := range []string{"", "   "} {
		e := newP10BEnv(1)
		out := e.comment(b1Thread, 0, args, 1)
		if out.code != "usage" || e.m.last(t).Text != usageComment {
			t.Fatalf("args %q: %+v %q", args, out, e.m.texts())
		}
		e.noWork(t)
	}
	// A staged text that is blank also gives the usage line.
	e := newP10BEnv(1)
	if out := e.comment(b1Thread, 0, e.staged(t, "   "), 1); out.code != "usage" || e.m.last(t).Text != usageComment {
		t.Fatalf("blank staged: %+v %q", out, e.m.texts())
	}
	e.noWork(t)
}

// R-B-3: outside a ClickUp task topic (General, a fixed topic, an unknown thread, a non-task subject) the usage
// line is the answer, nothing is drafted, and the staged text is dropped.
func TestP10FixB_CommentCommandOutsideTaskTopic(t *testing.T) {
	for name, thread := range map[string]int64{"general": 0, "unknown thread": 999, "fixed topic": 77, "other subject": 78} {
		t.Run(name, func(t *testing.T) {
			e := newP10BEnv(1)
			e.topics.rows = append(e.topics.rows,
				Topic{ID: 77, WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: b1Chat, ThreadID: 77, SubjectKind: TopicSubjectFixed, SubjectKey: "Điều phối", State: TopicOpen},
				Topic{ID: 78, WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: b1Chat, ThreadID: 78, SubjectKind: "workflow", SubjectKey: "41", State: TopicOpen})
			args := e.staged(t, "bình luận lạc chỗ")
			out := e.comment(thread, 0, args, 1)
			if out.code != "usage" || e.m.last(t).Text != usageComment {
				t.Fatalf("outcome %+v texts %q", out, e.m.texts())
			}
			e.noWork(t)
			if e.stillStaged(args) {
				t.Fatal("the text stayed staged")
			}
		})
	}
}

// R-B-3: a topic of another workspace's scope (a row for another chat) is not a task topic.
func TestP10FixB_CommentCommandForeignTopicRow(t *testing.T) {
	e := newP10BEnv(1)
	e.topics.sloppy = true // the store ignores the scope; the worker's own guard must reject the row
	e.topics.rows = []Topic{{ID: 55, WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: b1Chat - 1, ThreadID: b1Thread, SubjectKind: TopicSubjectClickUpTask, SubjectKey: b1Task, State: TopicOpen}}
	if out := e.comment(b1Thread, 0, e.staged(t, "x"), 1); out.code != "usage" {
		t.Fatalf("outcome %+v", out)
	}
	e.noWork(t)
}

// R-B-3: the draft tool or the assistant is not wired: a plain refusal, no job.
func TestP10FixB_CommentCommandNotAvailable(t *testing.T) {
	e := newP10BEnv(1)
	e.w.d.ReportDrafts = nil
	args := e.staged(t, "nội dung")
	if out := e.comment(b1Thread, 0, args, 1); out.code != "not_available" || e.m.last(t).Text != msgNotAvailable {
		t.Fatalf("outcome %+v texts %q", out, e.m.texts())
	}
	e.noWork(t)

	off := newP10BEnv(1)
	off.w.d.Assistant = nil
	if out := off.comment(b1Thread, 0, "nội dung", 1); out.code != "" || off.m.last(t).Text != msgHint {
		t.Fatalf("no assistant: %+v texts %q", out, off.m.texts())
	}
}

// R-B-3: the staged text is gone (expired or already taken): the "text gone" answer, no job.
func TestP10FixB_CommentCommandTextGone(t *testing.T) {
	e := newP10BEnv(1)
	args := e.staged(t, "nội dung")
	if out := e.comment(b1Thread, 0, args, 1); out.code != "assistant_queued" {
		t.Fatalf("first: %+v", out)
	}
	e.job()
	// The same key a second time (a duplicate delivery after the text was taken).
	if out := e.comment(b1Thread, 0, args, 2); out.code != "assistant_text_gone" || e.m.last(t).Text != msgAssistantGone {
		t.Fatalf("second: %+v texts %q", out, e.m.texts())
	}
	e.noWork(t)
	// An expired stage.
	args = e.staged(t, "cũ")
	e.w.now = func() time.Time { return time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC) }
	if out := e.comment(b1Thread, 0, args, 1); out.code != "assistant_text_gone" {
		t.Fatalf("expired: %+v", out)
	}
}

// R-B-3: a topic or link lookup error retries with the text still staged; the last attempt gives msgFailed and
// drops the text.
func TestP10FixB_CommentCommandLookupErrors(t *testing.T) {
	// The link lookup only runs for a reply, so that case sends /comment as a reply to a linked message.
	for name, tc := range map[string]struct {
		reply int64
		fail  func(e *p10BEnv, err error)
	}{
		"topic lookup": {0, func(e *p10BEnv, err error) { e.topics.mu.Lock(); e.topics.err = err; e.topics.mu.Unlock() }},
		"link lookup":  {p10BReply, func(e *p10BEnv, err error) { e.links.setErr(err) }},
	} {
		fail, reply := tc.fail, tc.reply
		t.Run(name, func(t *testing.T) {
			e := newP10BEnv(1)
			e.links.put(p10BReply, b1Task, "5001")
			args := e.staged(t, "nội dung quan trọng")
			fail(e, errors.New("db down"))
			out := e.comment(b1Thread, reply, args, 1)
			if !out.retry || out.code != "storage" || e.m.count() != 0 {
				t.Fatalf("first attempt: %+v texts %q", out, e.m.texts())
			}
			// Recovery on the retry: the same text drafts.
			fail(e, nil)
			if out := e.comment(b1Thread, reply, args, 2); out.code != "assistant_queued" {
				t.Fatalf("retry: %+v", out)
			}
			if j, ok := e.job(); !ok || j.text != "nội dung quan trọng" {
				t.Fatalf("job %+v", j)
			}
			// The last attempt gives up.
			args = e.staged(t, "lần cuối")
			fail(e, errors.New("db down"))
			out = e.comment(b1Thread, reply, args, e.w.settings.MaxAttempts)
			if out.retry || out.code != "handler_failed" || e.m.last(t).Text != msgFailed {
				t.Fatalf("last attempt: %+v texts %q", out, e.m.texts())
			}
			if e.stillStaged(args) {
				t.Fatal("the text stayed staged after the last attempt")
			}
			e.noWork(t)
		})
	}
}

// R-B-3: the rate limit still applies: a /comment over the limit is refused, never stored as a note.
func TestP10FixB_CommentCommandIsRateLimited(t *testing.T) {
	e := newP10BEnv(1)
	e.w.d.Assistant.RatePerMinute = 3
	var last outcome
	for i := 0; i < 10; i++ {
		last = e.comment(b1Thread, 0, "nội dung "+string(rune('a'+i%26)), 1)
		if last.code == "assistant_rate_limited" || last.code == "assistant_queue_full" {
			break
		}
		select {
		case <-e.w.assistantJobs:
		default:
		}
	}
	if last.code != "assistant_rate_limited" && last.code != "assistant_queue_full" {
		t.Fatalf("never limited: %+v", last)
	}
	if e.stored() != 0 {
		t.Fatalf("a limited /comment became a note: %v", e.c.instructions)
	}
}

// R-B-3: /comment is built in: a behavior cannot take it, a similar name is fine, and /help lists it.
func TestP10FixB_CommentIsReservedAndInHelp(t *testing.T) {
	if !reservedCommands["/comment"] {
		t.Fatal("/comment is not reserved")
	}
	doc := func(cmd string) Doc {
		return Doc{Schema: 1, Name: "d", Triggers: []TriggerDoc{{Type: TriggerTelegramCommand, Command: cmd}},
			Instructions: "x", Actions: map[string][]ActionDoc{string(ClassNotify): {{Type: ActionTelegramMessage}}}}
	}
	found := false
	for _, fe := range Validate(doc("/comment"), nil, nil) {
		if strings.HasSuffix(fe.Path, ".command") && strings.Contains(fe.Msg, "built-in") {
			found = true
		}
	}
	if !found {
		t.Fatal("a behavior was allowed to take /comment")
	}
	for _, ok := range []string{"/comments", "/commentary", "/comment2"} {
		if errs := Validate(doc(ok), nil, nil); len(errs) != 0 {
			t.Errorf("%s rejected: %v", ok, errs)
		}
	}
	e := newP10BEnv(0)
	if help := e.w.helpText(context.Background(), p4WS); !strings.Contains(help, "/comment <nội dung>") {
		t.Fatalf("help lacks /comment:\n%s", help)
	}
}

// R-B-3: /comment never reaches the behavior command trigger (it is handled before any behavior dispatch).
func TestP10FixB_CommentDoesNotReachBehaviorCommands(t *testing.T) {
	e := newP10BEnv(0)
	e.w.d.Commands = nil // a behavior dispatch would panic on a nil handler
	if out := e.comment(b1Thread, 0, "nội dung", 1); out.code != "assistant_queued" {
		t.Fatalf("outcome %+v", out)
	}
}
