package behavior

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Phase 6b batch B: topic lifecycle (REQ-9), router evaluation (REQ-13) and
// small reserved-name checks. Fakes only.

const (
	lcWS    = "ws-lc"
	lcOther = "ws-lc-other"
)

type lcStore struct {
	TopicStore
	mu      sync.Mutex
	topics  []Topic
	listErr error
	setErr  error
	sets    []string
}

func (s *lcStore) ListTopics(_ context.Context, ws string) ([]Topic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []Topic
	for _, t := range s.topics {
		if t.WorkspaceID == ws {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *lcStore) SetTopicState(_ context.Context, ws string, id int64, state string) (Topic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setErr != nil {
		return Topic{}, s.setErr
	}
	for i := range s.topics {
		if s.topics[i].ID == id && s.topics[i].WorkspaceID == ws {
			s.topics[i].State = state
			s.sets = append(s.sets, fmt.Sprintf("%d=%s", id, state))
			return s.topics[i], nil
		}
	}
	return Topic{}, ErrNotFound
}

func (s *lcStore) state(id int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.topics {
		if t.ID == id {
			return t.State
		}
	}
	return ""
}

// lcDeleter fails the test if a lifecycle ever tries to delete a topic.
func (s *lcStore) DeleteTopic(context.Context, string, int64) error {
	panic("lifecycle must never delete a topic")
}

type lcCloser struct {
	mu     sync.Mutex
	closed []string
	err    error
}

func (c *lcCloser) CloseTopic(_ context.Context, to ChatRef, thread int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = append(c.closed, fmt.Sprintf("%s/%s/%d/%d", to.WorkspaceID, to.IntegrationID, to.ChatID, thread))
	return c.err
}

func (c *lcCloser) calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.closed...)
}

type lcExec struct {
	mu     sync.Mutex
	byTask map[string][]int64
	states map[int64]workflow.State
	listEr error
	getEr  map[int64]error
}

func (e *lcExec) ExecutionsByResource(_ context.Context, ws, kind, id string) ([]int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if kind != "clickup.task" {
		return nil, errors.New("wrong kind")
	}
	if e.listEr != nil {
		return nil, e.listEr
	}
	return append([]int64(nil), e.byTask[ws+"/"+id]...), nil
}

func (e *lcExec) GetExecutionDetail(_ context.Context, ws string, id int64) (workflow.ExecutionDetail, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.getEr[id]; err != nil {
		return workflow.ExecutionDetail{}, err
	}
	st, ok := e.states[id]
	if !ok {
		return workflow.ExecutionDetail{}, workflow.ErrNotFound
	}
	return phase6BDetail(id, ws, st), nil
}

func (e *lcExec) set(id int64, st workflow.State) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.states[id] = st
}

type lcEnv struct {
	store  *lcStore
	closer *lcCloser
	exec   *lcExec
	l      *TopicLifecycle
	clock  time.Time
	reads  []string
	status string
	stype  string
	readEr error
	wss    []string
}

func lcTopic(id int64, ws, task string, thread int64) Topic {
	return Topic{ID: id, WorkspaceID: ws, IntegrationID: "bot-lc", ChatID: -1001234567890, ThreadID: thread, SubjectKind: TopicSubjectClickUpTask, SubjectKey: task, State: TopicOpen}
}

