package behavior

import (
	"context"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Phase 6b batch B fix round: regressions for review findings R1-R5.

// sayInChat is e.say for a lone General message in an arbitrary chat id.
func (e *b1LinkEnv) sayInChat(t *testing.T, chat int64, text string) outcome {
	t.Helper()
	key, err := e.inbox.StageAssistantText(p4WS, text, e.w.now())
	if err != nil {
		t.Fatal(err)
	}
	u := e.update(0, 0)
	u.ChatID = chat
	u.Args = AssistantTextPrefix + key
	to := ChatRef{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: chat}
	return e.w.workflowOrAssistant(withConversation(context.Background(), 77, UpdateText), u, to, "owner", "corr")
}

// TC-B1-R5 (REQ-8, review R5): a lone task link jumps only in a supergroup
// not remembered as non-forum; otherwise it is a normal assistant message.
func TestPhase6bB1_LinkJumpOnlyInForumSupergroup(t *testing.T) {
	const link = "https://app.clickup.com/t/86abc"
	for _, tc := range []struct {
		name     string
		chat     int64
		nonForum bool
		jump     bool
	}{
		{"forum supergroup jumps", -1001234567890, false, true},
		{"private chat (positive id)", 424242, false, false},
		{"small negative group id", -4242, false, false},
		{"basic group boundary id", -1000000000000, false, false},
		{"supergroup remembered as non-forum", -1001234567890, true, false},
		{"another supergroup is unaffected by the mark", -1009999999999, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newB1LinkEnv(t)
			if tc.nonForum {
				e.router.block(ChatRef{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: tc.chat}, topicCodeNotForum)
			}
			if tc.name == "another supergroup is unaffected by the mark" {
				e.router.block(ChatRef{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: -1001234567890}, topicCodeNotForum)
			}
			out := e.sayInChat(t, tc.chat, link)
			_, queued := e.job()
			if tc.jump {
				if out.code != "" || queued || e.client.createCount() != 1 || len(e.lastButtons(t)) != 1 {
					t.Fatalf("no jump: out=%+v queued=%v creates=%d", out, queued, e.client.createCount())
				}
				return
			}
			if !queued || e.client.createCount() != 0 || len(e.topicRows(p4WS)) != 0 {
				t.Fatalf("assistant path expected: out=%+v queued=%v creates=%d rows=%d", out, queued, e.client.createCount(), len(e.topicRows(p4WS)))
			}
		})
	}
	t.Run("non-forum memory expires", func(t *testing.T) {
		e := newB1LinkEnv(t)
		now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
		e.router.now = func() time.Time { return now }
		c := ChatRef{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: -1001234567890}
		e.router.block(c, topicCodeNotForum)
		if !e.router.ChatNonForum(c) {
			t.Fatal("not remembered")
		}
		now = now.Add(NonForumMemory + time.Second)
		if e.router.ChatNonForum(c) {
			t.Fatal("memory did not expire")
		}
		var nilRouter *TopicRouter
		if nilRouter.ChatNonForum(c) {
			t.Fatal("nil router reports non-forum")
		}
	})
}

// TC-B1-R1a (REQ-9, review R1): the router tells the lifecycle when it creates
// or reopens a task topic (task kind only, never on a failed reopen).
func TestPhase6bB1_RouterReportsTaskTopicOpen(t *testing.T) {
	ctx := context.Background()
	e := newB1DeliverEnv(t, -1001234567890)
	type call struct{ ws, task string }
	var got []call
	e.router.OnTaskTopicOpen(func(ws, task string) { got = append(got, call{ws, task}) })
	e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "một"})
	if len(got) != 1 || got[0] != (call{p6bWS, "86abc"}) {
		t.Fatalf("after create: %+v", got)
	}
	e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "hai"})
	if len(got) != 1 {
		t.Fatalf("an open topic reported again: %+v", got)
	}
	e.closeAndBlock("topic_no_rights")
	e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "ba"})
	if len(got) != 1 {
		t.Fatalf("failed reopen reported: %+v", got)
	}
	e.client.reopenErr = nil
	e.router.unblock(e.chat)
	e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "bốn"})
	if len(got) != 2 || got[1] != (call{p6bWS, "86abc"}) {
		t.Fatalf("after reopen: %+v", got)
	}
	// Workflow topics are not task topics.
	e.eng.deliver(ctx, e.route(ReportTopicWorkflow, ""), OutMessage{Text: "wf"})
	if len(got) != 2 {
		t.Fatalf("non-task topic reported: %+v", got)
	}
	// A router without a callback keeps working.
	e2 := newB1DeliverEnv(t, -1001234567890)
	if _, to, err := e2.eng.deliver(ctx, e2.route(ReportTopicTask, "86abc"), OutMessage{Text: "x"}); err != nil || to.ThreadID == 0 {
		t.Fatalf("to=%+v err=%v", to, err)
	}
	var nilRouter *TopicRouter
	nilRouter.OnTaskTopicOpen(func(string, string) {})
}

