package behavior

import (
	"bytes"
	"context"
	"errors"
	"log"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/relay"
)

// Test debt TD-TT-1..5 (hotfix task topic names). Fakes only.

// ttStore is the p6b topic store plus the link claim of the SQL store.
type ttStore struct {
	*p6bStore
	mu       sync.Mutex
	link     map[int64]int64 // absent: never posted (NULL)
	claimErr error
	claims   int
	releases int
	listErr  error
}

func (s *ttStore) ListTopics(context.Context, string) ([]Topic, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	s.p6bStore.mu.Lock()
	defer s.p6bStore.mu.Unlock()
	var out []Topic
	for _, r := range s.p6bStore.rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *ttStore) ClaimTopicLink(_ context.Context, _ string, id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	if s.claimErr != nil {
		return false, s.claimErr
	}
	if _, ok := s.link[id]; ok {
		return false, nil
	}
	s.link[id] = 0
	return true, nil
}

func (s *ttStore) SetTopicLink(_ context.Context, _ string, id, msg int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.link[id]; !ok || v != 0 {
		return ErrConflict
	}
	s.link[id] = msg
	return nil
}

func (s *ttStore) ReleaseTopicLink(_ context.Context, _ string, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases++
	if v, ok := s.link[id]; ok && v == 0 {
		delete(s.link, id)
	}
	return nil
}

func (s *ttStore) linkState(id int64) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.link[id]
	return v, ok
}

type ttSent struct {
	to  ChatRef
	msg OutMessage
}

// ttClient is the p6b forum client that can also post, pin and rename.
type ttClient struct {
	*p6bClient
	mu        sync.Mutex
	sent      []ttSent
	pins      []int64
	renames   map[int64]string
	sendErr   error
	pinErr    error
	renameErr map[int64]error
	nextMsg   int64
}

func (c *ttClient) SendMessage(_ context.Context, to ChatRef, m OutMessage) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sendErr != nil {
		return 0, c.sendErr
	}
	c.nextMsg++
	c.sent = append(c.sent, ttSent{to, m})
	return 700 + c.nextMsg, nil
}

func (c *ttClient) PinMessage(_ context.Context, _ ChatRef, id int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pinErr != nil {
		return c.pinErr
	}
	c.pins = append(c.pins, id)
	return nil
}

func (c *ttClient) RenameTopic(_ context.Context, _ ChatRef, thread int64, name string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.renameErr[thread]; err != nil {
		return false, err
	}
	if _, ok := c.renames[thread]; !ok {
		c.p6bClient.mu.Lock()
		if i := int(thread - 101); i >= 0 && i < len(c.p6bClient.names) {
			c.renames[thread] = c.p6bClient.names[i] // the name it was created with
		}
		c.p6bClient.mu.Unlock()
	}
	if c.renames[thread] == name {
		return false, nil
	}
	c.renames[thread] = name
	return true, nil
}

func (c *ttClient) sentCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

func (c *ttClient) names0() []string {
	c.p6bClient.mu.Lock()
	defer c.p6bClient.mu.Unlock()
	return append([]string(nil), c.p6bClient.names...)
}

type ttEnv struct {
	st     *ttStore
	cl     *ttClient
	router *TopicRouter
	eng    *Engine
	rec    *b1Recorder
	chat   ChatRef
	infos  map[string]TaskTopicInfo
	unread map[string]bool
}

