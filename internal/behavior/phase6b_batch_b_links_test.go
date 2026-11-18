package behavior

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Phase 6b batch B: jump links (REQ-8). Fakes only.

// TC-B1-20 (REQ-8): task reference forms.
func TestPhase6bB1_ParseTaskRefForms(t *testing.T) {
	good := map[string]string{
		"86abc":                                      "86abc",
		"  86abc  ":                                  "86abc",
		"https://app.clickup.com/t/86abc":            "86abc",
		"https://app.clickup.com/t/86abc/":           "86abc",
		"https://APP.clickup.com/t/86abc":            "86abc",
		"https://app.clickup.com/t/9012345/ABC-123":  "ABC-123",
		"https://app.clickup.com/t/86abc?comment=12": "86abc",
		"https://app.clickup.com/t/86abc#frag":       "86abc",
	}
	for in, want := range good {
		if got, ok := ParseTaskRef(in); !ok || got != want {
			t.Errorf("ParseTaskRef(%q)=(%q,%v) want %q", in, got, ok, want)
		}
	}
	bad := []string{
		"", " ", "http://app.clickup.com/t/86abc", "https://app.clickup.com.evil.example/t/86abc",
		"https://evil.example/t/86abc", "https://user@app.clickup.com/t/86abc", "https://app.clickup.com:8443/t/86abc",
		"https://app.clickup.com/t/", "https://app.clickup.com/t", "https://app.clickup.com/x/86abc",
		"https://app.clickup.com/t/12/34/56", "https://app.clickup.com/t/abc/86abc", "https://app.clickup.com/t/86a bc",
		"https://app.clickup.com/t/../86abc", "-bad", "a b", "../x", strings.Repeat("a", 65),
		"ftp://app.clickup.com/t/86abc", "https://app.clickup.com/t/%00",
	}
	for _, in := range bad {
		if got, ok := ParseTaskRef(in); ok {
			t.Errorf("ParseTaskRef(%q)=(%q,true), want false", in, got)
		}
	}
}

// TC-B1-21 (REQ-8): taskURLRef takes exactly one URL, never free text.
func TestPhase6bB1_TaskURLRefOnlyWholeURL(t *testing.T) {
	for _, in := range []string{"https://app.clickup.com/t/86abc", "<https://app.clickup.com/t/86abc>", "  https://app.clickup.com/t/86abc\n"} {
		if id, ok := taskURLRef(in); !ok || id != "86abc" {
			t.Errorf("taskURLRef(%q)=(%q,%v)", in, id, ok)
		}
	}
	for _, in := range []string{
		"86abc", "xem https://app.clickup.com/t/86abc giúp", "https://app.clickup.com/t/86abc giúp anh",
		"https://app.clickup.com/t/86abc https://app.clickup.com/t/86abd", "https://app.clickup.com/t/86abc\nthêm test",
		"http://app.clickup.com/t/86abc", "",
	} {
		if id, ok := taskURLRef(in); ok {
			t.Errorf("taskURLRef(%q)=(%q,true), want false", in, id)
		}
	}
}

// TC-B1-22 (REQ-8): deep links exist only for supergroup chat ids.
func TestPhase6bB1_TopicDeepLink(t *testing.T) {
	cases := []struct {
		chat, thread int64
		want         string
		ok           bool
	}{
		{-1001234567890, 55, "https://t.me/c/1234567890/55", true},
		{-1000000000001, 2, "https://t.me/c/1/2", true},
		{-100500, 55, "", false},        // a basic group / short id
		{-1000000000000, 55, "", false}, // not a supergroup id
		{500, 55, "", false},            // a private chat
		{0, 55, "", false},
		{-1001234567890, 0, "", false}, // General
		{-1001234567890, -4, "", false},
	}
	for _, c := range cases {
		got, ok := TopicDeepLink(c.chat, c.thread)
		if ok != c.ok || got != c.want {
			t.Errorf("TopicDeepLink(%d,%d)=(%q,%v) want (%q,%v)", c.chat, c.thread, got, ok, c.want, c.ok)
		}
	}
}

