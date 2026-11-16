package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Phase 6b batch B: follow-up resolution inside a topic (REQ-7). Fakes only.

const (
	b1Bot    = "bot-a"
	b1Chat   = int64(-1001234567890)
	b1Thread = int64(55)
	b1Task   = "86abc"
	b1Other  = "86zzz"
	b1WSAlt  = "019a6690-abcd-7000-8000-0000000000aa"
)

// b1Topics is a TopicLookup fake. sloppy makes it return rows without
// checking the scope, so the worker's own guard is what is tested.
type b1Topics struct {
	mu     sync.Mutex
	rows   []Topic
	err    error
	sloppy bool
	calls  int
}

func (s *b1Topics) TopicByThread(_ context.Context, ws, integration string, chat, thread int64) (Topic, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return Topic{}, s.err
	}
	for _, r := range s.rows {
		if s.sloppy && r.ThreadID == thread {
			return r, nil
		}
		if r.WorkspaceID == ws && r.IntegrationID == integration && r.ChatID == chat && r.ThreadID == thread {
			return r, nil
		}
	}
	return Topic{}, ErrNotFound
}

func b1TaskTopic(task string, thread int64) Topic {
	return Topic{ID: thread, WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: b1Chat, ThreadID: thread, SubjectKind: TopicSubjectClickUpTask, SubjectKey: task, State: TopicOpen}
}

type b1Env struct {
	w      *InboxWorker
	r      *phase6BReader
	c      *phase6BConversations
	m      *p4Messenger
	ap     *p4Approvals
	inbox  *AssistantInbox
	topics *b1Topics
}

// newB1Env builds a worker with a task topic (thread 55, task 86abc). states
// maps execution ids to states (all in p4WS); bound maps the task id (and a
// reply message ref) to executions.
func newB1Env(states map[int64]workflow.State, bound map[string][]int64) *b1Env {
	r := &phase6BReader{details: map[int64]workflow.ExecutionDetail{}}
	for id, st := range states {
		r.details[id] = phase6BDetail(id, p4WS, st)
	}
	r.details[45] = phase6BDetail(45, phase6RTForeignWS, workflow.StateRunning)
	c := &phase6BConversations{resourcesByID: bound}
	if c.resourcesByID == nil {
		c.resourcesByID = map[string][]int64{}
	}
	m := &p4Messenger{}
	ap := newP4Approvals()
	inbox := NewAssistantInbox()
	w := phase6BWorker(r, c, m)
	w.d.Approvals = ap
	w.d.Assistant = &Assistant{Inbox: inbox, Workflows: r, Conversations: c,
		Models: func(context.Context, string) (domain.LLMProvider, error) { return nil, errors.New("unused") }}
	w.d.WorkflowCommands = struct{ workflow.Engine }{}
	topics := &b1Topics{rows: []Topic{b1TaskTopic(b1Task, b1Thread)}}
	w.d.Topics = topics
	return &b1Env{w: w, r: r, c: c, m: m, ap: ap, inbox: inbox, topics: topics}
}

func (e *b1Env) update(thread, reply int64) InboundUpdate {
	return InboundUpdate{WorkspaceID: p4WS, IntegrationID: b1Bot, UpdateID: 900, ChatID: b1Chat, ThreadID: thread, MessageID: 700,
		ReplyToMessageID: reply, Kind: UpdateText}
}

func (e *b1Env) to(thread int64) ChatRef {
	return ChatRef{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: b1Chat, ThreadID: thread}
}

// say sends staged free text written in thread (0 = General).
func (e *b1Env) say(t *testing.T, thread, reply int64, text string) outcome {
	t.Helper()
	key, err := e.inbox.StageAssistantText(p4WS, text, e.w.now())
	if err != nil {
		t.Fatal(err)
	}
	u := e.update(thread, reply)
	u.Args = AssistantTextPrefix + key
	return e.w.workflowOrAssistant(withConversation(context.Background(), 77, UpdateText), u, e.to(thread), "owner", "corr")
}

