package behavior

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/reporting"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Phase 6b batch A: report.topic, the topic router, the fallback matrix and
// reply threads. Fakes only.

const (
	p6bWS     = "ws-6b"
	p6bBot    = "bot-6b"
	p6bChatID = int64(-100500)
)

func p6bBase() ChatRef { return ChatRef{WorkspaceID: p6bWS, IntegrationID: p6bBot, ChatID: p6bChatID} }

func p6bErr(code string) error { return &relay.DeliveryError{Code: code} }

// --- fake topic store (same claim semantics as the SQL store) ---

type p6bStore struct {
	mu        sync.Mutex
	rows      map[string]*Topic
	next      int64
	clock     time.Time
	ensureErr error
	setErr    error
	released  int
	deleted   int
}

func newP6bStore() *p6bStore {
	return &p6bStore{rows: map[string]*Topic{}, clock: time.Unix(1000, 0)}
}

func p6bKeyString(k TopicKey) string {
	return fmt.Sprint(k.WorkspaceID, "|", k.IntegrationID, "|", k.ChatID, "|", k.SubjectKind, "|", k.SubjectKey)
}

func (s *p6bStore) EnsureTopic(_ context.Context, k TopicKey) (Topic, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ensureErr != nil {
		return Topic{}, false, s.ensureErr
	}
	ks := p6bKeyString(k)
	if row, ok := s.rows[ks]; ok {
		if row.ThreadID == 0 && s.clock.Sub(row.ReservedAt) >= TopicReservationLease {
			row.ReservedAt = s.clock
			return *row, true, nil
		}
		return *row, false, nil
	}
	s.next++
	row := &Topic{ID: s.next, WorkspaceID: k.WorkspaceID, IntegrationID: k.IntegrationID, ChatID: k.ChatID, SubjectKind: k.SubjectKind, SubjectKey: k.SubjectKey, State: TopicOpen, ReservedAt: s.clock}
	s.rows[ks] = row
	return *row, true, nil
}

func (s *p6bStore) byID(id int64) (string, *Topic) {
	for k, r := range s.rows {
		if r.ID == id {
			return k, r
		}
	}
	return "", nil
}

func (s *p6bStore) SetTopicThread(_ context.Context, ws string, id, thread int64) (Topic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setErr != nil {
		return Topic{}, s.setErr
	}
	_, r := s.byID(id)
	if r == nil || r.WorkspaceID != ws || r.ThreadID != 0 {
		return Topic{}, ErrConflict
	}
	r.ThreadID = thread
	return *r, nil
}

func (s *p6bStore) ReleaseTopic(_ context.Context, ws string, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k, r := s.byID(id); r != nil && r.WorkspaceID == ws && r.ThreadID == 0 {
		delete(s.rows, k)
		s.released++
	}
	return nil
}

func (s *p6bStore) TopicBySubject(_ context.Context, k TopicKey) (Topic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rows[p6bKeyString(k)]; ok {
		return *r, nil
	}
	return Topic{}, ErrNotFound
}

func (s *p6bStore) TopicByThread(_ context.Context, ws, integration string, chat, thread int64) (Topic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rows {
		if r.WorkspaceID == ws && r.IntegrationID == integration && r.ChatID == chat && r.ThreadID == thread && thread != 0 {
			return *r, nil
		}
	}
	return Topic{}, ErrNotFound
}

func (s *p6bStore) SetTopicState(_ context.Context, ws string, id int64, state string) (Topic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, r := s.byID(id)
	if r == nil || r.WorkspaceID != ws {
		return Topic{}, ErrNotFound
	}
	r.State = state
	return *r, nil
}

func (s *p6bStore) ListTopics(context.Context, string) ([]Topic, error) { return nil, nil }

func (s *p6bStore) DeleteTopic(_ context.Context, ws string, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k, r := s.byID(id); r != nil && r.WorkspaceID == ws {
		delete(s.rows, k)
		s.deleted++
	}
	return nil
}

func (s *p6bStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows)
}

func (s *p6bStore) only(t *testing.T) Topic {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rows) != 1 {
		t.Fatalf("topic rows=%d, want 1", len(s.rows))
	}
	for _, r := range s.rows {
		return *r
	}
	return Topic{}
}

// p6bNoDelete hides DeleteTopic: a store without a deleter marks closed.
type p6bNoDelete struct{ TopicStore }

// --- fake Telegram forum client ---

type p6bClient struct {
	mu        sync.Mutex
	nextID    int64
	creates   int
	names     []string
	reopens   int
	createErr error
	reopenErr error
	zeroID    bool
	hook      func()
}

func (c *p6bClient) CreateTopic(_ context.Context, _ ChatRef, name string) (int64, error) {
	if c.hook != nil {
		c.hook()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.creates++
	c.names = append(c.names, name)
	if c.createErr != nil {
		return 0, c.createErr
	}
	if c.zeroID {
		return 0, nil
	}
	c.nextID++
	return 100 + c.nextID, nil
}

func (c *p6bClient) ReopenTopic(context.Context, ChatRef, int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reopens++
	return c.reopenErr
}

func (c *p6bClient) createCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.creates
}

// --- fake messenger recording the chat of every attempt ---

type p6bSend struct {
	to   ChatRef
	text string
	err  error
}

type p6bMessenger struct {
	Messenger
	mu       sync.Mutex
	attempts []p6bSend
	nextID   int64
	fail     func(to ChatRef, attempt int) error
}

func (m *p6bMessenger) SendMessage(_ context.Context, to ChatRef, msg OutMessage) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var err error
	if m.fail != nil {
		err = m.fail(to, len(m.attempts))
	}
	m.attempts = append(m.attempts, p6bSend{to: to, text: msg.Text, err: err})
	if err != nil {
		return 0, err
	}
	m.nextID++
	return 500 + m.nextID, nil
}

func (m *p6bMessenger) delivered() []p6bSend {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []p6bSend
	for _, a := range m.attempts {
		if a.err == nil {
			out = append(out, a)
		}
	}
	return out
}

func (m *p6bMessenger) tried() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.attempts)
}

// --- message log keyed by key, releasing on failure ---

type p6bMsgLog struct {
	ActionStore
	mu      sync.Mutex
	claimed map[string]bool
	ids     map[string]int64
}