func newLCEnv(t *testing.T, statuses string, topics ...Topic) *lcEnv {
	t.Helper()
	e := &lcEnv{store: &lcStore{topics: topics}, closer: &lcCloser{}, clock: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC),
		exec: &lcExec{byTask: map[string][]int64{}, states: map[int64]workflow.State{}, getEr: map[int64]error{}}, wss: []string{lcWS, lcOther}}
	var err error
	e.l, err = NewTopicLifecycle(TopicLifecycleDeps{
		Store: e.store, Closer: e.closer, Executions: e.exec, Reader: e.exec, CloseStatuses: statuses,
		Workspaces: func(context.Context) ([]string, error) { return e.wss, nil },
		ReadTask: func(_ context.Context, ws, task string) (string, string, error) {
			e.reads = append(e.reads, ws+"/"+task)
			return e.status, e.stype, e.readEr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.l.now = func() time.Time { return e.clock }
	return e
}

func (e *lcEnv) event(ws, task, status, stype string) {
	e.l.StatusChanged(ws, task, status, stype)
	e.l.bg.Wait()
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&b)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	return &b
}

// TC-B1-30 (REQ-9): done type, name list and their precedence.
func TestPhase6bB1_LifecycleDoneDetection(t *testing.T) {
	cases := []struct {
		name, statuses, status, stype string
		closes                        bool
	}{
		{"type done", "", "Shipped", "done", true},
		{"type closed", "", "Whatever", "closed", true},
		{"type is case-insensitive", "", "x", " DONE ", true},
		{"type custom beats a matching name", "", "Complete", "custom", false},
		{"type open beats a matching name", "", "closed", "open", false},
		{"type done beats a missing name", "complete", "Totally different", "done", true},
		{"default names without type", "", "Complete", "", true},
		{"default closed", "", "closed", "", true},
		{"default done with spaces", "", "  Done ", "", true},
		{"default miss", "", "In Progress", "", false},
		{"custom list replaces default", "shipped, released", "Shipped", "", true},
		{"custom list drops default names", "shipped", "Complete", "", false},
		{"custom list case and space", " SHIPPED ,released", "released", "", true},
		{"empty status never closes", "", "", "", false},
		{"substring is not a match", "", "not done yet", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newLCEnv(t, c.statuses, lcTopic(1, lcWS, "T1", 10))
			e.event(lcWS, "T1", c.status, c.stype)
			got := e.closer.calls()
			if c.closes != (len(got) == 1) {
				t.Fatalf("closes=%v calls=%v", c.closes, got)
			}
			if c.closes && (e.store.state(1) != TopicClosed || got[0] != "ws-lc/bot-lc/-1001234567890/10") {
				t.Fatalf("state=%q call=%q", e.store.state(1), got[0])
			}
			if !c.closes && e.store.state(1) != TopicOpen {
				t.Fatalf("state=%q", e.store.state(1))
			}
		})
	}
}

// TC-B1-31 (REQ-9): an active execution blocks the close; the sweep closes
// after it ends, without reading ClickUp again.
func TestPhase6bB1_LifecycleActiveBlocksThenSweepCloses(t *testing.T) {
	e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
	e.exec.byTask[lcWS+"/T1"] = []int64{7, 8}
	e.exec.states[7] = workflow.StateCompleted
	e.exec.states[8] = workflow.StateRunning
	e.event(lcWS, "T1", "Complete", "done")
	if len(e.closer.calls()) != 0 || e.store.state(1) != TopicOpen {
		t.Fatalf("closed while active: %v", e.closer.calls())
	}
	e.l.Sweep(context.Background())
	if len(e.closer.calls()) != 0 {
		t.Fatal("sweep closed while active")
	}
	e.exec.set(8, workflow.StateFailed)
	e.l.Sweep(context.Background())
	if got := e.closer.calls(); len(got) != 1 || e.store.state(1) != TopicClosed {
		t.Fatalf("calls=%v state=%q", got, e.store.state(1))
	}
	if len(e.reads) != 0 {
		t.Fatalf("a task already known as done was read: %v", e.reads)
	}
	// A second sweep does not close a closed topic again.
	e.l.Sweep(context.Background())
	if len(e.closer.calls()) != 1 {
		t.Fatalf("closed twice: %v", e.closer.calls())
	}
	// Waiting and paused states count as active.
	for _, st := range []workflow.State{workflow.StateWaitingForUser, workflow.StateWaitingForApproval, workflow.StateQueued} {
		e2 := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		e2.exec.byTask[lcWS+"/T1"] = []int64{9}
		e2.exec.states[9] = st
		if st.IsTerminal() {
			continue
		}
		e2.event(lcWS, "T1", "Complete", "done")
		if len(e2.closer.calls()) != 0 {
			t.Fatalf("state %s did not block the close", st)
		}
	}
}