func (e *b1Env) cmd(thread, reply int64, command, args string) outcome {
	u := e.update(thread, reply)
	u.Kind, u.Command, u.Args = UpdateCommand, command, args
	return e.w.workflowControlCommand(withConversation(context.Background(), 77, UpdateCommand), u, e.to(thread), "owner", "corr")
}

func (e *b1Env) job() (assistantJob, bool) {
	select {
	case j := <-e.w.assistantJobs:
		return j, true
	default:
		return assistantJob{}, false
	}
}

func (e *b1Env) stored() int {
	n := 0
	for _, items := range e.c.instructions {
		n += len(items)
	}
	return n
}

// requireClarified asserts one clarification whose buttons name exactly want
// (in order), with nothing stored and no assistant job.
func (e *b1Env) requireClarified(t *testing.T, out outcome, want ...int64) {
	t.Helper()
	if out.code != "workflow_target" || e.stored() != 0 || len(e.ap.created) != 1 {
		t.Fatalf("want clarification: code=%q stored=%v approvals=%d texts=%q", out.code, e.c.instructions, len(e.ap.created), e.m.texts())
	}
	msg := e.m.last(t)
	if len(msg.Buttons) != len(want) {
		t.Fatalf("buttons=%+v want %v", msg.Buttons, want)
	}
	for i, id := range want {
		if !strings.HasPrefix(msg.Buttons[i][0].Text, workflow.FormatRef(id)+" ") {
			t.Fatalf("button %d=%q want %s", i, msg.Buttons[i][0].Text, workflow.FormatRef(id))
		}
	}
	var p workflowTargetPayload
	if err := json.Unmarshal(e.ap.created[0].Payload, &p); err != nil || len(p.Candidates) != len(want) {
		t.Fatalf("payload=%s err=%v", e.ap.created[0].Payload, err)
	}
	if _, queued := e.job(); queued {
		t.Fatal("clarification also queued an assistant job")
	}
}

func (e *b1Env) requireNothingRan(t *testing.T) {
	t.Helper()
	if e.stored() != 0 || len(e.ap.created) != 0 {
		t.Fatalf("something ran: instructions=%v approvals=%d", e.c.instructions, len(e.ap.created))
	}
}

// requireTopicJob asserts the text was queued for the assistant with the
// exact context line for the subject and no workflow target.
func (e *b1Env) requireTopicJob(t *testing.T, out outcome, line string) assistantJob {
	t.Helper()
	if out.code != "assistant_queued" {
		t.Fatalf("outcome=%+v texts=%q", out, e.m.texts())
	}
	j, ok := e.job()
	if !ok || j.workflowID != 0 || j.topicContext != line {
		t.Fatalf("job=%+v ok=%v want topicContext %q", j, ok, line)
	}
	e.requireNothingRan(t)
	return j
}

const b1TaskLine = "[Context] This message is in the topic of ClickUp task 86abc."

func b1States(running int, terminal ...int64) (map[int64]workflow.State, map[string][]int64) {
	states := map[int64]workflow.State{}
	var ids []int64
	for i := 1; i <= running; i++ {
		id := int64(100 + i)
		states[id] = workflow.StateRunning
		ids = append(ids, id)
	}
	for _, id := range terminal {
		states[id] = workflow.StateCompleted
		ids = append(ids, id)
	}
	// An execution of another task that must never be a candidate.
	states[300] = workflow.StateRunning
	return states, map[string][]int64{b1Task: ids, b1Other: {300}}
}