// TC-B1-R1b (REQ-9, review R1): after TopicReopened the sweep keeps the topic
// open (no read, no close) until a new done event, or a read at least 30
// minutes later that says done.
func TestPhase6bB1_ReopenedTopicStaysOpenUntilNewDone(t *testing.T) {
	ctx := context.Background()
	t.Run("sweeps keep it open, new done event closes again", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		e.status, e.stype = "Complete", "done"
		e.event(lcWS, "T1", "Complete", "done")
		if e.store.state(1) != TopicClosed {
			t.Fatal("not closed by the done event")
		}
		// A send reopens the topic.
		e.store.SetTopicState(ctx, lcWS, 1, TopicOpen)
		e.l.TopicReopened(lcWS, "T1")
		for i := 0; i < 3; i++ {
			e.clock = e.clock.Add(5 * time.Minute)
			e.l.Sweep(ctx)
		}
		if e.store.state(1) != TopicOpen || len(e.closer.calls()) != 1 || len(e.reads) != 0 {
			t.Fatalf("sweep re-closed or read: state=%q calls=%v reads=%v", e.store.state(1), e.closer.calls(), e.reads)
		}
		e.event(lcWS, "T1", "Complete", "done")
		if e.store.state(1) != TopicClosed || len(e.closer.calls()) != 2 {
			t.Fatalf("new done event did not close: state=%q calls=%v", e.store.state(1), e.closer.calls())
		}
	})
	t.Run("done remembered while active, then reopened: not closed when it ends", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		e.exec.byTask[lcWS+"/T1"] = []int64{8}
		e.exec.states[8] = workflow.StateRunning
		e.event(lcWS, "T1", "Complete", "done")
		e.l.TopicReopened(lcWS, "T1")
		e.exec.set(8, workflow.StateCompleted)
		e.l.Sweep(ctx)
		if e.store.state(1) != TopicOpen || len(e.closer.calls()) != 0 {
			t.Fatalf("closed: %v", e.closer.calls())
		}
	})
	t.Run("a later read that says done closes it", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		e.status, e.stype = "Complete", "done"
		e.l.TopicReopened(lcWS, "T1")
		e.l.Sweep(ctx)
		if e.store.state(1) != TopicOpen || len(e.reads) != 0 {
			t.Fatalf("early read/close: reads=%v", e.reads)
		}
		e.clock = e.clock.Add(31 * time.Minute)
		e.l.Sweep(ctx)
		if e.store.state(1) != TopicClosed || len(e.reads) != 1 {
			t.Fatalf("state=%q reads=%v", e.store.state(1), e.reads)
		}
	})
	t.Run("a later read that says open keeps it open", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		e.status, e.stype = "In Progress", "custom"
		e.l.TopicReopened(lcWS, "T1")
		e.clock = e.clock.Add(31 * time.Minute)
		e.l.Sweep(ctx)
		if e.store.state(1) != TopicOpen || len(e.closer.calls()) != 0 {
			t.Fatal("closed an open task")
		}
	})
	t.Run("ignores bad input and other workspaces", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		e.status, e.stype = "Complete", "done"
		e.event(lcWS, "T1", "Complete", "done")
		e.store.SetTopicState(ctx, lcWS, 1, TopicOpen)
		e.l.TopicReopened("", "T1")
		e.l.TopicReopened(lcWS, "not a task id!")
		e.l.TopicReopened(lcOther, "T1")
		var nilL *TopicLifecycle
		nilL.TopicReopened(lcWS, "T1")
		e.l.Sweep(ctx)
		if e.store.state(1) != TopicClosed {
			t.Fatal("unrelated reopen calls cleared the done state")
		}
	})
	t.Run("router and lifecycle wired together", func(t *testing.T) {
		e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
		e.event(lcWS, "T1", "Complete", "done")
		d := newB1DeliverEnv(t, -1001234567890)
		d.chat.WorkspaceID = lcWS
		d.router.OnTaskTopicOpen(e.l.TopicReopened)
		// The router's own topic for T1 is created, which counts as an open.
		d.eng.deliver(ctx, d.route(ReportTopicTask, "T1"), OutMessage{Text: "x"})
		e.store.SetTopicState(ctx, lcWS, 1, TopicOpen)
		e.status, e.stype = "Complete", "done"
		e.l.Sweep(ctx)
		if e.store.state(1) != TopicOpen {
			t.Fatal("sweep re-closed after the router opened the task topic")
		}
	})
}