// TC-B1-32 (REQ-9, abuse): unknown activity keeps the topic open.
func TestPhase6bB1_LifecycleUnknownActivityKeepsOpen(t *testing.T) {
	t.Run("binding lookup error", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		e.exec.listEr = errors.New("db down")
		e.event(lcWS, "T1", "Complete", "done")
		e.l.Sweep(context.Background())
		if len(e.closer.calls()) != 0 {
			t.Fatalf("closed: %v", e.closer.calls())
		}
	})
	t.Run("execution read error", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		e.exec.byTask[lcWS+"/T1"] = []int64{7}
		e.exec.getEr[7] = errors.New("db down")
		e.event(lcWS, "T1", "Complete", "done")
		if len(e.closer.calls()) != 0 {
			t.Fatalf("closed: %v", e.closer.calls())
		}
	})
	t.Run("missing execution is not activity", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		e.exec.byTask[lcWS+"/T1"] = []int64{7}
		e.event(lcWS, "T1", "Complete", "done")
		if len(e.closer.calls()) != 1 {
			t.Fatalf("calls=%v", e.closer.calls())
		}
	})
	t.Run("no execution readers", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		e.l.d.Executions, e.l.d.Reader = nil, nil
		e.event(lcWS, "T1", "Complete", "done")
		if len(e.closer.calls()) != 0 {
			t.Fatalf("closed without activity data: %v", e.closer.calls())
		}
	})
	t.Run("too many bindings", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		ids := make([]int64, 0, 300)
		for i := int64(1); i <= 300; i++ {
			ids = append(ids, i)
			e.exec.states[i] = workflow.StateCompleted
		}
		e.exec.byTask[lcWS+"/T1"] = ids
		e.event(lcWS, "T1", "Complete", "done")
		if len(e.closer.calls()) != 0 {
			t.Fatal("closed although activity is unknown")
		}
	})
}

// TC-B1-33 (REQ-9): scope. Only open task topics of the event's workspace and
// task close; nothing else.
func TestPhase6bB1_LifecycleScope(t *testing.T) {
	workflowTopic := lcTopic(3, lcWS, "T1", 12)
	workflowTopic.SubjectKind = TopicSubjectWorkflow
	fixed := lcTopic(4, lcWS, "T1", 13)
	fixed.SubjectKind = TopicSubjectFixed
	reserved := lcTopic(5, lcWS, "T1", 0)
	already := lcTopic(6, lcWS, "T1", 14)
	already.State = TopicClosed
	e := newLCEnv(t, "",
		lcTopic(1, lcWS, "T1", 10), lcTopic(2, lcOther, "T1", 11), workflowTopic, fixed, reserved, already,
		lcTopic(7, lcWS, "T2", 15))
	e.event(lcWS, "T1", "Complete", "done")
	if got := e.closer.calls(); len(got) != 1 || !strings.HasSuffix(got[0], "/10") {
		t.Fatalf("calls=%v", got)
	}
	for _, id := range []int64{2, 3, 4, 5, 7} {
		if e.store.state(id) != TopicOpen {
			t.Fatalf("topic %d state=%q", id, e.store.state(id))
		}
	}
	// A done event for a workspace id that is empty or a task id that is
	// invalid is ignored.
	e2 := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
	e2.event("", "T1", "Complete", "done")
	e2.event(lcWS, "bad id!", "Complete", "done")
	e2.event(lcWS, "", "Complete", "done")
	if len(e2.closer.calls()) != 0 {
		t.Fatalf("calls=%v", e2.closer.calls())
	}
	var nilLC *TopicLifecycle
	nilLC.StatusChanged(lcWS, "T1", "Complete", "done")
	nilLC.Sweep(context.Background())
}