// TC-B1-01 (REQ-7): free text in a task topic goes to the subject's active
// execution: one -> instruction, several -> buttons with only those (newest
// first, at most 5), none -> assistant with the subject as context.
func TestPhase6bB1_TopicTextByActiveCount(t *testing.T) {
	t.Run("one active gets the instruction", func(t *testing.T) {
		e := newB1Env(b1States(1))
		out := e.say(t, b1Thread, 0, "thêm test cho case X")
		if out.code != "" || len(e.c.instructions[101]) != 1 || e.stored() != 1 {
			t.Fatalf("code=%q instructions=%v", out.code, e.c.instructions)
		}
		if got := e.c.instructions[101][0].Text; got != "thêm test cho case X" {
			t.Fatalf("stored=%q", got)
		}
		if _, queued := e.job(); queued {
			t.Fatal("instruction also queued an assistant job")
		}
	})
	t.Run("two active ask with only those", func(t *testing.T) {
		e := newB1Env(b1States(2))
		e.requireClarified(t, e.say(t, b1Thread, 0, "thêm test cho case X"), 102, 101)
	})
	t.Run("six active ask with the newest five", func(t *testing.T) {
		e := newB1Env(b1States(6))
		e.requireClarified(t, e.say(t, b1Thread, 0, "thêm test cho case X"), 106, 105, 104, 103, 102)
	})
	t.Run("none active asks the assistant with the subject", func(t *testing.T) {
		e := newB1Env(b1States(0))
		j := e.requireTopicJob(t, e.say(t, b1Thread, 0, "thêm test cho case X"), b1TaskLine)
		if j.u.ThreadID != b1Thread {
			t.Fatalf("job thread=%d", j.u.ThreadID)
		}
	})
	t.Run("terminal executions are ignored", func(t *testing.T) {
		e := newB1Env(b1States(0, 44, 45))
		e.requireTopicJob(t, e.say(t, b1Thread, 0, "thêm test"), b1TaskLine)
	})
	t.Run("terminal plus one active still targets the active one", func(t *testing.T) {
		e := newB1Env(b1States(1, 44))
		if out := e.say(t, b1Thread, 0, "tiếp tục đi"); out.code != "" || len(e.c.instructions[101]) != 1 {
			t.Fatalf("code=%q instructions=%v", out.code, e.c.instructions)
		}
	})
	t.Run("another task's execution is never a candidate", func(t *testing.T) {
		e := newB1Env(b1States(1))
		e.say(t, b1Thread, 0, "ghi chú")
		if len(e.c.instructions[300]) != 0 {
			t.Fatalf("task %s execution got %v", b1Other, e.c.instructions[300])
		}
	})
}

// TC-B1-02 (REQ-7): a question never becomes an instruction. One active
// execution is the context; none or several give the topic context.
func TestPhase6bB1_TopicQuestion(t *testing.T) {
	t.Run("one active is the workflow context", func(t *testing.T) {
		e := newB1Env(b1States(1))
		out := e.say(t, b1Thread, 0, "chạy tới đâu rồi?")
		j, ok := e.job()
		if out.code != "assistant_queued" || !ok || j.workflowID != 101 || j.topicContext != "" || e.stored() != 0 {
			t.Fatalf("out=%+v job=%+v ok=%v stored=%d", out, j, ok, e.stored())
		}
	})
	t.Run("none gives the subject context", func(t *testing.T) {
		e := newB1Env(b1States(0))
		e.requireTopicJob(t, e.say(t, b1Thread, 0, "task này sao rồi?"), b1TaskLine)
	})
	t.Run("two give the subject context and ask nothing", func(t *testing.T) {
		e := newB1Env(b1States(2))
		e.requireTopicJob(t, e.say(t, b1Thread, 0, "cái nào xong rồi?"), b1TaskLine)
	})
	t.Run("full-width question mark", func(t *testing.T) {
		e := newB1Env(b1States(2))
		e.requireTopicJob(t, e.say(t, b1Thread, 0, "cái nào xong rồi？"), b1TaskLine)
	})
}