func newTT(t *testing.T) *ttEnv {
	t.Helper()
	e := &ttEnv{
		st:     &ttStore{p6bStore: newP6bStore(), link: map[int64]int64{}},
		cl:     &ttClient{p6bClient: &p6bClient{}, renames: map[int64]string{}, renameErr: map[int64]error{}},
		rec:    &b1Recorder{},
		infos:  map[string]TaskTopicInfo{},
		unread: map[string]bool{},
	}
	e.chat = ChatRef{WorkspaceID: p6bWS, IntegrationID: p6bBot, ChatID: -1001234567890}
	var err error
	e.router, err = NewTopicRouter(TopicRouterDeps{Store: e.st, Client: e.cl,
		TaskInfo: func(_ context.Context, _, _, id string) (TaskTopicInfo, error) {
			if e.unread[id] {
				return TaskTopicInfo{}, errors.New("clickup 500 token=SECRET-TOKEN")
			}
			return e.infos[id], nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	e.eng = &Engine{d: ActionDeps{Messenger: e.rec, Topics: e.router}, s: ActionSettings{RequestTimeout: time.Second}, now: time.Now}
	return e
}

func (e *ttEnv) route(task string) ChatRef {
	c := e.chat
	c.topic = &TopicIntent{Mode: ReportTopicTask, TaskID: task}
	return c
}

func (e *ttEnv) notice(t *testing.T, task, text string) ChatRef {
	t.Helper()
	_, to, err := e.eng.deliver(context.Background(), e.route(task), OutMessage{Text: text})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	return to
}

// TC-TT-1 (TD-TT-1, REQ-1): topic names for every task shape.
func TestTD_TT1_TopicNameShapes(t *testing.T) {
	long := strings.Repeat("đ", 100) // 200 bytes
	cases := []struct {
		name   string
		info   TaskTopicInfo
		ok     bool
		want   string
		prefix string
	}{
		{"custom id and title", TaskTopicInfo{Title: "M01 Login", CustomID: "FC-6583"}, true, "FC-6583 · M01 Login", ""},
		{"no custom id keeps the task id", TaskTopicInfo{Title: "M01 Login"}, true, "86abc · M01 Login", ""},
		{"unreadable task keeps the task id only", TaskTopicInfo{Title: "ignored", CustomID: "FC-1"}, false, "86abc", ""},
		{"empty title gives the label only", TaskTopicInfo{CustomID: "FC-6583"}, true, "FC-6583", ""},
		{"blank title and no custom id", TaskTopicInfo{Title: " \n\t"}, true, "86abc", ""},
		{"invalid custom id is ignored", TaskTopicInfo{Title: "T", CustomID: "bad id!"}, true, "86abc · T", ""},
		{"custom id with slash is ignored", TaskTopicInfo{Title: "T", CustomID: "FC-1/../x"}, true, "86abc · T", ""},
		{"newline and tabs in the title", TaskTopicInfo{Title: "a\nb\tc  d", CustomID: "FC-2"}, true, "FC-2 · a b c d", ""},
		{"control characters", TaskTopicInfo{Title: "x\x00y\x1bz", CustomID: "FC-2"}, true, "FC-2 · x y z", ""},
		{"vietnamese title", TaskTopicInfo{Title: "Đăng nhập thất bại", CustomID: "FC-3"}, true, "FC-3 · Đăng nhập thất bại", ""},
		{"emoji title", TaskTopicInfo{Title: "Fix 🐛 now", CustomID: "FC-3"}, true, "FC-3 · Fix 🐛 now", ""},
		{"multibyte title is cut on a rune boundary", TaskTopicInfo{Title: long, CustomID: "FC-6583"}, true, "", "FC-6583 · đ"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TaskTopicName("86abc", c.info, c.ok)
			if c.want != "" && got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
			if c.prefix != "" && !strings.HasPrefix(got, c.prefix) {
				t.Fatalf("got %q want prefix %q", got, c.prefix)
			}
			if len(got) > MaxTopicNameBytes || !utf8.ValidString(got) || got != strings.TrimSpace(got) || got == "" {
				t.Fatalf("bad name %q (%d bytes)", got, len(got))
			}
		})
	}
	// Every title length around the 128-byte limit, with 1-, 2-, 3- and 4-byte runes.
	for _, r := range []string{"a", "đ", "ệ", "日", "🐛"} {
		for n := 1; n <= 140; n++ {
			title := strings.Repeat(r, n)
			if !utf8.ValidString(title) {
				continue
			}
			got := TaskTopicName("86abc", TaskTopicInfo{Title: title, CustomID: "FC-6583"}, true)
			if len(got) > MaxTopicNameBytes || !utf8.ValidString(got) || !strings.HasPrefix(got, "FC-6583") || got != strings.TrimSpace(got) {
				t.Fatalf("rune %q x %d: %q (%d bytes)", r, n, got, len(got))
			}
		}
	}
	// A title that is all spaces after the cut keeps the label, not a dangling separator.
	got := TaskTopicName(strings.Repeat("a", 127), TaskTopicInfo{Title: "title"}, true)
	if len(got) > MaxTopicNameBytes || strings.HasSuffix(got, "·") {
		t.Fatalf("dangling separator: %q", got)
	}
}

// TC-TT-1b (TD-TT-1, REQ-1): the created topic carries the name; unreadable tasks and the title-only reader still work.
func TestTD_TT1_CreatedTopicName(t *testing.T) {
	e := newTT(t)
	e.infos["86abc"] = TaskTopicInfo{Title: "M01 Login", CustomID: "FC-6583", URL: "https://app.clickup.com/t/9/FC-6583"}
	e.unread["86gone"] = true
	e.notice(t, "86abc", "một")
	e.notice(t, "86gone", "hai")
	names := e.cl.names0()
	if len(names) != 2 || names[0] != "FC-6583 · M01 Login" || names[1] != "86gone" {
		t.Fatalf("names %q", names)
	}
	// Only the old title reader wired: "<task id> · <title>".
	st := &ttStore{p6bStore: newP6bStore(), link: map[int64]int64{}}
	cl := &ttClient{p6bClient: &p6bClient{}, renames: map[int64]string{}, renameErr: map[int64]error{}}
	r, err := NewTopicRouter(TopicRouterDeps{Store: st, Client: cl,
		TaskName: func(context.Context, string, string, string) (string, error) { return "Old title", nil }})
	if err != nil {
		t.Fatal(err)
	}
	eng := &Engine{d: ActionDeps{Messenger: &b1Recorder{}, Topics: r}, s: ActionSettings{RequestTimeout: time.Second}, now: time.Now}
	if _, _, err := eng.deliver(context.Background(), e.route("86abc"), OutMessage{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if n := cl.names0(); len(n) != 1 || n[0] != "86abc · Old title" {
		t.Fatalf("names %q", n)
	}
	// The topic name never reaches the log.
	buf := captureLog(t)
	e2 := newTT(t)
	e2.infos["86abc"] = TaskTopicInfo{Title: "Private payroll title", CustomID: "FC-1"}
	e2.cl.createErr = p6bErr("topic_no_rights")
	e2.notice(t, "86abc", "x")
	if strings.Contains(buf.String(), "payroll") {
		t.Fatalf("title logged: %s", buf.String())
	}
}

// TC-TT-2 (TD-TT-2, REQ-2): the link message is posted once, with the button, and pinned.
func TestTD_TT2_LinkPostedOnceAndPinned(t *testing.T) {
	e := newTT(t)
	e.infos["86abc"] = TaskTopicInfo{Title: "M01 Login", CustomID: "FC-6583", URL: "https://app.clickup.com/t/9008218621/FC-6583"}
	to := e.notice(t, "86abc", "một")
	if to.ThreadID == 0 {
		t.Fatal("not delivered in the topic")
	}
	if e.cl.sentCount() != 1 {
		t.Fatalf("link messages %d", e.cl.sentCount())
	}
	m := e.cl.sent[0]
	if m.msg.Text != "FC-6583 · M01 Login" || len(m.msg.Buttons) != 1 || len(m.msg.Buttons[0]) != 1 ||
		m.msg.Buttons[0][0].Text != "Mở task ClickUp" || m.msg.Buttons[0][0].URL != "https://app.clickup.com/t/9008218621/FC-6583" {
		t.Fatalf("link message %+v", m.msg)
	}
	if m.to.ThreadID != to.ThreadID || m.to.topic != nil {
		t.Fatalf("link not posted in the topic thread: %+v", m.to)
	}
	if len(e.cl.pins) != 1 || e.cl.pins[0] != 701 {
		t.Fatalf("pins %v", e.cl.pins)
	}
	row := e.st.only(t)
	if v, ok := e.st.linkState(row.ID); !ok || v != 701 {
		t.Fatalf("link state %d %v", v, ok)
	}
	// Later notices to the same topic never post another link.
	e.notice(t, "86abc", "hai")
	e.notice(t, "86abc", "ba")
	if e.cl.sentCount() != 1 || len(e.cl.pins) != 1 || e.cl.createCount() != 1 {
		t.Fatalf("sent=%d pins=%d creates=%d", e.cl.sentCount(), len(e.cl.pins), e.cl.createCount())
	}
	// The notices themselves went to the topic through the normal messenger.
	if len(e.rec.sent) != 3 {
		t.Fatalf("notices %d", len(e.rec.sent))
	}
	// A workflow topic and a General message get no task link.
	wf := e.chat
	wf.topic = &TopicIntent{Mode: ReportTopicWorkflow, ExecutionID: 9}
	if _, _, err := e.eng.deliver(context.Background(), wf, OutMessage{Text: "wf"}); err != nil {
		t.Fatal(err)
	}
	if e.cl.sentCount() != 1 {
		t.Fatal("workflow topic got a task link")
	}
}

// TC-TT-2b (TD-TT-2): concurrent creates post one link.
func TestTD_TT2_ConcurrentCreatesOneLink(t *testing.T) {
	e := newTT(t)
	e.infos["86abc"] = TaskTopicInfo{Title: "T", CustomID: "FC-1"}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = e.eng.deliver(context.Background(), e.route("86abc"), OutMessage{Text: "x"})
		}()
	}
	wg.Wait()
	if e.cl.sentCount() > 1 {
		t.Fatalf("%d link messages for one topic", e.cl.sentCount())
	}
	if e.cl.createCount() != 1 {
		t.Fatalf("%d topics created", e.cl.createCount())
	}
}

// TC-TT-2c (TD-TT-2): the claim survives an unclear send outcome and is released after a clear failure.
func TestTD_TT2_ClaimKeptOnUnclearOutcome(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		err      error
		retryOK  bool // a later refresh may post
		released bool
	}{
		{"timeout is unclear", context.DeadlineExceeded, false, false},
		{"retryable server error is unclear", &relay.DeliveryError{Code: "server_error", Retryable: true}, false, false},
		{"plain transport error is unclear", errors.New("connection reset"), false, false},
		{"permanent error is clear", &relay.DeliveryError{Code: "chat_not_found"}, true, true},
		{"rate limit is clear", &relay.DeliveryError{Code: "rate_limited", Retryable: true, RetryAfter: time.Second}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTT(t)
			e.infos["86abc"] = TaskTopicInfo{Title: "T", CustomID: "FC-1"}
			e.cl.sendErr = tc.err
			e.notice(t, "86abc", "một")
			row := e.st.only(t)
			_, claimed := e.st.linkState(row.ID)
			if claimed == tc.released {
				t.Fatalf("claim kept=%v, want released=%v", claimed, tc.released)
			}
			e.cl.sendErr = nil
			n, err := e.router.RefreshTaskTopics(ctx, p6bWS)
			if err != nil {
				t.Fatal(err)
			}
			if tc.retryOK && (n.Linked != 1 || e.cl.sentCount() != 1) {
				t.Fatalf("not retried after a clear failure: %+v sent=%d", n, e.cl.sentCount())
			}
			if !tc.retryOK && (n.Linked != 0 || e.cl.sentCount() != 0) {
				t.Fatalf("posted again after an unclear outcome (risk of a duplicate): %+v sent=%d", n, e.cl.sentCount())
			}
		})
	}
}