// TC-B1-34 (REQ-9): close errors. A gone or already closed topic is marked
// closed; any other error keeps it open for the next try; never deletes.
func TestPhase6bB1_LifecycleCloseErrors(t *testing.T) {
	for _, tc := range []struct {
		code   string
		closed bool
	}{
		{"thread_not_found", true}, {"topic_closed", true}, {"topic_no_rights", false}, {"transport", false}, {"rate_limited", false},
	} {
		t.Run(tc.code, func(t *testing.T) {
			e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
			e.closer.err = p6bErr(tc.code)
			e.event(lcWS, "T1", "Complete", "done")
			if got := e.store.state(1) == TopicClosed; got != tc.closed {
				t.Fatalf("closed=%v want %v", got, tc.closed)
			}
		})
	}
	t.Run("plain error", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		e.closer.err = errors.New("secret-token-xyz exploded")
		logs := captureLog(t)
		e.event(lcWS, "T1", "Complete", "done")
		if e.store.state(1) != TopicOpen || strings.Contains(logs.String(), "secret-token-xyz") {
			t.Fatalf("state=%q logs=%q", e.store.state(1), logs.String())
		}
	})
	t.Run("state not stored is retried next sweep", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		e.store.setErr = errors.New("db down")
		e.event(lcWS, "T1", "Complete", "done")
		e.store.setErr = nil
		e.l.Sweep(context.Background())
		if e.store.state(1) != TopicClosed || len(e.closer.calls()) != 2 {
			t.Fatalf("state=%q calls=%v", e.store.state(1), e.closer.calls())
		}
	})
	t.Run("list error closes nothing", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		e.store.listErr = errors.New("db down")
		e.event(lcWS, "T1", "Complete", "done")
		e.l.Sweep(context.Background())
		if len(e.closer.calls()) != 0 {
			t.Fatal("closed without a topic list")
		}
	})
	t.Run("logs name ids and a code only", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		logs := captureLog(t)
		e.event(lcWS, "T1", "Secret Status Name", "done")
		if out := logs.String(); !strings.Contains(out, "closed topic 1 (task_done)") || strings.Contains(out, "T1") || strings.Contains(out, "Secret Status Name") {
			t.Fatalf("log=%q", out)
		}
	})
}

// TC-B1-35 (REQ-9): the sweep reads at most 5 tasks, never the same task
// within 30 minutes, keeps the topic open on a read error and closes on done.
func TestPhase6bB1_LifecycleSweepThrottle(t *testing.T) {
	var topics []Topic
	for i := int64(1); i <= 7; i++ {
		topics = append(topics, lcTopic(i, lcWS, fmt.Sprintf("T%d", i), 10+i))
	}
	e := newLCEnv(t, "", topics...)
	e.status, e.stype = "In Progress", "open"
	e.l.Sweep(context.Background())
	if len(e.reads) != 5 {
		t.Fatalf("first sweep reads=%d (%v)", len(e.reads), e.reads)
	}
	e.l.Sweep(context.Background())
	if len(e.reads) != 7 {
		t.Fatalf("second sweep total reads=%d, want the two unread tasks only", len(e.reads))
	}
	seen := map[string]int{}
	for _, r := range e.reads {
		seen[r]++
	}
	for r, n := range seen {
		if n != 1 {
			t.Fatalf("%s read %d times", r, n)
		}
	}
	e.clock = e.clock.Add(29 * time.Minute)
	e.l.Sweep(context.Background())
	if len(e.reads) != 7 {
		t.Fatalf("read again after 29 minutes: %d", len(e.reads))
	}
	// After 30 minutes the done task is found, at most 5 per sweep.
	e.clock = e.clock.Add(2 * time.Minute)
	e.status, e.stype = "Complete", "done"
	e.l.Sweep(context.Background())
	if len(e.reads) != 12 {
		t.Fatalf("reads after 31 minutes=%d, want 12", len(e.reads))
	}
	if got := len(e.closer.calls()); got != 5 {
		t.Fatalf("closed=%d, want 5 (one per read)", got)
	}
}