// b1LinkEnv is a worker whose Links and Topics are a real TopicRouter on the
// batch A fakes.
type b1LinkEnv struct {
	*b1Env
	store  *p6bStore
	client *p6bClient
	router *TopicRouter
	names  map[string]error
	reads  int
	mu     sync.Mutex
}

func newB1LinkEnv(t *testing.T) *b1LinkEnv {
	t.Helper()
	e := &b1LinkEnv{b1Env: newB1Env(b1States(0)), store: newP6bStore(), client: &p6bClient{}, names: map[string]error{}}
	var err error
	e.router, err = NewTopicRouter(TopicRouterDeps{
		Store: e.store, Client: e.client,
		TaskName: func(_ context.Context, _, _, task string) (string, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.reads++
			return "private title", e.names[task]
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.w.d.Links, e.w.d.Topics = e.router, e.router
	return e
}

func (e *b1LinkEnv) topicRows(ws string) []Topic {
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	var out []Topic
	for _, r := range e.store.rows {
		if r.WorkspaceID == ws {
			out = append(out, *r)
		}
	}
	return out
}

func (e *b1LinkEnv) topicCmd(args string) outcome {
	u := e.update(0, 0)
	u.Kind, u.Command, u.Args = UpdateCommand, "/topic", args
	return e.w.topicCommand(context.Background(), u, e.to(0))
}

func (e *b1LinkEnv) lastButtons(t *testing.T) []Button {
	t.Helper()
	msg := e.m.last(t)
	var out []Button
	for _, row := range msg.Buttons {
		out = append(out, row...)
	}
	return out
}

// TC-B1-23 (REQ-8): a ClickUp task URL pasted in General answers with one
// "Mở topic" URL button; the topic is created once and reused.
func TestPhase6bB1_PastedTaskLinkInGeneral(t *testing.T) {
	e := newB1LinkEnv(t)
	out := e.say(t, 0, 0, "https://app.clickup.com/t/86abc")
	if out.code != "" {
		t.Fatalf("code=%q texts=%q", out.code, e.m.texts())
	}
	btns := e.lastButtons(t)
	if len(btns) != 1 || btns[0].Text != "Mở topic" || btns[0].Data != "" || btns[0].URL != "https://t.me/c/1234567890/101" {
		t.Fatalf("buttons=%+v", btns)
	}
	if got := e.m.last(t).Text; got != "Task 86abc: nhấn nút để mở topic." {
		t.Fatalf("text=%q", got)
	}
	if _, queued := e.job(); queued {
		t.Fatal("a pasted link also queued the assistant")
	}
	// Second paste reuses the topic.
	e.say(t, 0, 0, "<https://app.clickup.com/t/9012/86abc>")
	if got := e.lastButtons(t); len(got) != 1 || got[0].URL != btns[0].URL || e.client.createCount() != 1 {
		t.Fatalf("second paste: %+v creates=%d", got, e.client.createCount())
	}
	// /topic with a bare id and with a URL.
	for _, args := range []string{"86abc", "https://app.clickup.com/t/86abc"} {
		if out := e.topicCmd(args); out.code != "" || e.lastButtons(t)[0].URL != btns[0].URL {
			t.Fatalf("/topic %s: %+v", args, out)
		}
	}
	// A closed topic is reopened by the jump.
	for _, r := range e.topicRows(p4WS) {
		e.store.SetTopicState(context.Background(), p4WS, r.ID, TopicClosed)
	}
	e.say(t, 0, 0, "https://app.clickup.com/t/86abc")
	if e.client.reopens != 1 || e.lastButtons(t)[0].URL != btns[0].URL {
		t.Fatalf("reopens=%d buttons=%+v", e.client.reopens, e.lastButtons(t))
	}
	// The stored topic carries ids only: no title.
	for _, r := range e.topicRows(p4WS) {
		if strings.Contains(fmt.Sprint(r), "private title") {
			t.Fatalf("row carries a title: %+v", r)
		}
	}
}

// TC-B1-24 (REQ-8, human): text that only contains a link, or a link inside a
// topic, is a normal message.
func TestPhase6bB1_LinkDoesNotTriggerElsewhere(t *testing.T) {
	for name, tc := range map[string]struct {
		thread int64
		text   string
	}{
		"inside a topic":        {b1Thread, "https://app.clickup.com/t/86abd"},
		"inside longer text":    {0, "xem giúp https://app.clickup.com/t/86abc nhé"},
		"link then words":       {0, "https://app.clickup.com/t/86abc nhé"},
		"two links":             {0, "https://app.clickup.com/t/86abc https://app.clickup.com/t/86abd"},
		"look-alike host":       {0, "https://app.clickup.com.evil.example/t/86abc"},
		"bare id is not a link": {0, "86abc"},
		"plain http":            {0, "http://app.clickup.com/t/86abc"},
		"unrelated URL":         {0, "https://example.com/t/86abc"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newB1LinkEnv(t)
			out := e.say(t, tc.thread, 0, tc.text)
			if out.code != "assistant_queued" {
				t.Fatalf("out=%+v texts=%q", out, e.m.texts())
			}
			if e.m.count() != 0 || e.client.createCount() != 0 || len(e.topicRows(p4WS)) != 0 {
				t.Fatalf("sent=%d creates=%d rows=%d", e.m.count(), e.client.createCount(), len(e.topicRows(p4WS)))
			}
		})
	}
	t.Run("links are off without Links", func(t *testing.T) {
		e := newB1LinkEnv(t)
		e.w.d.Links = nil
		if out := e.say(t, 0, 0, "https://app.clickup.com/t/86abc"); out.code != "assistant_queued" {
			t.Fatalf("out=%+v", out)
		}
		if out := e.topicCmd("86abc"); out.code != "not_available" || e.m.last(t).Text != "Chưa bật tính năng topic." {
			t.Fatalf("out=%+v text=%q", out, e.m.last(t).Text)
		}
	})
}

// TC-B1-25 (REQ-8): failures answer with a short fixed Vietnamese message,
// create nothing and leak nothing.
func TestPhase6bB1_JumpFailures(t *testing.T) {
	t.Run("unreadable task", func(t *testing.T) {
		e := newB1LinkEnv(t)
		e.names["86abc"] = fmt.Errorf("token secret-123 rejected")
		out := e.say(t, 0, 0, "https://app.clickup.com/t/86abc")
		if out.code != "task_unreadable" || e.m.last(t).Text != "Không đọc được task này." || len(e.m.last(t).Buttons) != 0 {
			t.Fatalf("out=%+v msg=%+v", out, e.m.last(t))
		}
		if e.client.createCount() != 0 || len(e.topicRows(p4WS)) != 0 {
			t.Fatal("a topic was created for an unreadable task")
		}
		if strings.Contains(e.m.last(t).Text, "secret-123") {
			t.Fatal("error text leaked")
		}
	})
	t.Run("chat is not a forum", func(t *testing.T) {
		e := newB1LinkEnv(t)
		e.client.createErr = p6bErr("topic_not_forum")
		out := e.topicCmd("86abc")
		if out.code != "topic_unavailable" || !strings.HasPrefix(e.m.last(t).Text, "Không tạo được liên kết") || len(e.m.last(t).Buttons) != 0 {
			t.Fatalf("out=%+v msg=%+v", out, e.m.last(t))
		}
	})
	t.Run("bot lacks the right", func(t *testing.T) {
		e := newB1LinkEnv(t)
		e.client.createErr = p6bErr("topic_no_rights")
		if out := e.topicCmd("86abc"); out.code != "topic_unavailable" {
			t.Fatalf("out=%+v", out)
		}
	})
	t.Run("chat id is not a supergroup id", func(t *testing.T) {
		e := newB1LinkEnv(t)
		u := e.update(0, 0)
		u.ChatID, u.Kind, u.Command, u.Args = -100500, UpdateCommand, "/topic", "86abc"
		out := e.w.topicCommand(context.Background(), u, ChatRef{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: -100500})
		if out.code != "topic_unavailable" || len(e.m.last(t).Buttons) != 0 {
			t.Fatalf("out=%+v msg=%+v", out, e.m.last(t))
		}
	})
	t.Run("usage", func(t *testing.T) {
		e := newB1LinkEnv(t)
		for _, args := range []string{"", "   ", "hai từ", "https://evil.example/t/86abc", "../x"} {
			if out := e.topicCmd(args); out.code != "usage" || e.m.last(t).Text != "Cú pháp: /topic <task id hoặc link ClickUp>" {
				t.Fatalf("args=%q out=%+v text=%q", args, out, e.m.last(t).Text)
			}
		}
		if e.client.createCount() != 0 {
			t.Fatal("a bad /topic created a topic")
		}
	})
	t.Run("send failure is a failed reply, not a panic", func(t *testing.T) {
		e := newB1LinkEnv(t)
		e.m.sendErr = fmt.Errorf("boom")
		if out := e.topicCmd("86abc"); out.code != "reply_failed" {
			t.Fatalf("out=%+v", out)
		}
	})
}

// TC-B1-26 (REQ-8, isolation): the same task in another workspace or chat gets
// its own topic and link; one workspace never reads another's topic.
func TestPhase6bB1_JumpIsolation(t *testing.T) {
	e := newB1LinkEnv(t)
	e.topicCmd("86abc")
	first := e.lastButtons(t)[0].URL
	// Another workspace, same integration id and chat.
	u := e.update(0, 0)
	u.WorkspaceID = b1WSAlt
	b := ChatRef{WorkspaceID: b1WSAlt, IntegrationID: b1Bot, ChatID: b1Chat}
	link, err := e.router.TaskTopicLink(context.Background(), b, "86abc")
	if err != nil || link == first {
		t.Fatalf("other workspace link=%q err=%v first=%q", link, err, first)
	}
	if len(e.topicRows(p4WS)) != 1 || len(e.topicRows(b1WSAlt)) != 1 || e.client.createCount() != 2 {
		t.Fatalf("rows=%d/%d creates=%d", len(e.topicRows(p4WS)), len(e.topicRows(b1WSAlt)), e.client.createCount())
	}
	// ExistingTopicLink is scoped too.
	if _, ok := e.router.ExistingTopicLink(context.Background(), ChatRef{WorkspaceID: "ws-none", IntegrationID: b1Bot, ChatID: b1Chat}, TopicSubjectClickUpTask, "86abc"); ok {
		t.Fatal("a workspace without the topic got a link")
	}
	if _, ok := e.router.ExistingTopicLink(context.Background(), ChatRef{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: -1009999999999}, TopicSubjectClickUpTask, "86abc"); ok {
		t.Fatal("another chat got a link")
	}
	// The topic's thread id in a base with a thread is ignored: General topic.
	b.WorkspaceID, b.ThreadID = p4WS, 999
	if l2, err := e.router.TaskTopicLink(context.Background(), b, "86abc"); err != nil || l2 != first {
		t.Fatalf("base thread leaked into the link: %q %v", l2, err)
	}
	// Invalid ids never reach the store.
	for _, id := range []string{"", "a b", "../x"} {
		if _, err := e.router.TaskTopicLink(context.Background(), b, id); err != ErrTopicUnavailable {
			t.Fatalf("id %q err=%v", id, err)
		}
	}
	var nilRouter *TopicRouter
	if _, err := nilRouter.TaskTopicLink(context.Background(), b, "86abc"); err != ErrTopicUnavailable {
		t.Fatalf("nil router err=%v", err)
	}
	if _, ok := nilRouter.ExistingTopicLink(context.Background(), b, TopicSubjectClickUpTask, "86abc"); ok {
		t.Fatal("nil router has links")
	}
	if _, ok := nilRouter.JumpButton(context.Background(), b, TopicIntent{}); ok {
		t.Fatal("nil router has a jump button")
	}
}

// --- deliver: fallback button ---

type b1Delivered struct {
	to  ChatRef
	msg OutMessage
}

type b1Recorder struct {
	Messenger
	mu   sync.Mutex
	sent []b1Delivered
	fail func(to ChatRef) error
}

func (m *b1Recorder) SendMessage(_ context.Context, to ChatRef, msg OutMessage) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		if err := m.fail(to); err != nil {
			return 0, err
		}
	}
	m.sent = append(m.sent, b1Delivered{to: to, msg: msg})
	return int64(900 + len(m.sent)), nil
}