// TC-B1-03 (REQ-7): /note /status /cancel /resume without an id use the topic
// subject in the same order.
func TestPhase6bB1_TopicCommands(t *testing.T) {
	t.Run("note one active", func(t *testing.T) {
		e := newB1Env(b1States(1))
		if out := e.cmd(b1Thread, 0, "/note", "thêm case Y"); out.code != "" || len(e.c.instructions[101]) != 1 {
			t.Fatalf("code=%q instructions=%v", out.code, e.c.instructions)
		}
	})
	t.Run("note two active asks", func(t *testing.T) {
		e := newB1Env(b1States(2))
		e.requireClarified(t, e.cmd(b1Thread, 0, "/note", "thêm case Y"), 102, 101)
	})
	t.Run("note none asks which, stores nothing", func(t *testing.T) {
		e := newB1Env(b1States(0))
		out := e.cmd(b1Thread, 0, "/note", "thêm case Y")
		if out.code != "workflow_target_not_found" {
			t.Fatalf("code=%q", out.code)
		}
		e.requireNothingRan(t)
	})
	t.Run("status one active answers for it", func(t *testing.T) {
		e := newB1Env(b1States(1))
		if out := e.cmd(b1Thread, 0, "/status", ""); out.code != "" {
			t.Fatalf("code=%q", out.code)
		}
		if got := e.m.last(t).Text; !strings.HasPrefix(got, "WF-101 · ") {
			t.Fatalf("status=%q", got)
		}
	})
	t.Run("status two active asks", func(t *testing.T) {
		e := newB1Env(b1States(2))
		e.requireClarified(t, e.cmd(b1Thread, 0, "/status", ""), 102, 101)
	})
	t.Run("cancel one active proposes a cancel of it", func(t *testing.T) {
		e := newB1Env(b1States(1))
		e.cmd(b1Thread, 0, "/cancel", "")
		if len(e.ap.created) != 1 || e.ap.created[0].Kind == ApprovalWorkflowTarget {
			t.Fatalf("approvals=%+v texts=%q", e.ap.created, e.m.texts())
		}
		if got := e.m.last(t).Text; !strings.Contains(got, "WF-101") || strings.Contains(got, "WF-102") {
			t.Fatalf("proposal=%q", got)
		}
	})
	t.Run("cancel two active asks and cancels nothing", func(t *testing.T) {
		e := newB1Env(b1States(2))
		e.requireClarified(t, e.cmd(b1Thread, 0, "/cancel", ""), 102, 101)
	})
	t.Run("cancel none finds nothing", func(t *testing.T) {
		e := newB1Env(b1States(0, 44))
		if out := e.cmd(b1Thread, 0, "/cancel", ""); out.code != "workflow_target_not_found" {
			t.Fatalf("code=%q", out.code)
		}
		e.requireNothingRan(t)
	})
	t.Run("resume two active asks", func(t *testing.T) {
		e := newB1Env(b1States(2))
		e.requireClarified(t, e.cmd(b1Thread, 0, "/resume", ""), 102, 101)
	})
	t.Run("explicit id in the command wins over the topic", func(t *testing.T) {
		e := newB1Env(b1States(2))
		if out := e.cmd(b1Thread, 0, "/status", "WF-102"); out.code != "" || !strings.HasPrefix(e.m.last(t).Text, "WF-102 · ") {
			t.Fatalf("code=%q text=%q", out.code, e.m.texts())
		}
	})
}

// TC-B1-04 (REQ-7, abuse): an explicit WF-n that is foreign or unknown never
// runs; the buttons list the topic subject's executions only.
func TestPhase6bB1_ExplicitForeignOrInvalidRef(t *testing.T) {
	for _, text := range []string{"WF-45 làm lại đi", "WF-999 làm lại đi"} {
		t.Run(text, func(t *testing.T) {
			e := newB1Env(b1States(1))
			out := e.say(t, b1Thread, 0, text)
			e.requireClarified(t, out, 101)
			if len(e.c.instructions[45]) != 0 {
				t.Fatal("foreign execution received an instruction")
			}
		})
	}
	t.Run("a foreign ref mentioned mid-sentence runs nothing", func(t *testing.T) {
		e := newB1Env(b1States(1))
		out := e.say(t, b1Thread, 0, "làm lại WF-45 giúp anh")
		if out.code != "assistant_queued" || e.stored() != 0 || len(e.ap.created) != 0 || len(e.c.instructions[45]) != 0 {
			t.Fatalf("out=%+v stored=%v", out, e.c.instructions)
		}
	})
	t.Run("explicit own ref inside the topic works", func(t *testing.T) {
		e := newB1Env(b1States(2))
		if out := e.say(t, b1Thread, 0, "WF-101 thêm test"); out.code != "" || len(e.c.instructions[101]) != 1 {
			t.Fatalf("code=%q instructions=%v", out.code, e.c.instructions)
		}
	})
	t.Run("two refs ask", func(t *testing.T) {
		e := newB1Env(b1States(2))
		e.requireClarified(t, e.say(t, b1Thread, 0, "WF-101 và WF-102 thêm test"), 102, 101)
	})
	t.Run("note command with a foreign ref", func(t *testing.T) {
		e := newB1Env(b1States(1))
		e.requireClarified(t, e.cmd(b1Thread, 0, "/note", "WF-45 thêm test"), 101)
	})
}