// TC-B1-36 (REQ-9): a read error keeps the topic open and is not retried
// immediately; a done read closes; a task with an active execution is not
// read at all.
func TestPhase6bB1_LifecycleSweepReads(t *testing.T) {
	t.Run("read error keeps open", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		e.readEr = errors.New("clickup down")
		e.status, e.stype = "Complete", "done"
		e.l.Sweep(context.Background())
		e.l.Sweep(context.Background())
		if len(e.reads) != 1 || len(e.closer.calls()) != 0 || e.store.state(1) != TopicOpen {
			t.Fatalf("reads=%d closes=%v", len(e.reads), e.closer.calls())
		}
		// After the wait, when ClickUp is back, the topic closes.
		e.readEr = nil
		e.clock = e.clock.Add(31 * time.Minute)
		e.l.Sweep(context.Background())
		if e.store.state(1) != TopicClosed {
			t.Fatal("not closed after a good read")
		}
	})
	t.Run("done read closes", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		e.status, e.stype = "Complete", ""
		e.l.Sweep(context.Background())
		if e.store.state(1) != TopicClosed {
			t.Fatal("not closed")
		}
	})
	t.Run("open status stays open", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		e.status, e.stype = "Complete", "custom"
		e.l.Sweep(context.Background())
		if e.store.state(1) != TopicOpen {
			t.Fatal("closed a custom-type status")
		}
	})
	t.Run("active execution is not read", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		e.exec.byTask[lcWS+"/T1"] = []int64{7}
		e.exec.states[7] = workflow.StateRunning
		e.l.Sweep(context.Background())
		if len(e.reads) != 0 {
			t.Fatalf("reads=%v", e.reads)
		}
	})
	t.Run("unknown activity is not read", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		e.exec.listEr = errors.New("db down")
		e.l.Sweep(context.Background())
		if len(e.reads) != 0 {
			t.Fatalf("reads=%v", e.reads)
		}
	})
	t.Run("no ReadTask means no read and no close", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		e.l.d.ReadTask = nil
		e.l.Sweep(context.Background())
		if len(e.closer.calls()) != 0 {
			t.Fatal("closed without evidence")
		}
	})
	t.Run("one task with two chats is read once", func(t *testing.T) {
		two := lcTopic(2, lcWS, "T1", 20)
		two.ChatID = -1009999999999
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10), two)
		e.status, e.stype = "Complete", "done"
		e.l.Sweep(context.Background())
		if len(e.reads) != 1 || len(e.closer.calls()) != 2 {
			t.Fatalf("reads=%d closes=%v", len(e.reads), e.closer.calls())
		}
	})
	t.Run("workspace listing failure is survivable", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		e.l.d.Workspaces = func(context.Context) ([]string, error) { return nil, errors.New("db") }
		e.l.Sweep(context.Background())
		e.l.d.Workspaces = nil
		e.l.Sweep(context.Background())
	})
	t.Run("cancelled context stops the sweep", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		e.l.Sweep(ctx)
		if len(e.reads) != 0 || len(e.closer.calls()) != 0 {
			t.Fatal("work after cancel")
		}
	})
}

// TC-B1-37 (REQ-9): construction rules and Run.
func TestPhase6bB1_LifecycleConstructionAndRun(t *testing.T) {
	if _, err := NewTopicLifecycle(TopicLifecycleDeps{Closer: &lcCloser{}}); err == nil {
		t.Fatal("no store accepted")
	}
	if _, err := NewTopicLifecycle(TopicLifecycleDeps{Store: &lcStore{}}); err == nil {
		t.Fatal("no closer accepted")
	}
	e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
	e.status, e.stype = "Complete", "done"
	e.l.d.Interval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.l.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for e.store.state(1) != TopicClosed && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil || e.store.state(1) != TopicClosed {
		t.Fatalf("run err=%v state=%q", err, e.store.state(1))
	}
}

// TC-B1-38 (REQ-9): a lifecycle that never deletes, many events at once.
func TestPhase6bB1_LifecycleBurstNoDoubleClose(t *testing.T) {
	e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); e.l.StatusChanged(lcWS, "T1", "Complete", "done") }()
	}
	wg.Wait()
	e.l.bg.Wait()
	if e.store.state(1) != TopicClosed {
		t.Fatal("not closed")
	}
	// A burst may close more than once at the same instant (each call is
	// idempotent in Telegram); the stored result is one closed topic.
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	if len(e.store.topics) != 1 {
		t.Fatalf("topics=%d", len(e.store.topics))
	}
}

// TC-B1-39 (REQ-9): reopening on a later send: a closed topic is reopened by
// the next send and the lifecycle does not close it again until a new
// done-event or sweep read says so.
func TestPhase6bB1_ClosedTopicReopensOnNextSend(t *testing.T) {
	e := newB1DeliverEnv(t, -1001234567890)
	ctx := context.Background()
	_, first, _ := e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "một"})
	e.closeAndBlock("topic_not_forum")
	e.client.reopenErr = nil
	_, second, err := e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "hai"})
	if err != nil || second.ThreadID != first.ThreadID || e.client.reopens != 1 {
		t.Fatalf("second=%+v err=%v reopens=%d", second, err, e.client.reopens)
	}
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	for _, r := range e.store.rows {
		if r.State != TopicOpen {
			t.Fatalf("state after reopen=%q", r.State)
		}
	}
}