func (m *b1Recorder) last(t *testing.T) b1Delivered {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sent) == 0 {
		t.Fatal("nothing delivered")
	}
	return m.sent[len(m.sent)-1]
}

type b1DeliverEnv struct {
	store  *p6bStore
	client *p6bClient
	router *TopicRouter
	rec    *b1Recorder
	eng    *Engine
	chat   ChatRef
}

func newB1DeliverEnv(t *testing.T, chatID int64) *b1DeliverEnv {
	t.Helper()
	e := &b1DeliverEnv{store: newP6bStore(), client: &p6bClient{}, rec: &b1Recorder{}}
	e.chat = ChatRef{WorkspaceID: p6bWS, IntegrationID: p6bBot, ChatID: chatID}
	var err error
	e.router, err = NewTopicRouter(TopicRouterDeps{Store: e.store, Client: e.client})
	if err != nil {
		t.Fatal(err)
	}
	e.eng = &Engine{d: ActionDeps{Messenger: e.rec, Topics: e.router}, s: ActionSettings{RequestTimeout: time.Second}, now: time.Now}
	return e
}

func (e *b1DeliverEnv) route(mode, task string) ChatRef {
	c := e.chat
	c.topic = &TopicIntent{Mode: mode, TaskID: task, ExecutionID: 0}
	return c
}