// TC-B1-05 (REQ-7): a reply to a bound message wins over the topic subject.
func TestPhase6bB1_ReplyTargetWinsInTopic(t *testing.T) {
	states, bound := b1States(2)
	states[500] = workflow.StateRunning
	bound[workflow.TelegramMessageRef(b1Bot, b1Chat, 9)] = []int64{500}
	e := newB1Env(states, bound)
	if out := e.say(t, b1Thread, 9, "thêm test"); out.code != "" || len(e.c.instructions[500]) != 1 || e.stored() != 1 {
		t.Fatalf("code=%q instructions=%v", out.code, e.c.instructions)
	}
	// A reply to an unbound message falls back to the subject.
	e2 := newB1Env(b1States(2))
	e2.requireClarified(t, e2.say(t, b1Thread, 77, "thêm test"), 102, 101)
}

// TC-B1-06 (REQ-7): outside a task topic the Phase 6 rules apply unchanged.
func TestPhase6bB1_NoSubjectKeepsPhase6(t *testing.T) {
	states, bound := b1States(2)
	cases := map[string]func(e *b1Env) int64{
		"General":        func(e *b1Env) int64 { return 0 },
		"unknown thread": func(e *b1Env) int64 { return 999 },
		"fixed topic": func(e *b1Env) int64 {
			e.topics.rows = []Topic{{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: b1Chat, ThreadID: 77, SubjectKind: TopicSubjectFixed, SubjectKey: "Reports"}}
			return 77
		},
		"unknown subject kind": func(e *b1Env) int64 {
			e.topics.rows = []Topic{{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: b1Chat, ThreadID: 77, SubjectKind: "other", SubjectKey: b1Task}}
			return 77
		},
		"bad task key": func(e *b1Env) int64 {
			e.topics.rows = []Topic{{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: b1Chat, ThreadID: 77, SubjectKind: TopicSubjectClickUpTask, SubjectKey: "../x y"}}
			return 77
		},
		"topics unavailable": func(e *b1Env) int64 { e.w.d.Topics = nil; return b1Thread },
		"lookup error":       func(e *b1Env) int64 { e.topics.err = errors.New("db down"); return b1Thread },
		"foreign workspace row": func(e *b1Env) int64 {
			e.topics.sloppy = true
			e.topics.rows = []Topic{{WorkspaceID: b1WSAlt, IntegrationID: b1Bot, ChatID: b1Chat, ThreadID: b1Thread, SubjectKind: TopicSubjectClickUpTask, SubjectKey: b1Task}}
			return b1Thread
		},
		"foreign integration row": func(e *b1Env) int64 {
			e.topics.sloppy = true
			e.topics.rows = []Topic{{WorkspaceID: p4WS, IntegrationID: "bot-b", ChatID: b1Chat, ThreadID: b1Thread, SubjectKind: TopicSubjectClickUpTask, SubjectKey: b1Task}}
			return b1Thread
		},
		"foreign chat row": func(e *b1Env) int64 {
			e.topics.sloppy = true
			e.topics.rows = []Topic{{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: -1009999999999, ThreadID: b1Thread, SubjectKind: TopicSubjectClickUpTask, SubjectKey: b1Task}}
			return b1Thread
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e := newB1Env(states, bound)
			thread := setup(e)
			// Phase 6: no bound workflow in this conversation, so the text goes to
			// the plain assistant (no topic context) and nothing is stored.
			out := e.say(t, thread, 0, "thêm test cho case X")
			j, ok := e.job()
			if out.code != "assistant_queued" || !ok || j.topicContext != "" || j.workflowID != 0 {
				t.Fatalf("out=%+v job=%+v ok=%v", out, j, ok)
			}
			e.requireNothingRan(t)
		})
	}
	t.Run("Phase 6 conversation binding still clarifies in General", func(t *testing.T) {
		e := newB1Env(states, bound)
		e.c.bound = []int64{101, 102}
		e.say(t, 0, 0, "thêm test") // plain text: assistant
		e.job()
		out := e.cmd(0, 0, "/note", "thêm test")
		e.requireClarified(t, out, 102, 101)
	})
	t.Run("a question in a fixed topic keeps Phase 6", func(t *testing.T) {
		e := newB1Env(states, bound)
		e.topics.rows = []Topic{{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: b1Chat, ThreadID: 77, SubjectKind: TopicSubjectFixed, SubjectKey: "Reports"}}
		e.requireTopicJobNone(t, e.say(t, 77, 0, "sao rồi?"))
	})
}