// TC-TT-2d (TD-TT-2): a pin failure is tolerated, logged with a fixed text and not retried; a failed claim posts nothing.
func TestTD_TT2_PinFailureAndClaimFailure(t *testing.T) {
	ctx := context.Background()
	buf := &bytes.Buffer{}
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })

	e := newTT(t)
	e.infos["86abc"] = TaskTopicInfo{Title: "Private payroll", CustomID: "FC-1"}
	e.cl.pinErr = &relay.DeliveryError{Code: "not_enough_rights"}
	to := e.notice(t, "86abc", "một")
	if to.ThreadID == 0 || e.cl.sentCount() != 1 || len(e.cl.pins) != 0 {
		t.Fatalf("to=%+v sent=%d pins=%v", to, e.cl.sentCount(), e.cl.pins)
	}
	if !strings.Contains(buf.String(), "task link not pinned") || strings.Contains(buf.String(), "payroll") || strings.Contains(buf.String(), "FC-1") {
		t.Fatalf("log %q", buf.String())
	}
	// Not retried: another refresh and another notice neither post nor pin.
	e.cl.pinErr = nil
	n, _ := e.router.RefreshTaskTopics(ctx, p6bWS)
	e.notice(t, "86abc", "hai")
	if n.Linked != 0 || e.cl.sentCount() != 1 || len(e.cl.pins) != 0 {
		t.Fatalf("refresh=%+v sent=%d pins=%v", n, e.cl.sentCount(), e.cl.pins)
	}
	// A failing claim still delivers the notice and posts nothing.
	e2 := newTT(t)
	e2.infos["86abc"] = TaskTopicInfo{Title: "T", CustomID: "FC-1"}
	e2.st.claimErr = errors.New("db down")
	if to := e2.notice(t, "86abc", "x"); to.ThreadID == 0 || e2.cl.sentCount() != 0 {
		t.Fatalf("to=%+v sent=%d", to, e2.cl.sentCount())
	}
}