// closeAndBlock marks the stored topic closed and makes the reopen fail.
func (e *b1DeliverEnv) closeAndBlock(code string) {
	e.store.mu.Lock()
	for _, r := range e.store.rows {
		r.State = TopicClosed
	}
	e.store.mu.Unlock()
	e.client.reopenErr = p6bErr(code)
}

// TC-B1-27 (REQ-8): a report that falls back to General although its topic
// exists carries the jump button; nothing else does.
func TestPhase6bB1_FallbackJumpButtonOnDeliver(t *testing.T) {
	const supergroup = int64(-1001234567890)
	ctx := context.Background()
	t.Run("delivered in the topic: no button", func(t *testing.T) {
		e := newB1DeliverEnv(t, supergroup)
		_, to, err := e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "xong"})
		if err != nil || to.ThreadID == 0 || len(e.rec.last(t).msg.Buttons) != 0 {
			t.Fatalf("to=%+v err=%v msg=%+v", to, err, e.rec.last(t).msg)
		}
	})
	t.Run("topic exists but cannot be reopened: General plus button", func(t *testing.T) {
		e := newB1DeliverEnv(t, supergroup)
		_, first, _ := e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "một"})
		e.closeAndBlock("topic_no_rights")
		_, to, err := e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "hai", Buttons: [][]Button{{{Text: "Làm", Data: "x"}}}})
		got := e.rec.last(t)
		want := fmt.Sprintf("https://t.me/c/1234567890/%d", first.ThreadID)
		if err != nil || to.ThreadID != 0 || len(got.msg.Buttons) != 2 {
			t.Fatalf("to=%+v err=%v buttons=%+v", to, err, got.msg.Buttons)
		}
		if got.msg.Buttons[0][0].Data != "x" || got.msg.Buttons[1][0] != (Button{Text: "Mở topic", URL: want}) {
			t.Fatalf("buttons=%+v want jump %q last", got.msg.Buttons, want)
		}
	})
	t.Run("the existing buttons slice is not modified", func(t *testing.T) {
		e := newB1DeliverEnv(t, supergroup)
		e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "một"})
		e.closeAndBlock("topic_no_rights")
		rows := make([][]Button, 1, 4)
		rows[0] = []Button{{Text: "Làm", Data: "x"}}
		e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "hai", Buttons: rows})
		if len(rows) != 1 || len(rows[:2]) != 2 || rows[:2][1] != nil {
			t.Fatalf("caller's rows were written into: %+v", rows[:2])
		}
	})
	t.Run("no topic exists yet: no button", func(t *testing.T) {
		e := newB1DeliverEnv(t, supergroup)
		e.client.createErr = p6bErr("topic_not_forum")
		_, to, err := e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "xong"})
		if err != nil || to.ThreadID != 0 || len(e.rec.last(t).msg.Buttons) != 0 {
			t.Fatalf("to=%+v err=%v msg=%+v", to, err, e.rec.last(t).msg)
		}
	})
	t.Run("general mode with a task: task-topic callback button, no URL button", func(t *testing.T) {
		e := newB1DeliverEnv(t, supergroup)
		e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "một"})
		e.closeAndBlock("topic_no_rights")
		e.eng.deliver(ctx, e.route(ReportTopicGeneral, "86abc"), OutMessage{Text: "chung"})
		got := e.rec.last(t).msg.Buttons
		want := [][]Button{{{Text: "📌 Mở topic task", Data: "tt:86abc"}}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("general mode buttons=%+v want %+v", got, want)
		}
		for _, row := range got {
			for _, b := range row {
				if b.URL != "" {
					t.Fatalf("unexpected URL button %+v", b)
				}
			}
		}
	})
	t.Run("general mode with no task, and no-subject send: no button", func(t *testing.T) {
		e := newB1DeliverEnv(t, supergroup)
		e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "một"})
		e.closeAndBlock("topic_no_rights")
		e.eng.deliver(ctx, e.route(ReportTopicGeneral, ""), OutMessage{Text: "chung"})
		if len(e.rec.last(t).msg.Buttons) != 0 {
			t.Fatalf("general mode without task got buttons %+v", e.rec.last(t).msg.Buttons)
		}
		e.eng.deliver(ctx, e.route(ReportTopicTask, ""), OutMessage{Text: "không task"})
		if len(e.rec.last(t).msg.Buttons) != 0 {
			t.Fatalf("no-subject send got buttons %+v", e.rec.last(t).msg.Buttons)
		}
	})
	t.Run("force reply is left alone", func(t *testing.T) {
		e := newB1DeliverEnv(t, supergroup)
		e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "một"})
		e.closeAndBlock("topic_no_rights")
		e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "hỏi", ForceReply: true})
		if m := e.rec.last(t).msg; len(m.Buttons) != 0 || !m.ForceReply {
			t.Fatalf("force reply message=%+v", m)
		}
	})
	t.Run("a message that already has the link is not given a second", func(t *testing.T) {
		e := newB1DeliverEnv(t, supergroup)
		_, first, _ := e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "một"})
		e.closeAndBlock("topic_no_rights")
		link, _ := TopicDeepLink(supergroup, first.ThreadID)
		e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "hai", Buttons: [][]Button{{{Text: "Mở", URL: link}}}})
		if m := e.rec.last(t).msg; len(m.Buttons) != 1 {
			t.Fatalf("buttons=%+v", m.Buttons)
		}
	})
	t.Run("not a supergroup id: no button", func(t *testing.T) {
		e := newB1DeliverEnv(t, -100500)
		e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "một"})
		e.closeAndBlock("topic_no_rights")
		e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "hai"})
		if m := e.rec.last(t).msg; len(m.Buttons) != 0 {
			t.Fatalf("buttons=%+v", m.Buttons)
		}
	})
	t.Run("a topic that vanished: retry in General carries the button once", func(t *testing.T) {
		e := newB1DeliverEnv(t, supergroup)
		e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "một"})
		// The topic is open in the store, but Telegram says the thread is gone
		// and also refuses a new topic: the retry goes to General.
		e.rec.fail = func(to ChatRef) error {
			if to.ThreadID != 0 {
				return p6bErr("thread_not_found")
			}
			return nil
		}
		e.client.createErr = p6bErr("topic_no_rights")
		_, to, err := e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "hai"})
		if err != nil || to.ThreadID != 0 {
			t.Fatalf("to=%+v err=%v", to, err)
		}
		if m := e.rec.last(t).msg; len(m.Buttons) > 1 {
			t.Fatalf("buttons=%+v", m.Buttons)
		}
	})
}