func (e *b1Env) requireTopicJobNone(t *testing.T, out outcome) {
	t.Helper()
	j, ok := e.job()
	if out.code != "assistant_queued" || !ok || j.topicContext != "" || j.workflowID != 0 {
		t.Fatalf("out=%+v job=%+v ok=%v", out, j, ok)
	}
}

// TC-B1-07 (REQ-7): a topic whose subject is a workflow.
func TestPhase6bB1_WorkflowSubjectTopic(t *testing.T) {
	wfTopic := func(key string) Topic {
		return Topic{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: b1Chat, ThreadID: 61, SubjectKind: TopicSubjectWorkflow, SubjectKey: key}
	}
	t.Run("active workflow gets the instruction", func(t *testing.T) {
		e := newB1Env(map[int64]workflow.State{101: workflow.StateRunning}, nil)
		e.topics.rows = []Topic{wfTopic("101")}
		if out := e.say(t, 61, 0, "thêm test"); out.code != "" || len(e.c.instructions[101]) != 1 {
			t.Fatalf("code=%q instructions=%v", out.code, e.c.instructions)
		}
	})
	t.Run("terminal workflow asks the assistant with its ref", func(t *testing.T) {
		e := newB1Env(map[int64]workflow.State{44: workflow.StateCompleted}, nil)
		e.topics.rows = []Topic{wfTopic("44")}
		e.requireTopicJob(t, e.say(t, 61, 0, "thêm test"), "[Context] This message is in the topic of WF-44.")
	})
	t.Run("missing and foreign workflow behave like terminal", func(t *testing.T) {
		for _, key := range []string{"999", "45"} {
			e := newB1Env(nil, nil)
			e.topics.rows = []Topic{wfTopic(key)}
			e.requireTopicJob(t, e.say(t, 61, 0, "thêm test"), "[Context] This message is in the topic of WF-"+key+".")
		}
	})
	t.Run("non numeric or non positive key is no subject", func(t *testing.T) {
		for _, key := range []string{"abc", "0", "-3", ""} {
			e := newB1Env(map[int64]workflow.State{101: workflow.StateRunning}, nil)
			e.topics.rows = []Topic{wfTopic(key)}
			e.requireTopicJobNone(t, e.say(t, 61, 0, "thêm test"))
		}
	})
	t.Run("read error is uncertain, never a blind instruction", func(t *testing.T) {
		e := newB1Env(map[int64]workflow.State{101: workflow.StateRunning}, nil)
		e.r.detailErr = map[int64]error{101: errors.New("db down")}
		e.topics.rows = []Topic{wfTopic("101")}
		out := e.say(t, 61, 0, "thêm test")
		e.requireNothingRan(t)
		if out.code != "assistant_queued" {
			t.Fatalf("out=%+v", out)
		}
	})
}