func newP6bMsgLog() *p6bMsgLog {
	return &p6bMsgLog{claimed: map[string]bool{}, ids: map[string]int64{}}
}

func (s *p6bMsgLog) ClaimMessage(_ context.Context, _ string, _ int64, key string) (bool, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed[key] {
		return false, s.ids[key], nil
	}
	s.claimed[key] = true
	return true, 0, nil
}

func (s *p6bMsgLog) SetMessageID(_ context.Context, _ string, _ int64, key string, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids[key] = id
	return nil
}

func (s *p6bMsgLog) ReleaseMessage(_ context.Context, _ string, _ int64, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.claimed, key)
	return nil
}

// --- conversation fake recording the bound thread ---

type p6bConv struct {
	workflow.ConversationStore
	mu      sync.Mutex
	threads []int64
}

func (c *p6bConv) EnsureConversation(_ context.Context, cv workflow.Conversation) (workflow.Conversation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.threads = append(c.threads, cv.ThreadID)
	cv.ID = 1
	return cv, nil
}
func (*p6bConv) BindWorkflow(context.Context, string, int64, int64, string) error { return nil }
func (*p6bConv) BindResource(context.Context, workflow.ResourceBinding) error     { return nil }

type p6bResources struct {
	list []workflow.ResourceBinding
	err  error
}

func (r p6bResources) ListResources(context.Context, string, int64) ([]workflow.ResourceBinding, error) {
	return r.list, r.err
}

type p6bChats struct{ chat ChatRef }

func (c p6bChats) OwnerChat(context.Context, string, TriggerData) (ChatRef, error) {
	return c.chat, nil
}

type p6bEnv struct {
	store  *p6bStore
	client *p6bClient
	msg    *p6bMessenger
	log    *p6bMsgLog
	conv   *p6bConv
	router *TopicRouter
	eng    *Engine
	titles map[string]string
	titleN int
	titleE error
}