// TC-TT-2e (TD-TT-2): the button only opens ClickUp; anything else falls back to the canonical URL.
func TestTD_TT2_TaskLinkURL(t *testing.T) {
	long := "https://app.clickup.com/t/" + strings.Repeat("a", 600)
	cases := []struct {
		name string
		url  string
		ok   bool
		want string
	}{
		{"clickup url kept", "https://app.clickup.com/t/9008218621/FC-6583", true, "https://app.clickup.com/t/9008218621/FC-6583"},
		{"empty falls back", "", true, "https://app.clickup.com/t/86abc"},
		{"other host", "https://evil.example/t/86abc", true, "https://app.clickup.com/t/86abc"},
		{"lookalike host", "https://app.clickup.com.evil.example/t/1", true, "https://app.clickup.com/t/86abc"},
		{"http scheme", "http://app.clickup.com/t/1", true, "https://app.clickup.com/t/86abc"},
		{"javascript scheme", "javascript:alert(1)", true, "https://app.clickup.com/t/86abc"},
		{"tg scheme", "tg://resolve?domain=x", true, "https://app.clickup.com/t/86abc"},
		{"space inside", "https://app.clickup.com/t/1 2", true, "https://app.clickup.com/t/86abc"},
		{"newline inside", "https://app.clickup.com/t/1\nx", true, "https://app.clickup.com/t/86abc"},
		{"non ascii", "https://app.clickup.com/t/đ", true, "https://app.clickup.com/t/86abc"},
		{"too long", long, true, "https://app.clickup.com/t/86abc"},
		{"unreadable task ignores url", "https://app.clickup.com/t/9/FC-1", false, "https://app.clickup.com/t/86abc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := taskLinkURL("86abc", TaskTopicInfo{URL: c.url}, c.ok); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
	// End to end: an evil url from the reader never becomes the button.
	e := newTT(t)
	e.infos["86abc"] = TaskTopicInfo{Title: "T", CustomID: "FC-1", URL: "https://evil.example/phish"}
	e.notice(t, "86abc", "x")
	if got := e.cl.sent[0].msg.Buttons[0][0].URL; got != "https://app.clickup.com/t/86abc" {
		t.Fatalf("button url %q", got)
	}
	// An unreadable task still gets a link (fallback url, task id label).
	e.unread["86zzz"] = true
	e.notice(t, "86zzz", "x")
	last := e.cl.sent[len(e.cl.sent)-1].msg
	if last.Text != "86zzz" || last.Buttons[0][0].URL != "https://app.clickup.com/t/86zzz" {
		t.Fatalf("unreadable link %+v", last)
	}
}

// TC-TT-3 (TD-TT-3, REQ-3): rename and link counts, idempotence, closed topics, failures.
func TestTD_TT3_RefreshCountsAndIdempotence(t *testing.T) {
	ctx := context.Background()
	e := newTT(t)
	// Three task topics created with the old names (task id only: tasks unreadable at the time).
	for _, id := range []string{"86a1", "86a2", "86a3"} {
		e.unread[id] = true
		e.notice(t, id, "x")
	}
	for id := range e.unread {
		delete(e.unread, id)
	}
	e.infos["86a1"] = TaskTopicInfo{Title: "One", CustomID: "FC-1"}
	e.infos["86a2"] = TaskTopicInfo{Title: "Two", CustomID: "FC-2"}
	e.infos["86a3"] = TaskTopicInfo{Title: "Three"}
	// Link messages were posted at creation only for readable tasks: none were.
	if e.cl.sentCount() != 3 {
		t.Fatalf("setup: %d link messages", e.cl.sentCount())
	}
	// The first topic is closed: it is renamed but gets no new link (its link was posted already, so it is claimed).
	row1, _ := e.st.TopicBySubject(ctx, TopicKey{WorkspaceID: p6bWS, IntegrationID: p6bBot, ChatID: e.chat.ChatID, SubjectKind: TopicSubjectClickUpTask, SubjectKey: "86a1"})
	if _, err := e.st.SetTopicState(ctx, p6bWS, row1.ID, TopicClosed); err != nil {
		t.Fatal(err)
	}
	sentBefore := e.cl.sentCount()
	n, err := e.router.RefreshTaskTopics(ctx, p6bWS)
	if err != nil {
		t.Fatal(err)
	}
	if n.Renamed != 3 || n.Failed != 0 {
		t.Fatalf("counts %+v", n)
	}
	if n.Linked != 0 || e.cl.sentCount() != sentBefore {
		t.Fatalf("links were already posted at creation, none may be added: %+v", n)
	}
	if e.cl.renames[101] != "FC-1 · One" || e.cl.renames[102] != "FC-2 · Two" || e.cl.renames[103] != "86a3 · Three" {
		t.Fatalf("renames %v", e.cl.renames)
	}
	// Running it again changes nothing new.
	n2, err := e.router.RefreshTaskTopics(ctx, p6bWS)
	if err != nil || n2 != (TopicRefreshCounts{}) {
		t.Fatalf("second run %+v %v", n2, err)
	}
	if e.cl.sentCount() != sentBefore {
		t.Fatal("second run posted links")
	}
}

// TC-TT-3b (TD-TT-3): topics without a link get one when open and not when closed; counts only.
func TestTD_TT3_RefreshLinksOpenTopicsOnly(t *testing.T) {
	ctx := context.Background()
	e := newTT(t)
	e.infos["86a1"] = TaskTopicInfo{Title: "One", CustomID: "FC-1"}
	e.infos["86a2"] = TaskTopicInfo{Title: "Two", CustomID: "FC-2"}
	// Topics that exist from before the hotfix: thread bound, never linked.
	for i, id := range []string{"86a1", "86a2"} {
		row, _, err := e.st.EnsureTopic(ctx, TopicKey{WorkspaceID: p6bWS, IntegrationID: p6bBot, ChatID: e.chat.ChatID, SubjectKind: TopicSubjectClickUpTask, SubjectKey: id})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.st.SetTopicThread(ctx, p6bWS, row.ID, int64(300+i), row.ReservedAt); err != nil {
			t.Fatal(err)
		}
		e.cl.renames[int64(300+i)] = id
	}
	closed, _ := e.st.TopicBySubject(ctx, TopicKey{WorkspaceID: p6bWS, IntegrationID: p6bBot, ChatID: e.chat.ChatID, SubjectKind: TopicSubjectClickUpTask, SubjectKey: "86a2"})
	_, _ = e.st.SetTopicState(ctx, p6bWS, closed.ID, TopicClosed)
	buf := captureLog(t)
	n, err := e.router.RefreshTaskTopics(ctx, p6bWS)
	if err != nil {
		t.Fatal(err)
	}
	if n.Renamed != 2 || n.Linked != 1 || n.Failed != 0 {
		t.Fatalf("counts %+v", n)
	}
	if e.cl.sentCount() != 1 || e.cl.sent[0].to.ThreadID != 300 {
		t.Fatalf("link must go to the open topic only: %+v", e.cl.sent)
	}
	if e.cl.renames[301] != "FC-2 · Two" {
		t.Fatalf("closed topic not renamed: %q", e.cl.renames[301])
	}
	if strings.Contains(buf.String(), "One") || strings.Contains(buf.String(), "FC-1") {
		t.Fatalf("names logged: %s", buf.String())
	}
	if n2, _ := e.router.RefreshTaskTopics(ctx, p6bWS); n2 != (TopicRefreshCounts{}) {
		t.Fatalf("not idempotent: %+v", n2)
	}
}

// TC-TT-3c (TD-TT-3): unreadable tasks, rename errors, other subjects and bad rows.
func TestTD_TT3_RefreshFailuresAndSkips(t *testing.T) {
	ctx := context.Background()
	e := newTT(t)
	key := func(kind, id string) TopicKey {
		return TopicKey{WorkspaceID: p6bWS, IntegrationID: p6bBot, ChatID: e.chat.ChatID, SubjectKind: kind, SubjectKey: id}
	}
	add := func(kind, id string, thread int64) {
		row, _, err := e.st.EnsureTopic(ctx, key(kind, id))
		if err != nil {
			t.Fatal(err)
		}
		if thread > 0 {
			if _, err := e.st.SetTopicThread(ctx, p6bWS, row.ID, thread, row.ReservedAt); err != nil {
				t.Fatal(err)
			}
		}
		e.cl.renames[thread] = "old"
	}
	add(TopicSubjectClickUpTask, "86ok", 401)
	add(TopicSubjectClickUpTask, "86unread", 402)
	add(TopicSubjectClickUpTask, "86renameerr", 403)
	add(TopicSubjectClickUpTask, "86notmod", 404)
	add(TopicSubjectClickUpTask, "86nothread", 0)
	add(TopicSubjectWorkflow, "55", 405)
	add(TopicSubjectFixed, "alerts", 406)
	add(TopicSubjectClickUpTask, "bad key/..", 407)
	for _, id := range []string{"86ok", "86renameerr", "86notmod"} {
		e.infos[id] = TaskTopicInfo{Title: id, CustomID: "FC-" + "9"}
	}
	e.unread["86unread"] = true
	e.cl.renameErr[403] = errors.New("telegram 400 secret detail")
	e.cl.renameErr[404] = p6bErr("topic_not_modified")
	buf := captureLog(t)
	n, err := e.router.RefreshTaskTopics(ctx, p6bWS)
	if err != nil {
		t.Fatal(err)
	}
	// ok: renamed + linked. unread: failed. renameerr: failed (but linked). notmod: not counted as renamed, not failed.
	if n.Renamed != 1 || n.Failed != 2 {
		t.Fatalf("counts %+v", n)
	}
	for thread, name := range map[int64]string{405: "old", 406: "old", 407: "old", 402: "old"} {
		if e.cl.renames[thread] != name {
			t.Fatalf("thread %d renamed to %q", thread, e.cl.renames[thread])
		}
	}
	for _, s := range e.cl.sent {
		if s.to.ThreadID == 402 || s.to.ThreadID == 405 || s.to.ThreadID == 406 || s.to.ThreadID == 407 {
			t.Fatalf("link posted in thread %d", s.to.ThreadID)
		}
	}
	if strings.Contains(buf.String(), "secret") || strings.Contains(buf.String(), "SECRET") {
		t.Fatalf("raw error logged: %s", buf.String())
	}
}

// TC-TT-3d (TD-TT-3): edge cases of the refresh entry point.
func TestTD_TT3_RefreshEdges(t *testing.T) {
	ctx := context.Background()
	var nilRouter *TopicRouter
	if _, err := nilRouter.RefreshTaskTopics(ctx, p6bWS); !errors.Is(err, ErrNotAvailable) {
		t.Fatalf("nil router: %v", err)
	}
	e := newTT(t)
	e.st.listErr = errors.New("db down")
	if _, err := e.router.RefreshTaskTopics(ctx, p6bWS); err == nil {
		t.Fatal("list error swallowed")
	}
	e.st.listErr = nil
	// An empty workspace is fine.
	if n, err := e.router.RefreshTaskTopics(ctx, p6bWS); err != nil || n != (TopicRefreshCounts{}) {
		t.Fatalf("%+v %v", n, err)
	}
	// A cancelled context stops the run.
	e.infos["86a1"] = TaskTopicInfo{Title: "One"}
	e.notice(t, "86a1", "x")
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := e.router.RefreshTaskTopics(cctx, p6bWS); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	// A client that cannot rename: links are still handled, nothing counts as renamed.
	st := &ttStore{p6bStore: newP6bStore(), link: map[int64]int64{}}
	plain := &p6bClient{}
	r, _ := NewTopicRouter(TopicRouterDeps{Store: st, Client: plain,
		TaskInfo: func(context.Context, string, string, string) (TaskTopicInfo, error) {
			return TaskTopicInfo{Title: "T"}, nil
		}})
	row, _, _ := st.EnsureTopic(ctx, TopicKey{WorkspaceID: p6bWS, IntegrationID: p6bBot, ChatID: -1001, SubjectKind: TopicSubjectClickUpTask, SubjectKey: "86a1"})
	_, _ = st.SetTopicThread(ctx, p6bWS, row.ID, 9, row.ReservedAt)
	if n, err := r.RefreshTaskTopics(ctx, p6bWS); err != nil || n.Renamed != 0 {
		t.Fatalf("%+v %v", n, err)
	}
}

// TC-TT-5 (TD-TT-5, REQ-5): notices for a task with a topic keep going to that topic.
func TestTD_TT5_NoticesKeepGoingToTheTaskTopic(t *testing.T) {
	e := newTT(t)
	e.infos["86a1"] = TaskTopicInfo{Title: "One", CustomID: "FC-1"}
	e.infos["86a2"] = TaskTopicInfo{Title: "Two", CustomID: "FC-2"}
	first := e.notice(t, "86a1", "bình luận mới")
	second := e.notice(t, "86a1", "đổi trạng thái")
	other := e.notice(t, "86a2", "bình luận khác")
	if first.ThreadID == 0 || second.ThreadID != first.ThreadID {
		t.Fatalf("task notices split across threads: %d and %d", first.ThreadID, second.ThreadID)
	}
	if other.ThreadID == 0 || other.ThreadID == first.ThreadID {
		t.Fatalf("another task shares the topic: %d", other.ThreadID)
	}
	if e.cl.createCount() != 2 {
		t.Fatalf("creates=%d", e.cl.createCount())
	}
	// The pinned link message must not replace a notice: every notice was still sent to the messenger.
	if len(e.rec.sent) != 3 {
		t.Fatalf("notices delivered %d", len(e.rec.sent))
	}
	for i, d := range e.rec.sent {
		if d.to.ThreadID == 0 {
			t.Fatalf("notice %d went to General", i)
		}
	}
	// Task whose info cannot be read still keeps its topic, and the notice goes there.
	e.unread["86a3"] = true
	a := e.notice(t, "86a3", "x")
	b := e.notice(t, "86a3", "y")
	if a.ThreadID == 0 || a.ThreadID != b.ThreadID {
		t.Fatalf("unreadable task notices: %d %d", a.ThreadID, b.ThreadID)
	}
	// A closed topic is reopened and the notice still goes to it.
	e.st.p6bStore.mu.Lock()
	for _, r := range e.st.p6bStore.rows {
		r.State = TopicClosed
	}
	e.st.p6bStore.mu.Unlock()
	again := e.notice(t, "86a1", "sau khi đóng")
	if again.ThreadID != first.ThreadID {
		t.Fatalf("closed topic notice went to thread %d, want %d", again.ThreadID, first.ThreadID)
	}
	if e.cl.reopens == 0 {
		t.Fatal("closed topic not reopened")
	}
	// A task with no topic at all (General mode) is unaffected.
	gen := e.chat
	gen.topic = &TopicIntent{Mode: ReportTopicGeneral}
	if _, to, err := e.eng.deliver(context.Background(), gen, OutMessage{Text: "g"}); err != nil || to.ThreadID != 0 {
		t.Fatalf("general: to=%+v err=%v", to, err)
	}
}