// TC-B1-08 (REQ-7, abuse): read errors make the target uncertain, so the
// worker asks instead of writing to a possibly wrong execution.
func TestPhase6bB1_ReadErrorsAsk(t *testing.T) {
	t.Run("one active and one unreadable asks", func(t *testing.T) {
		states, bound := b1States(2)
		e := newB1Env(states, bound)
		e.r.detailErr = map[int64]error{101: errors.New("db down")}
		out := e.say(t, b1Thread, 0, "thêm test")
		e.requireClarified(t, out, 102)
	})
	t.Run("binding lookup failure sends text to the assistant, writes nothing", func(t *testing.T) {
		e := newB1Env(b1States(1))
		e.w.d.Conversations = &b1FailingBindings{phase6BConversations: e.c}
		out := e.say(t, b1Thread, 0, "thêm test")
		e.requireNothingRan(t)
		j, ok := e.job()
		if out.code != "assistant_queued" || !ok || j.workflowID != 0 {
			t.Fatalf("out=%+v job=%+v", out, j)
		}
	})
	t.Run("more than 50 bindings is uncertain", func(t *testing.T) {
		states, bound := b1States(0)
		var ids []int64
		for i := int64(1); i <= 60; i++ {
			ids = append(ids, 1000+i)
			states[1000+i] = workflow.StateRunning
		}
		bound[b1Task] = ids
		e := newB1Env(states, bound)
		out := e.say(t, b1Thread, 0, "thêm test")
		if out.code != "workflow_target" || e.stored() != 0 {
			t.Fatalf("code=%q stored=%d", out.code, e.stored())
		}
		if got := len(e.m.last(t).Buttons); got != 5 {
			t.Fatalf("buttons=%d", got)
		}
	})
}

type b1FailingBindings struct{ *phase6BConversations }

func (*b1FailingBindings) ExecutionsByResource(context.Context, string, string, string) ([]int64, error) {
	return nil, errors.New("db down")
}

// TC-B1-09 (REQ-7): /list in a task topic shows the task's executions and a
// hint; in General it shows the workspace list unchanged.
func TestPhase6bB1_ListInTopicAndGeneral(t *testing.T) {
	e := newB1Env(b1States(1, 44))
	e.r.list = []workflow.ExecutionView{
		{Execution: workflow.Execution{ID: 300, WorkspaceID: p4WS, State: workflow.StateRunning}, BehaviorName: "other"},
		{Execution: workflow.Execution{ID: 101, WorkspaceID: p4WS, State: workflow.StateRunning}, BehaviorName: "review"},
	}
	out := e.cmd(b1Thread, 0, "/list", "")
	text := e.m.last(t).Text
	if out.code != "" || !strings.Contains(text, "WF-101 · review") || !strings.Contains(text, "WF-44 · review") ||
		strings.Contains(text, "WF-300") || !strings.Contains(text, "gõ /list ở General") {
		t.Fatalf("topic list=%q", text)
	}
	e.cmd(0, 0, "/list", "")
	general := e.m.last(t).Text
	if !strings.Contains(general, "WF-300") || !strings.Contains(general, "WF-101") || strings.Contains(general, "gõ /list ở General") {
		t.Fatalf("General list=%q", general)
	}
	// A topic without executions, and a workflow-subject topic, say so or fall
	// back to the workspace list.
	empty := newB1Env(b1States(0))
	empty.cmd(b1Thread, 0, "/list", "")
	if got := empty.m.last(t).Text; !strings.HasPrefix(got, "Task này chưa có workflow.") {
		t.Fatalf("empty topic list=%q", got)
	}
	// "/list extra" is a usage error even inside a topic.
	if out := e.cmd(b1Thread, 0, "/list", "x"); out.code != "usage" {
		t.Fatalf("code=%q", out.code)
	}
	// All reads failing inside a topic is a failure, not an empty list.
	bad := newB1Env(b1States(1))
	bad.w.d.Conversations = &b1FailingBindings{phase6BConversations: bad.c}
	if out := bad.cmd(b1Thread, 0, "/list", ""); out.code != "handler_failed" {
		t.Fatalf("code=%q", out.code)
	}
}