// --- /list jump rows ---

type b1Lister struct {
	*phase6BConversations
	resources map[int64][]workflow.ResourceBinding
	err       map[int64]error
}

func (l *b1Lister) ListResources(_ context.Context, _ string, id int64) ([]workflow.ResourceBinding, error) {
	if err := l.err[id]; err != nil {
		return nil, err
	}
	return l.resources[id], nil
}

// TC-B1-28 (REQ-8): /list gets one "WF-n · Mở topic" row per topic, deduped,
// at most 10, only for tasks that have a topic in this chat.
func TestPhase6bB1_ListJumpRows(t *testing.T) {
	e := newB1LinkEnv(t)
	states := map[int64]workflow.State{}
	res := map[int64][]workflow.ResourceBinding{}
	var views []workflow.ExecutionView
	add := func(id int64, resources ...workflow.ResourceBinding) {
		states[id] = workflow.StateRunning
		res[id] = resources
		views = append(views, workflow.ExecutionView{Execution: workflow.Execution{ID: id, WorkspaceID: p4WS, State: workflow.StateRunning}, BehaviorName: "review"})
	}
	task := func(id string) workflow.ResourceBinding {
		return workflow.ResourceBinding{Kind: "clickup.task", ExternalID: id}
	}
	// WF-30 and WF-29 share task T1; WF-28 has no topic; WF-27 has a PR only;
	// WF-26 has an invalid external id; WF-25..14 have 12 distinct topics.
	for _, id := range []int64{30, 29} {
		add(id, task("T1"))
	}
	add(28, task("NOTOPIC"))
	add(27, workflow.ResourceBinding{Kind: "github.pr", ExternalID: "T1"})
	add(26, task("bad id"))
	for i := int64(25); i >= 14; i-- {
		add(i, task(fmt.Sprintf("K%d", i)))
	}
	seed := func(taskID string, thread int64) {
		topic, _, err := e.store.EnsureTopic(context.Background(), TopicKey{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: b1Chat, SubjectKind: TopicSubjectClickUpTask, SubjectKey: taskID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.store.SetTopicThread(context.Background(), p4WS, topic.ID, thread, topic.ReservedAt); err != nil {
			t.Fatal(err)
		}
	}
	seed("T1", 501)
	for i := int64(25); i >= 14; i-- {
		seed(fmt.Sprintf("K%d", i), 600+i)
	}
	e.r.list = views
	for id, st := range states {
		e.r.details[id] = phase6BDetail(id, p4WS, st)
	}
	e.w.d.Conversations = &b1Lister{phase6BConversations: e.c, resources: res}
	if out := e.cmd(0, 0, "/list", ""); out.code != "" {
		t.Fatalf("code=%q", out.code)
	}
	var rows []string
	for _, row := range e.m.last(t).Buttons {
		if len(row) != 1 || row[0].Data != "" {
			t.Fatalf("row=%+v", row)
		}
		rows = append(rows, row[0].Text+" "+row[0].URL)
	}
	if len(rows) != 10 {
		t.Fatalf("rows=%d: %q", len(rows), rows)
	}
	if rows[0] != "WF-30 · Mở topic https://t.me/c/1234567890/501" {
		t.Fatalf("first row=%q", rows[0])
	}
	for _, r := range rows {
		if strings.Contains(r, "WF-29") || strings.Contains(r, "WF-28") || strings.Contains(r, "WF-27") || strings.Contains(r, "WF-26") {
			t.Fatalf("unexpected row %q", r)
		}
	}
	// The text list is unchanged and has no link text.
	if txt := e.m.last(t).Text; !strings.Contains(txt, "WF-30 · review") || strings.Contains(txt, "t.me") {
		t.Fatalf("text=%q", txt)
	}
	// A lister error for one execution skips it, a Links-less worker sends none.
	e.w.d.Conversations = &b1Lister{phase6BConversations: e.c, resources: res, err: map[int64]error{30: fmt.Errorf("db")}}
	e.cmd(0, 0, "/list", "")
	if got := e.m.last(t).Buttons; len(got) == 0 || !strings.HasPrefix(got[0][0].Text, "WF-25 ") && !strings.HasPrefix(got[0][0].Text, "WF-29 ") {
		t.Fatalf("rows after error=%+v", got)
	}
	e.w.d.Links = nil
	e.cmd(0, 0, "/list", "")
	if len(e.m.last(t).Buttons) != 0 {
		t.Fatal("buttons without Links")
	}
}