// TC-B1-40 (REQ-13): EvaluateRoute reports route, tier and usage, never text.
func TestPhase6bB1_EvaluateRoute(t *testing.T) {
	ctx := context.Background()
	run := func(step p6aJSONStep) (RouteEvalResult, *p6aModel) {
		m := &p6aModel{json: []p6aJSONStep{step}}
		return EvaluateRoute(ctx, m, p6aFast, nil, "chào em"), m
	}
	if r, m := run(p6aJSONStep{resp: p6aRouter(`{"route":"answer","reply":"SECRET-REPLY-TEXT","tier":"none"}`)}); r.Route != "answer" || r.Tier != "none" || r.RouterError != "" || r.Usage.Input != 10 || r.Usage.Output != 2 || len(m.jsonReqs) != 1 || m.jsonReqs[0].Model != p6aFast {
		t.Fatalf("answer: %+v model=%q", r, m.jsonReqs[0].Model)
	} else if strings.Contains(fmt.Sprintf("%+v", r), "SECRET-REPLY-TEXT") {
		t.Fatal("reply text in the result")
	}
	if r, _ := run(p6aJSONStep{resp: p6aRouter(`{"route":"escalate","reply":"","tier":"work"}`)}); r.Route != "work" || r.Tier != "work" {
		t.Fatalf("work: %+v", r)
	}
	if r, _ := run(p6aJSONStep{resp: p6aRouter(`{"route":"escalate","reply":"","tier":"deep"}`)}); r.Route != "deep" || r.Tier != "deep" {
		t.Fatalf("deep: %+v", r)
	}
	for name, step := range map[string]p6aJSONStep{
		"provider error": {err: errors.New("token sk-secret expired")},
		"bad json":       {resp: p6aRouter(`not json`)},
		"empty":          {resp: p6aRouter(`{}`)},
	} {
		r, _ := run(step)
		if r.Route != "work_fallback" || r.Tier != "work" || r.RouterError == "" || strings.Contains(r.RouterError, "sk-secret") || strings.Contains(r.RouterError, " ") {
			t.Fatalf("%s: %+v", name, r)
		}
	}
	// History reaches the router before the text.
	hist, ok := RouteEvalHistory("task")
	if !ok || len(hist) != 2 {
		t.Fatalf("task history ok=%v %+v", ok, hist)
	}
	m := &p6aModel{json: []p6aJSONStep{{resp: p6aRouter(`{"route":"answer","reply":"x","tier":"none"}`)}}}
	EvaluateRoute(ctx, m, p6aFast, hist, "cảm ơn")
	msgs := m.jsonReqs[0].Messages
	if len(msgs) != 3 || msgs[0].Role != domain.UserRole || !strings.Contains(msgs[0].Text, "86c1x2") || msgs[2].Text != "cảm ơn" {
		t.Fatalf("messages=%+v", msgs)
	}
	for _, kind := range []string{"", "none", "general", "task"} {
		if _, ok := RouteEvalHistory(kind); !ok {
			t.Fatalf("history %q rejected", kind)
		}
	}
	for _, kind := range []string{"other", "TASK", "../x"} {
		if _, ok := RouteEvalHistory(kind); ok {
			t.Fatalf("history %q accepted", kind)
		}
	}
	if h, _ := RouteEvalHistory(""); h != nil {
		t.Fatal("none history not empty")
	}
}

// TC-B1-41 (REQ-8): /topic is an engine command: a behavior cannot take it,
// and /help lists it.
func TestPhase6bB1_TopicCommandReservedAndHelped(t *testing.T) {
	if !reservedCommands["/topic"] {
		t.Fatal("/topic is not reserved")
	}
	e := newB1Env(b1States(0))
	if help := e.w.helpText(context.Background(), p4WS); !strings.Contains(help, "/topic <task id hoặc link ClickUp>") {
		t.Fatalf("help=%q", help)
	}
	u := e.update(0, 0)
	u.Kind, u.Command, u.Args = UpdateCommand, "/topic", "86abc"
	out := e.w.dispatch(context.Background(), u, "corr")
	if out.code != "not_available" {
		t.Fatalf("dispatch /topic without Links: %+v", out)
	}
	if _, err := json.Marshal(Button{Text: "x", URL: "https://t.me/c/1/2"}); err != nil {
		t.Fatal(err)
	}
}