// TC-B1-10 (REQ-7): the context line is exact, goes to the model only, is not
// stored in the history, skips the router (route skip) and the history of a
// topic is separate from General and from other topics.
func TestPhase6bB1_ContextLineAndThreadHistory(t *testing.T) {
	m := &p6aModel{}
	e := p6aEnv(m)
	ctx := context.Background()
	job := func(thread int64, text, line string) assistantJob {
		j := p6aJob(text, 0, "")
		j.u.ThreadID, j.to.ThreadID, j.topicContext = thread, thread, line
		return j
	}
	res, err := e.w.converse(ctx, job(b1Thread, "thêm test cho case X", b1TaskLine))
	if err != nil || res.route != routeSkip || res.model != p6aWork || len(m.jsonReqs) != 0 {
		t.Fatalf("topic turn: %+v err=%v router calls=%d", res, err, len(m.jsonReqs))
	}
	owner := m.toolReqs[0].Turns[len(m.toolReqs[0].Turns)-1].Text
	if owner != b1TaskLine+"\nthêm test cho case X" {
		t.Fatalf("owner turn=%q", owner)
	}
	if m.toolReqs[0].System != assistantSystem {
		t.Fatal("system prompt changed for a topic turn")
	}
	hist, _ := e.assistant.threadHistoryFor(p4WS, "bot-a", 77, b1Thread)
	got := hist.messagesForTurns(20)
	if len(got) == 0 {
		t.Fatal("topic history empty")
	}
	for _, msg := range got {
		if strings.Contains(msg.Text, "[Context]") || strings.Contains(msg.Text, "86abc") {
			t.Fatalf("stored history carries the context line: %q", msg.Text)
		}
	}
	// General has its own, empty history; a plain General turn goes through
	// the router again and sees no topic message.
	if _, err := e.w.converse(ctx, job(0, "chào", "")); err != nil {
		t.Fatal(err)
	}
	if len(m.jsonReqs) != 1 {
		t.Fatalf("router calls=%d, want 1 (General turn)", len(m.jsonReqs))
	}
	for _, msg := range m.jsonReqs[0].Messages {
		if strings.Contains(msg.Text, "thêm test cho case X") {
			t.Fatalf("General router saw the topic's message: %q", msg.Text)
		}
	}
	// Another topic does not see the first topic's turns either.
	if _, err := e.w.converse(ctx, job(b1Thread+1, "xin chào", "[Context] This message is in the topic of ClickUp task 86zzz.")); err != nil {
		t.Fatal(err)
	}
	last := m.toolReqs[len(m.toolReqs)-1]
	for _, turn := range last.Turns {
		if strings.Contains(turn.Text, "thêm test cho case X") {
			t.Fatalf("topic 2 saw topic 1's turn: %q", turn.Text)
		}
	}
	// A workflow target wins over the topic context in the prefix.
	m2 := &p6aModel{}
	e2 := p6aEnv(m2)
	wf := job(b1Thread, "còn bao lâu?", b1TaskLine)
	wf.workflowID = 101
	if _, err := e2.w.converse(ctx, wf); err != nil {
		t.Fatal(err)
	}
	if o := m2.toolReqs[0].Turns[len(m2.toolReqs[0].Turns)-1].Text; strings.Contains(o, "ClickUp task") || !strings.HasPrefix(o, "[Context] This message replies to WF-101.") {
		t.Fatalf("workflow owner turn=%q", o)
	}
	// Negative thread ids are the main thread.
	if h1, _ := e.assistant.threadHistoryFor(p4WS, "bot-a", 77, -5); h1 == nil {
		t.Fatal("no history")
	} else if h0, _ := e.assistant.threadHistoryFor(p4WS, "bot-a", 77, 0); h0 != h1 {
		t.Fatal("negative thread id is not the main thread history")
	}
}

// TC-B1-11 (REQ-7): the context line names only the subject id.
func TestPhase6bB1_ContextLineText(t *testing.T) {
	cases := []struct {
		t    topicTarget
		want string
	}{
		{topicTarget{kind: TopicSubjectClickUpTask, key: "86abc"}, "[Context] This message is in the topic of ClickUp task 86abc."},
		{topicTarget{kind: TopicSubjectWorkflow, key: "12"}, "[Context] This message is in the topic of WF-12."},
		{topicTarget{kind: TopicSubjectWorkflow, key: "x"}, ""},
		{topicTarget{kind: TopicSubjectWorkflow, key: "0"}, ""},
		{topicTarget{}, ""},
	}
	for _, c := range cases {
		if got := c.t.contextLine(); got != c.want {
			t.Fatalf("%+v: %q want %q", c.t, got, c.want)
		}
	}
	var one topicTarget
	if _, ok := one.single(); ok {
		t.Fatal("empty target has a single execution")
	}
}