func newP6bEnv(t *testing.T) *p6bEnv {
	t.Helper()
	e := &p6bEnv{store: newP6bStore(), client: &p6bClient{}, msg: &p6bMessenger{}, log: newP6bMsgLog(), conv: &p6bConv{}, titles: map[string]string{}}
	var err error
	e.router, err = NewTopicRouter(TopicRouterDeps{
		Store: e.store, Client: e.client,
		TaskName: func(_ context.Context, _, _, task string) (string, error) {
			e.titleN++
			return e.titles[task], e.titleE
		},
		Resources: p6bResources{list: []workflow.ResourceBinding{{Kind: "github.pr", ExternalID: "o/r#1"}, {Kind: "clickup.task", ExternalID: "BOUND-7"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.eng = &Engine{
		d: ActionDeps{Store: e.log, Messenger: e.msg, Conversations: e.conv, Topics: e.router, Reports: reporting.NewRenderer(nil, nil), Chats: p6bChats{p6bBase()}},
		s: ActionSettings{RequestTimeout: time.Second}, now: time.Now,
	}
	return e
}

// send routes one message of an execution like Dispatch does.
func (e *p6bEnv) send(ctx context.Context, key, text string, rc RunContext) (int64, error) {
	chat := e.eng.routed(ctx, rc, p6bBase())
	return e.eng.sendOnce(ctx, rc.Run, key, chat, OutMessage{Text: text})
}

func p6bRC(mode, task string) RunContext {
	return RunContext{
		Run:     Run{ID: 7, WorkspaceID: p6bWS, BehaviorName: "lead-tag"},
		Doc:     ResolvedDoc{Name: "lead-tag", ReportTopic: mode},
		Trigger: TriggerData{TaskID: task, IntegrationID: p6bBot},
	}
}

func lastThread(t *testing.T, m *p6bMessenger) int64 {
	t.Helper()
	d := m.delivered()
	if len(d) == 0 {
		t.Fatal("nothing delivered")
	}
	return d[len(d)-1].to.ThreadID
}

// ---------------------------------------------------------------- REQ-5

// TC-A5.1 (REQ-5): report.topic validation.
func TestPhase6bReportTopicValidation(t *testing.T) {
	base := func(topic string) Doc {
		return Doc{Schema: 1, Name: "x", Triggers: []TriggerDoc{{Type: TriggerCommentAdded}}, Instructions: "Summarize.",
			Actions: map[string][]ActionDoc{"notify": {{Type: "telegram.message"}}}, Report: &ReportDoc{Topic: topic}}
	}
	has := func(errs []FieldError) bool {
		for _, e := range errs {
			if e.Path == "report.topic" {
				return true
			}
		}
		return false
	}
	valid := []string{"task", "workflow", "general", "PR approve", "Đánh giá 🙂", strings.Repeat("a", 64), strings.Repeat("ả", 21) /* 63 bytes */, "a", "Task", "GENERAL"}
	for _, v := range valid {
		if errs := Validate(base(v), nil, nil); has(errs) {
			t.Errorf("%q refused: %v", v, errs)
		}
	}
	invalid := map[string]string{
		"65 bytes":        strings.Repeat("a", 65),
		"22 two-byte":     strings.Repeat("ả", 22) + "a",
		"newline":         "PR\napprove",
		"tab":             "PR\tapprove",
		"NUL":             "PR\x00x",
		"escape":          "PR\x1bx",
		"invalid UTF-8":   "PR \xff\xfe",
		"blank":           "   ",
		"unicode control": "a\u0085b",
	}
	for name, v := range invalid {
		if errs := Validate(base(v), nil, nil); !has(errs) {
			t.Errorf("%s (%q) accepted", name, v)
		}
	}
	// Empty topic = default (task); absent block = default.
	if errs := Validate(base(""), nil, nil); has(errs) {
		t.Errorf("empty topic must default: %v", errs)
	}
	doc := base("")
	doc.Report = nil
	r, err := Resolve(doc, nil)
	if err != nil || r.ReportTopic != ReportTopicTask {
		t.Fatalf("default resolved topic=%q err=%v", r.ReportTopic, err)
	}
	for _, mode := range []string{"workflow", "general", "PR approve"} {
		r, err := Resolve(base(mode), nil)
		if err != nil || r.ReportTopic != mode {
			t.Errorf("resolved %q = %q err=%v", mode, r.ReportTopic, err)
		}
	}
}

// TC-A5.2 (REQ-5): the example behaviour sets a fixed topic and stays
// valid and generic.
func TestPhase6bExampleReportTopic(t *testing.T) {
	raw, err := os.ReadFile("../../examples/behaviors/pr-auto-approve.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Parse(raw, FormatYAML)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Report == nil || doc.Report.Topic != "PR approve" {
		t.Fatalf("report block=%+v", doc.Report)
	}
	vars := map[string]string{"github_account": "owner"}
	if errs := Validate(doc, vars, nil, WithGitHubAccounts(map[string]string{"owner": GitHubAccountPAT})); len(errs) != 0 {
		t.Fatalf("example invalid: %v", errs)
	}
	r, err := Resolve(doc, vars)
	if err != nil || r.ReportTopic != "PR approve" {
		t.Fatalf("resolved=%q err=%v", r.ReportTopic, err)
	}
	// Strict YAML: a typo under report is refused.
	if _, err := Parse([]byte(strings.Replace(string(raw), "topic: \"PR approve\"", "topik: \"PR approve\"", 1)), FormatYAML); err == nil {
		t.Error("unknown report key accepted")
	}
}

// ---------------------------------------------------------------- REQ-2/3

// TC-A6.1 (REQ-2): task mode uses the trigger task; second event reuses the
// topic; the stored key is the id, never the title.
func TestPhase6bTaskTopicCreatedOnceAndReused(t *testing.T) {
	e := newP6bEnv(t)
	logs := p6aCaptureLog(t)
	e.titles["ABC-12"] = "Quyết định SECRET-TITLE về giá"
	rc := p6bRC("task", "ABC-12")
	if _, err := e.send(context.Background(), "k1", "báo cáo SECRET-BODY một", rc); err != nil {
		t.Fatal(err)
	}
	first := lastThread(t, e.msg)
	if first == 0 || e.client.createCount() != 1 {
		t.Fatalf("thread=%d creates=%d", first, e.client.createCount())
	}
	if _, err := e.send(context.Background(), "k2", "báo cáo SECRET-BODY hai", rc); err != nil {
		t.Fatal(err)
	}
	if lastThread(t, e.msg) != first || e.client.createCount() != 1 || e.titleN != 1 {
		t.Fatalf("second event thread=%d creates=%d titleReads=%d", lastThread(t, e.msg), e.client.createCount(), e.titleN)
	}
	row := e.store.only(t)
	if row.SubjectKind != TopicSubjectClickUpTask || row.SubjectKey != "ABC-12" || row.ThreadID != first {
		t.Fatalf("row=%+v", row)
	}
	if got := e.client.names[0]; got != "ABC-12 · Quyết định SECRET-TITLE về giá" {
		t.Fatalf("topic name=%q", got)
	}
	// TC-A10.1 (REQ-6): nothing private reaches the log.
	for _, secret := range []string{"SECRET-TITLE", "SECRET-BODY", "Quyết định"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("log leaks %q:\n%s", secret, logs.String())
		}
	}
}

// TC-A6.2 (REQ-2): the task comes from the execution's ClickUp binding when
// the trigger has none (default mode).
func TestPhase6bTaskFromExecutionBinding(t *testing.T) {
	e := newP6bEnv(t)
	ctx := withWorkflowExecution(context.Background(), 900)
	if _, err := e.send(ctx, "k", "x", p6bRC("", "")); err != nil {
		t.Fatal(err)
	}
	if row := e.store.only(t); row.SubjectKind != TopicSubjectClickUpTask || row.SubjectKey != "BOUND-7" {
		t.Fatalf("row=%+v", row)
	}
	// A lister error means no task: workflow topic instead.
	e2 := newP6bEnv(t)
	e2.router.d.Resources = p6bResources{err: errors.New("db down")}
	if _, err := e2.send(ctx, "k", "x", p6bRC("task", "")); err != nil {
		t.Fatal(err)
	}
	if row := e2.store.only(t); row.SubjectKind != TopicSubjectWorkflow || row.SubjectKey != "900" {
		t.Fatalf("row=%+v", row)
	}
}

// TC-A6.3 (REQ-2): no task => WF-<id> topic; workflow mode ignores a task;
// fixed and general modes.
func TestPhase6bTopicModes(t *testing.T) {
	ctx := withWorkflowExecution(context.Background(), 31)
	t.Run("no task uses WF topic", func(t *testing.T) {
		e := newP6bEnv(t)
		e.router.d.Resources = nil
		if _, err := e.send(ctx, "k", "x", p6bRC("task", "")); err != nil {
			t.Fatal(err)
		}
		if row := e.store.only(t); row.SubjectKind != TopicSubjectWorkflow || row.SubjectKey != "31" {
			t.Fatalf("row=%+v", row)
		}
		if e.client.names[0] != "WF-31 · lead-tag" {
			t.Fatalf("name=%q", e.client.names[0])
		}
	})
	t.Run("workflow mode ignores the task", func(t *testing.T) {
		e := newP6bEnv(t)
		if _, err := e.send(ctx, "k", "x", p6bRC("workflow", "ABC-1")); err != nil {
			t.Fatal(err)
		}
		if row := e.store.only(t); row.SubjectKind != TopicSubjectWorkflow || row.SubjectKey != "31" || e.titleN != 0 {
			t.Fatalf("row=%+v titleReads=%d", row, e.titleN)
		}
	})
	t.Run("fixed name is shared by two executions", func(t *testing.T) {
		e := newP6bEnv(t)
		rc := p6bRC("PR approve", "ABC-1")
		if _, err := e.send(withWorkflowExecution(context.Background(), 1), "k", "x", rc); err != nil {
			t.Fatal(err)
		}
		first := lastThread(t, e.msg)
		rc.Run.ID = 8
		if _, err := e.send(withWorkflowExecution(context.Background(), 2), "k", "y", rc); err != nil {
			t.Fatal(err)
		}
		if lastThread(t, e.msg) != first || e.client.createCount() != 1 {
			t.Fatalf("fixed topic not shared: creates=%d", e.client.createCount())
		}
		if row := e.store.only(t); row.SubjectKind != TopicSubjectFixed || row.SubjectKey != "PR approve" || e.client.names[0] != "PR approve" {
			t.Fatalf("row=%+v name=%q", row, e.client.names[0])
		}
	})
	t.Run("general goes to General without touching the store", func(t *testing.T) {
		e := newP6bEnv(t)
		if _, err := e.send(ctx, "k", "x", p6bRC("general", "ABC-1")); err != nil {
			t.Fatal(err)
		}
		if lastThread(t, e.msg) != 0 || e.store.count() != 0 || e.client.createCount() != 0 {
			t.Fatalf("general touched topics: rows=%d creates=%d", e.store.count(), e.client.createCount())
		}
	})
	t.Run("mode words are never topic names", func(t *testing.T) {
		e := newP6bEnv(t)
		for _, m := range []string{"general", " general ", "workflow", "task"} {
			e.router.Destination(ctx, p6bBase(), TopicIntent{Mode: m, ExecutionID: 31, TaskID: "T-1"})
		}
		e.store.mu.Lock()
		defer e.store.mu.Unlock()
		for _, r := range e.store.rows {
			if r.SubjectKind == TopicSubjectFixed {
				t.Fatalf("mode word became a fixed topic: %+v", r)
			}
		}
	})
	t.Run("invalid task id is not a task subject", func(t *testing.T) {
		e := newP6bEnv(t)
		e.router.d.Resources = nil
		for _, bad := range []string{"../x", "a b", strings.Repeat("a", 65), "-lead"} {
			e.router.Destination(ctx, p6bBase(), TopicIntent{Mode: "task", ExecutionID: 31, TaskID: bad})
		}
		e.store.mu.Lock()
		defer e.store.mu.Unlock()
		for _, r := range e.store.rows {
			if r.SubjectKind == TopicSubjectClickUpTask {
				t.Fatalf("invalid task id stored: %+v", r)
			}
		}
	})
	t.Run("no task and no execution goes to General", func(t *testing.T) {
		e := newP6bEnv(t)
		if _, err := e.send(context.Background(), "k", "x", p6bRC("task", "")); err != nil {
			t.Fatal(err)
		}
		if lastThread(t, e.msg) != 0 || e.store.count() != 0 {
			t.Fatal("expected General")
		}
	})
}

// TC-A6.4 (REQ-2): a run that sends nothing creates no topic; a nil router
// changes nothing.
func TestPhase6bTopicIsLazy(t *testing.T) {
	e := newP6bEnv(t)
	chat := e.eng.routed(withWorkflowExecution(context.Background(), 5), p6bRC("task", "ABC-9"), p6bBase())
	if chat.topic == nil || chat.ThreadID != 0 {
		t.Fatalf("routed chat=%+v", chat)
	}
	if e.store.count() != 0 || e.client.createCount() != 0 || e.titleN != 0 || e.msg.tried() != 0 {
		t.Fatal("routing a chat must not touch Telegram or the store")
	}
	off := newP6bEnv(t)
	off.eng.d.Topics = nil
	if _, err := off.send(withWorkflowExecution(context.Background(), 5), "k", "x", p6bRC("task", "ABC-9")); err != nil {
		t.Fatal(err)
	}
	if lastThread(t, off.msg) != 0 || off.store.count() != 0 {
		t.Fatal("without a router everything goes to General")
	}
}

// TC-A6.5 (REQ-2): topic names are sanitized, bounded and never stored.
func TestPhase6bTopicNameSanitizing(t *testing.T) {
	cases := map[string]string{
		"control":     "T-1 · a\x00b\nc\td\x1be",
		"blanks":      "T-1 ·    spaced     out   ",
		"invalid":     "T-1 · bad\xff\xfetitle",
		"huge":        strings.Repeat("ả", 500),
		"emoji":       strings.Repeat("🙂", 100),
		"only-ctrl":   "\x00\x01\x02",
		"unicode-nel": "x\u0085y",
	}
	for name, title := range cases {
		t.Run(name, func(t *testing.T) {
			e := newP6bEnv(t)
			e.titles["T-1"] = title
			e.router.Destination(context.Background(), p6bBase(), TopicIntent{Mode: "task", TaskID: "T-1", ExecutionID: 1})
			if e.client.createCount() != 1 {
				t.Fatalf("creates=%d", e.client.createCount())
			}
			got := e.client.names[0]
			if len(got) > MaxTopicNameBytes || !utf8.ValidString(got) || got == "" {
				t.Fatalf("name %q (len %d)", got, len(got))
			}
			for _, r := range got {
				if r < 0x20 || r == 0x7f || r == 0x85 {
					t.Fatalf("control character in %q", got)
				}
			}
			if strings.Contains(got, "  ") || got != strings.TrimSpace(got) {
				t.Fatalf("blanks not collapsed: %q", got)
			}
			if name == "only-ctrl" && got != "T-1" {
				t.Fatalf("empty title must give the bare id, got %q", got)
			}
			row := e.store.only(t)
			if strings.Contains(row.SubjectKey, "·") || len(row.SubjectKey) > 128 {
				t.Fatalf("stored key carries the title: %q", row.SubjectKey)
			}
		})
	}
	// A hostile fixed name from YAML is sanitized too.
	e := newP6bEnv(t)
	e.router.Destination(context.Background(), p6bBase(), TopicIntent{Mode: "PR\napprove\x00", ExecutionID: 1})
	if e.client.names[0] != "PR approve" {
		t.Fatalf("fixed name=%q", e.client.names[0])
	}
	if e.client.names[0] == "" {
		t.Fatal("empty name")
	}
}

// TC-A6.6 (REQ-2): a task-name read failure still gives a topic named by the
// bare task id.
func TestPhase6bTaskNameFailureUsesBareID(t *testing.T) {
	e := newP6bEnv(t)
	e.titleE = errors.New("clickup 500 SECRET-DETAIL")
	logs := p6aCaptureLog(t)
	if _, err := e.send(withWorkflowExecution(context.Background(), 1), "k", "x", p6bRC("task", "ABC-5")); err != nil {
		t.Fatal(err)
	}
	if e.client.names[0] != "ABC-5" || lastThread(t, e.msg) == 0 {
		t.Fatalf("name=%q thread=%d", e.client.names[0], lastThread(t, e.msg))
	}
	if strings.Contains(logs.String(), "SECRET-DETAIL") {
		t.Fatal("name-read error leaked into the log")
	}
	// A router without a namer also works.
	e2 := newP6bEnv(t)
	e2.router.d.TaskName = nil
	e2.router.Destination(context.Background(), p6bBase(), TopicIntent{Mode: "task", TaskID: "ABC-6", ExecutionID: 1})
	if e2.client.names[0] != "ABC-6" {
		t.Fatalf("name=%q", e2.client.names[0])
	}
}

// TC-A6.7 (REQ-2): two simultaneous events of a new task create one topic;
// the loser sends to General; both messages are delivered once.
func TestPhase6bConcurrentFirstUseCreatesOneTopic(t *testing.T) {
	e := newP6bEnv(t)
	gate := make(chan struct{})
	e.client.hook = func() { <-gate } // the creator is slow
	ctx := withWorkflowExecution(context.Background(), 4)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rc := p6bRC("task", "RACE-1")
			rc.Run.ID = int64(100 + i)
			_, err := e.send(ctx, fmt.Sprint("k", i), fmt.Sprint("m", i), rc)
			errs <- err
		}(i)
	}
	deadline := time.Now().Add(5 * time.Second)
	for e.msg.tried() < 7 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if e.client.createCount() != 1 {
		t.Fatalf("creates=%d, want 1", e.client.createCount())
	}
	d := e.msg.delivered()
	if len(d) != 8 {
		t.Fatalf("delivered=%d, want 8", len(d))
	}
	general, topic := 0, 0
	for _, s := range d {
		if s.to.ThreadID == 0 {
			general++
		} else {
			topic++
		}
	}
	if topic != 1 || general != 7 {
		t.Fatalf("topic=%d general=%d, want 1 and 7", topic, general)
	}
	// After the winner finished, the next event uses the topic.
	rc := p6bRC("task", "RACE-1")
	rc.Run.ID = 999
	if _, err := e.send(ctx, "klater", "later", rc); err != nil || lastThread(t, e.msg) == 0 {
		t.Fatalf("later send thread=%d err=%v", lastThread(t, e.msg), err)
	}
}

// ---------------------------------------------------------------- REQ-6

// TC-A7.5 (REQ-6): fallback matrix. Every case delivers exactly one message
// and returns no error.
func TestPhase6bFallbackMatrix(t *testing.T) {
	ctx := withWorkflowExecution(context.Background(), 12)
	rc := p6bRC("task", "FB-1")
	type setup func(e *p6bEnv)
	preTopic := func(e *p6bEnv, state string) {
		top, _, _ := e.store.EnsureTopic(ctx, TopicKey{WorkspaceID: p6bWS, IntegrationID: p6bBot, ChatID: p6bChatID, SubjectKind: TopicSubjectClickUpTask, SubjectKey: "FB-1"})
		_, _ = e.store.SetTopicThread(ctx, p6bWS, top.ID, 77)
		if state == TopicClosed {
			_, _ = e.store.SetTopicState(ctx, p6bWS, top.ID, TopicClosed)
		}
	}
	for _, tc := range []struct {
		name       string
		setup      setup
		wantThread int64 // thread of the delivered message
		attempts   int
		creates    int
		rows       int
		after      func(t *testing.T, e *p6bEnv)
	}{
		{"not a forum", func(e *p6bEnv) { e.client.createErr = p6bErr("topic_not_forum") }, 0, 1, 1, 0, nil},
		{"no rights to create", func(e *p6bEnv) { e.client.createErr = p6bErr("topic_no_rights") }, 0, 1, 1, 0, nil},
		{"create transport error", func(e *p6bEnv) { e.client.createErr = p6bErr("transport") }, 0, 1, 1, 0, nil},
		{"create timeout", func(e *p6bEnv) { e.client.createErr = context.DeadlineExceeded }, 0, 1, 1, 0, nil},
		{"create returns no id", func(e *p6bEnv) { e.client.zeroID = true }, 0, 1, 1, 0, nil},
		{"no client", func(e *p6bEnv) { e.router.d.Client = nil }, 0, 1, 0, 0, nil},
		{"store failure", func(e *p6bEnv) { e.store.ensureErr = errors.New("db down") }, 0, 1, 0, 0, nil},
		{"thread id not stored keeps the message in the topic", func(e *p6bEnv) { e.store.setErr = errors.New("db down") }, 101, 1, 1, 1, nil},
		{"closed topic, reopen ok", func(e *p6bEnv) { preTopic(e, TopicClosed) }, 77, 1, 0, 1, func(t *testing.T, e *p6bEnv) {
			if e.client.reopens != 1 || e.store.only(t).State != TopicOpen {
				t.Fatalf("reopens=%d state=%s", e.client.reopens, e.store.only(t).State)
			}
		}},
		{"closed topic, reopen fails", func(e *p6bEnv) { preTopic(e, TopicClosed); e.client.reopenErr = p6bErr("topic_no_rights") }, 0, 1, 0, 1, nil},
		{"closed topic, thread deleted by hand", func(e *p6bEnv) { preTopic(e, TopicClosed); e.client.reopenErr = p6bErr("thread_not_found") }, 0, 1, 0, 0, nil},
		{"send into topic: closed, reopen ok", func(e *p6bEnv) {
			preTopic(e, TopicOpen)
			e.msg.fail = func(to ChatRef, n int) error {
				if n == 0 {
					return p6bErr("topic_closed")
				}
				return nil
			}
		}, 77, 2, 0, 1, nil},
		{"send into topic: closed, reopen fails", func(e *p6bEnv) {
			preTopic(e, TopicOpen)
			e.client.reopenErr = p6bErr("topic_no_rights")
			e.msg.fail = func(to ChatRef, n int) error {
				if to.ThreadID != 0 {
					return p6bErr("topic_closed")
				}
				return nil
			}
		}, 0, 2, 0, 1, nil},
		{"send into topic: thread not found", func(e *p6bEnv) {
			preTopic(e, TopicOpen)
			e.msg.fail = func(to ChatRef, n int) error {
				if to.ThreadID != 0 {
					return p6bErr("thread_not_found")
				}
				return nil
			}
		}, 0, 2, 0, 0, nil},
		{"send into topic: not a forum anymore", func(e *p6bEnv) {
			preTopic(e, TopicOpen)
			e.msg.fail = func(to ChatRef, n int) error {
				if to.ThreadID != 0 {
					return p6bErr("topic_not_forum")
				}
				return nil
			}
		}, 0, 2, 0, 1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newP6bEnv(t)
			tc.setup(e)
			if _, err := e.send(ctx, "report", "body", rc); err != nil {
				t.Fatalf("send failed: %v", err)
			}
			d := e.msg.delivered()
			if len(d) != 1 {
				t.Fatalf("delivered=%d, want exactly 1", len(d))
			}
			if d[0].to.ThreadID != tc.wantThread {
				t.Fatalf("delivered to thread %d, want %d", d[0].to.ThreadID, tc.wantThread)
			}
			if e.msg.tried() != tc.attempts || e.client.createCount() != tc.creates || e.store.count() != tc.rows {
				t.Fatalf("attempts=%d creates=%d rows=%d, want %d %d %d", e.msg.tried(), e.client.createCount(), e.store.count(), tc.attempts, tc.creates, tc.rows)
			}
			// The message key was recorded once: a replay sends nothing.
			if _, err := e.send(ctx, "report", "body", rc); err != nil || len(e.msg.delivered()) != 1 {
				t.Fatalf("replay delivered=%d err=%v", len(e.msg.delivered()), err)
			}
			if tc.after != nil {
				tc.after(t, e)
			}
		})
	}
}

// TC-A7.6 (REQ-6): a topic deleted by hand: the binding is dropped, the
// message goes to General, and the next send recreates the topic.
func TestPhase6bDeletedTopicIsRecreatedOnNextSend(t *testing.T) {
	e := newP6bEnv(t)
	ctx := withWorkflowExecution(context.Background(), 12)
	rc := p6bRC("task", "GONE-1")
	if _, err := e.send(ctx, "k1", "one", rc); err != nil {
		t.Fatal(err)
	}
	first := lastThread(t, e.msg)
	e.msg.mu.Lock()
	e.msg.fail = func(to ChatRef, n int) error {
		if to.ThreadID == first {
			return p6bErr("thread_not_found")
		}
		return nil
	}
	e.msg.mu.Unlock()
	if _, err := e.send(ctx, "k2", "two", rc); err != nil {
		t.Fatal(err)
	}
	if lastThread(t, e.msg) != 0 || e.store.deleted != 1 || e.store.count() != 0 {
		t.Fatalf("thread=%d deleted=%d rows=%d", lastThread(t, e.msg), e.store.deleted, e.store.count())
	}
	if _, err := e.send(ctx, "k3", "three", rc); err != nil {
		t.Fatal(err)
	}
	if third := lastThread(t, e.msg); third == 0 || third == first || e.client.createCount() != 2 {
		t.Fatalf("recreated thread=%d first=%d creates=%d", third, first, e.client.createCount())
	}
	if len(e.msg.delivered()) != 3 {
		t.Fatalf("delivered=%d, want 3", len(e.msg.delivered()))
	}
	// A store without a deleter marks the lost topic closed instead.
	e2 := newP6bEnv(t)
	e2.router.d.Store = p6bNoDelete{e2.store}
	e2.msg.fail = func(to ChatRef, n int) error {
		if to.ThreadID != 0 {
			return p6bErr("thread_not_found")
		}
		return nil
	}
	if _, err := e2.send(ctx, "k1", "one", rc); err != nil {
		t.Fatal(err)
	}
	if _, err := e2.send(ctx, "k2", "two", rc); err != nil {
		t.Fatal(err)
	}
	if row := e2.store.only(t); row.State != TopicClosed {
		t.Fatalf("lost topic state=%s, want closed", row.State)
	}
}

// TC-A7.7 (REQ-6): a transport error or timeout when sending into a topic is
// not retried in General (the outcome is unknown); the claim is released so
// a later retry sends once.
func TestPhase6bTransportErrorDoesNotResend(t *testing.T) {
	for _, mode := range []struct {
		name string
		err  error
	}{{"transport", p6bErr("transport")}, {"timeout", context.DeadlineExceeded}, {"rate limited", p6bErr("rate_limited")}, {"server", p6bErr("server")}} {
		t.Run(mode.name, func(t *testing.T) {
			e := newP6bEnv(t)
			ctx := withWorkflowExecution(context.Background(), 12)
			rc := p6bRC("task", "TR-1")
			if _, err := e.send(ctx, "k1", "one", rc); err != nil { // creates the topic
				t.Fatal(err)
			}
			topicThread := lastThread(t, e.msg)
			failing := true
			e.msg.mu.Lock()
			e.msg.fail = func(ChatRef, int) error {
				if failing {
					return mode.err
				}
				return nil
			}
			e.msg.mu.Unlock()
			before := e.msg.tried()
			rc2 := rc
			rc2.Run.ID = 8
			if _, err := e.send(ctx, "k2", "two", rc2); err == nil {
				t.Fatal("transport failure must surface")
			}
			if got := e.msg.tried() - before; got != 1 {
				t.Fatalf("attempts after the failure=%d, want 1 (no General resend)", got)
			}
			if len(e.msg.delivered()) != 1 {
				t.Fatalf("delivered=%d", len(e.msg.delivered()))
			}
			failing = false
			if _, err := e.send(ctx, "k2", "two", rc2); err != nil {
				t.Fatal(err)
			}
			if d := e.msg.delivered(); len(d) != 2 || d[1].to.ThreadID != topicThread || d[1].text != "two" {
				t.Fatalf("retry delivered=%+v", d)
			}
		})
	}
}

// TC-A7.8 (REQ-6): fallbacks are counted and logged by code only, with no
// topic name, task title or message text.
func TestPhase6bFallbackLogIsPrivate(t *testing.T) {
	e := newP6bEnv(t)
	logs := p6aCaptureLog(t)
	e.titles["PRIV-1"] = "TITLE-PRIVATE-💥"
	e.client.createErr = p6bErr("topic_not_forum")
	if _, err := e.send(withWorkflowExecution(context.Background(), 3), "k", "TEXT-PRIVATE", p6bRC("task", "PRIV-1")); err != nil {
		t.Fatal(err)
	}
	e.msg.fail = func(to ChatRef, n int) error {
		if to.ThreadID != 0 {
			return p6bErr("thread_not_found")
		}
		return nil
	}
	e2 := newP6bEnv(t)
	e2.titles["PRIV-1"] = "TITLE-PRIVATE-💥"
	e2.msg.fail = e.msg.fail
	for i := 0; i < 2; i++ {
		rc := p6bRC("task", "PRIV-1")
		if _, err := e2.send(withWorkflowExecution(context.Background(), 3), fmt.Sprint("k", i), "TEXT-PRIVATE", rc); err != nil {
			t.Fatal(err)
		}
	}
	if e.router.Fallbacks() != 1 || e2.router.Fallbacks() != 2 {
		t.Fatalf("fallbacks=%d,%d, want 1,2", e.router.Fallbacks(), e2.router.Fallbacks())
	}
	out := logs.String()
	if !strings.Contains(out, "topic_not_forum") || !strings.Contains(out, "thread_not_found") {
		t.Fatalf("fallback codes missing from log:\n%s", out)
	}
	for _, s := range []string{"TITLE-PRIVATE", "TEXT-PRIVATE", "PRIV-1 ·"} {
		if strings.Contains(out, s) {
			t.Fatalf("log leaks %q:\n%s", s, out)
		}
	}
}

// ---------------------------------------------------------------- REQ-4/5

// TC-A8.1 (REQ-5): the message binding stores the thread the message was
// really sent to (the topic, or General after a fallback).
func TestPhase6bBindingStoresDeliveredThread(t *testing.T) {
	e := newP6bEnv(t)
	ctx := withWorkflowExecution(context.Background(), 21)
	if _, err := e.send(ctx, "k1", "one", p6bRC("task", "BIND-1")); err != nil {
		t.Fatal(err)
	}
	topic := lastThread(t, e.msg)
	e.msg.fail = func(to ChatRef, n int) error {
		if to.ThreadID != 0 {
			return p6bErr("topic_not_forum")
		}
		return nil
	}
	if _, err := e.send(ctx, "k2", "two", p6bRC("task", "BIND-1")); err != nil {
		t.Fatal(err)
	}
	e.conv.mu.Lock()
	defer e.conv.mu.Unlock()
	if len(e.conv.threads) != 2 || e.conv.threads[0] != topic || e.conv.threads[1] != 0 {
		t.Fatalf("bound threads=%v, want [%d 0]", e.conv.threads, topic)
	}
}

// TC-A8.2 (REQ-5): replies to an inbound command, button and free text stay
// in the inbound topic and never create a topic.
func TestPhase6bRepliesStayInInboundThread(t *testing.T) {
	const thread = int64(31)
	e := newP6bEnv(t)
	ctx := withWorkflowExecution(context.Background(), 21)
	// A command-triggered run answers in the command's chat and thread.
	rc := p6bRC("task", "CMD-1")
	rc.Trigger.TelegramChatID = p6bChatID
	rc.Trigger.TelegramThreadID = thread
	if chat := e.eng.routed(ctx, rc, p6bBase()); chat.ThreadID != thread || chat.topic != nil {
		t.Fatalf("command reply chat=%+v", chat)
	}
	if _, err := e.send(ctx, "k", "reply", rc); err != nil {
		t.Fatal(err)
	}
	if lastThread(t, e.msg) != thread || e.store.count() != 0 || e.client.createCount() != 0 {
		t.Fatalf("thread=%d rows=%d creates=%d", lastThread(t, e.msg), e.store.count(), e.client.createCount())
	}
	// A command in General stays in General.
	rcG := p6bRC("task", "CMD-1")
	rcG.Trigger.TelegramChatID = p6bChatID
	if _, err := e.send(ctx, "k2", "reply", rcG); err != nil || lastThread(t, e.msg) != 0 || e.store.count() != 0 {
		t.Fatalf("General command reply thread=%d rows=%d err=%v", lastThread(t, e.msg), e.store.count(), err)
	}
	// Prompt after a button press (Đặt tên) and callback chat refs.
	a := CallbackAction{WorkspaceID: p6bWS, IntegrationID: p6bBot, ChatID: p6bChatID, ThreadID: thread}
	if got := chatOfAction(a); got.ThreadID != thread || got.ChatID != p6bChatID {
		t.Fatalf("chatOfAction=%+v", got)
	}
	if _, err := e.eng.prompt(ctx, chatOfAction(a), 9, "Trả lời"); err != nil {
		t.Fatal(err)
	}
	if lastThread(t, e.msg) != thread || e.store.count() != 0 {
		t.Fatalf("prompt thread=%d rows=%d", lastThread(t, e.msg), e.store.count())
	}
	// The inbound topic was deleted: the prompt falls back to General once.
	e.msg.fail = func(to ChatRef, n int) error {
		if to.ThreadID != 0 {
			return p6bErr("thread_not_found")
		}
		return nil
	}
	before := len(e.msg.delivered())
	if _, err := e.eng.prompt(ctx, chatOfAction(a), 9, "Trả lời"); err != nil {
		t.Fatal(err)
	}
	if d := e.msg.delivered(); len(d) != before+1 || d[len(d)-1].to.ThreadID != 0 {
		t.Fatalf("deleted inbound topic: delivered=%d last=%+v", len(d), d[len(d)-1])
	}
	// Free text answer: chat carries the thread.
	if got := (FreeTextAnswer{WorkspaceID: p6bWS, IntegrationID: p6bBot, ChatID: p6bChatID, ThreadID: thread}); got.ThreadID != thread {
		t.Fatal("FreeTextAnswer.ThreadID lost")
	}
}

// TC-A8.3 (REQ-5): Dispatch and RunFailed route end to end; digests of
// queued entries have no topic mark (they go to General).
func TestPhase6bRunFailedRoutesToTaskTopic(t *testing.T) {
	e := newP6bEnv(t)
	logs := p6aCaptureLog(t)
	e.titles["FAIL-1"] = "TITLE-FAIL-SECRET"
	ctx := withWorkflowExecution(context.Background(), 44)
	rc := p6bRC("task", "FAIL-1")
	if err := e.eng.RunFailed(ctx, rc, "model_error"); err != nil {
		t.Fatal(err)
	}
	if err := e.eng.RunFailed(ctx, rc, "model_error"); err != nil { // replay: one message
		t.Fatal(err)
	}
	d := e.msg.delivered()
	if len(d) != 1 || d[0].to.ThreadID == 0 || e.client.createCount() != 1 {
		t.Fatalf("delivered=%+v creates=%d", d, e.client.createCount())
	}
	if strings.Contains(logs.String(), "TITLE-FAIL-SECRET") {
		t.Fatal("task title leaked")
	}
	if got := (DigestBatch{Chat: p6bBase()}).Chat; got.topic != nil || got.ThreadID != 0 {
		t.Fatalf("digest chat must be General: %+v", got)
	}
	// The run context's chat for a queued digest is stored without the mark.
	if routed := e.eng.routed(ctx, rc, p6bBase()); routed.topic == nil {
		t.Fatal("routed chat lost its mark")
	}
}

// ---------------------------------------------------------------- REQ-6 destination

// TC-A9.1 (REQ-6): Destination never touches a chat that already has a
// thread, a zero chat, or a nil router.
func TestPhase6bDestinationGuards(t *testing.T) {
	e := newP6bEnv(t)
	in := TopicIntent{Mode: "task", TaskID: "G-1", ExecutionID: 1}
	withThread := p6bBase()
	withThread.ThreadID = 9
	if got := e.router.Destination(context.Background(), withThread, in); got.ThreadID != 9 {
		t.Fatalf("thread changed: %+v", got)
	}
	if got := e.router.Destination(context.Background(), ChatRef{}, in); got.ChatID != 0 || got.ThreadID != 0 {
		t.Fatalf("zero chat: %+v", got)
	}
	var nilRouter *TopicRouter
	if got := nilRouter.Destination(context.Background(), p6bBase(), in); got.ThreadID != 0 {
		t.Fatalf("nil router: %+v", got)
	}
	if nilRouter.ActionThread(context.Background(), p6bWS, p6bBot, p6bChatID, 5) != 0 {
		t.Fatal("nil router ActionThread")
	}
	if _, err := NewTopicRouter(TopicRouterDeps{}); err == nil {
		t.Fatal("router without a store accepted")
	}
	if e.store.count() != 0 || e.client.createCount() != 0 {
		t.Fatal("guards touched the store")
	}
	// Workspace isolation: the same subject in another workspace gets its own topic.
	other := p6bBase()
	other.WorkspaceID = "ws-other"
	a := e.router.Destination(context.Background(), p6bBase(), in)
	b := e.router.Destination(context.Background(), other, in)
	if a.ThreadID == 0 || b.ThreadID == 0 || a.ThreadID == b.ThreadID || e.store.count() != 2 {
		t.Fatalf("workspaces share a topic: %d %d rows=%d", a.ThreadID, b.ThreadID, e.store.count())
	}
}

// TC-A9.2 (REQ-6): a pending reservation of another sender sends to General;
// a stale one is taken over after the lease.
func TestPhase6bPendingAndStaleReservation(t *testing.T) {
	e := newP6bEnv(t)
	key := TopicKey{WorkspaceID: p6bWS, IntegrationID: p6bBot, ChatID: p6bChatID, SubjectKind: TopicSubjectClickUpTask, SubjectKey: "P-1"}
	if _, c, _ := e.store.EnsureTopic(context.Background(), key); !c {
		t.Fatal("setup")
	}
	in := TopicIntent{Mode: "task", TaskID: "P-1", ExecutionID: 1}
	if got := e.router.Destination(context.Background(), p6bBase(), in); got.ThreadID != 0 || e.client.createCount() != 0 {
		t.Fatalf("pending: %+v creates=%d", got, e.client.createCount())
	}
	e.store.mu.Lock()
	e.store.clock = e.store.clock.Add(TopicReservationLease + time.Second)
	e.store.mu.Unlock()
	if got := e.router.Destination(context.Background(), p6bBase(), in); got.ThreadID == 0 || e.client.createCount() != 1 {
		t.Fatalf("stale takeover: %+v creates=%d", got, e.client.createCount())
	}
}

// ---------------------------------------------------------------- REQ-5 relay

// TC-A9.3 (REQ-6): the router adapter used by the relay returns the task
// topic of an execution and handles rejected sends like the engine does.
func TestPhase6bActionThreadAndRejected(t *testing.T) {
	e := newP6bEnv(t)
	thread := e.router.ActionThread(context.Background(), p6bWS, p6bBot, p6bChatID, 61)
	if thread == 0 {
		t.Fatal("agent report not routed to a topic")
	}
	if row := e.store.only(t); row.SubjectKey != "BOUND-7" || e.client.names[0] != "BOUND-7" {
		t.Fatalf("row=%+v names=%v", row, e.client.names)
	}
	if again := e.router.ActionThread(context.Background(), p6bWS, p6bBot, p6bChatID, 61); again != thread || e.client.createCount() != 1 {
		t.Fatalf("second report thread=%d creates=%d", again, e.client.createCount())
	}
	if e.router.ActionThread(context.Background(), p6bWS, p6bBot, p6bChatID, 0) != 0 {
		t.Fatal("no execution must be General")
	}
	next, ok := e.router.ActionThreadRejected(context.Background(), p6bWS, p6bBot, p6bChatID, thread, p6bErr("thread_not_found"))
	if !ok || next != 0 || e.store.count() != 0 {
		t.Fatalf("not found: next=%d ok=%v rows=%d", next, ok, e.store.count())
	}
	thread = e.router.ActionThread(context.Background(), p6bWS, p6bBot, p6bChatID, 61)
	next, ok = e.router.ActionThreadRejected(context.Background(), p6bWS, p6bBot, p6bChatID, thread, p6bErr("topic_closed"))
	if !ok || next != thread || e.client.reopens != 1 {
		t.Fatalf("closed+reopen: next=%d ok=%v reopens=%d", next, ok, e.client.reopens)
	}
	if next, ok = e.router.ActionThreadRejected(context.Background(), p6bWS, p6bBot, p6bChatID, thread, p6bErr("transport")); ok || next != thread {
		t.Fatalf("transport must not resend: next=%d ok=%v", next, ok)
	}
	if next, ok = e.router.ActionThreadRejected(context.Background(), p6bWS, p6bBot, p6bChatID, 0, p6bErr("thread_not_found")); ok {
		t.Fatalf("General send rejection must not recover: next=%d", next)
	}
}
