package behavior

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// Phase 7b carry-ins (REQ-16 and C1): custom task ids in any letter case, a
// command behavior addressed with a custom id, and Recover reopening a topic
// without the 30 s sweep closing it again. Fakes only.

// TC-C1-1 (REQ-16): custom ids are accepted in any letter case and normalised to upper case.
func TestPhase7bC1_CustomIDLetterCase(t *testing.T) {
	want := map[string]TaskRef{
		"fc-6583":                           {ID: "FC-6583"},
		"Fc-6583":                           {ID: "FC-6583"},
		"FC-6583":                           {ID: "FC-6583"},
		"  fc-6583":                         {ID: "FC-6583"},
		"abc12-7":                           {ID: "ABC12-7"},
		"a-1":                               {ID: "A-1"},
		"https://app.clickup.com/t/fc-6583": {ID: "FC-6583"},
		"https://app.clickup.com/t/9008218621/fc-6583":           {ID: "FC-6583", TeamID: "9008218621"},
		"https://app.clickup.com/t/9008218621/Fc-6583?comment=1": {ID: "FC-6583", TeamID: "9008218621"},
		"https://APP.clickup.com/t/12/fc-1/":                     {ID: "FC-1", TeamID: "12"},
		"86abc":                                                  {ID: "86abc"},
		"86ABC":                                                  {ID: "86ABC"},
		"abcdef":                                                 {ID: "abcdef"},
		"86a-1":                                                  {ID: "86a-1"},
		"https://app.clickup.com/t/9/86abc":                      {ID: "86abc", TeamID: "9"},
		"https://app.clickup.com/t/86abc":                        {ID: "86abc"},
	}
	for in, w := range want {
		got, ok := ParseTaskRefTeam(in)
		if !ok || got != w {
			t.Errorf("ParseTaskRefTeam(%q)=(%+v,%v) want %+v", in, got, ok, w)
		}
		if id, ok := ParseTaskRef(in); !ok || id != w.ID {
			t.Errorf("ParseTaskRef(%q)=(%q,%v) want %q", in, id, ok, w.ID)
		}
	}
	for id, custom := range map[string]bool{
		"fc-6583": true, "FC-6583": true, "Fc-6583": true, "a-1": true,
		"86abc": false, "86a-1": false, "abcdef": false, "fc-": false, "fc-12a": false, "fc-1-2": false, "-1": false, "": false, "fc 1": false,
	} {
		if got := (TaskRef{ID: id}).Custom(); got != custom {
			t.Errorf("Custom(%q)=%v want %v", id, got, custom)
		}
	}
	// Lowercase does not open a way around the other checks.
	for _, in := range []string{"fc-1/../x", "fc-1?x=y", "fc 1", "fc-1\nx", "https://evil.example/t/1/fc-1", "https://app.clickup.com/t/abc/fc-1", ""} {
		if got, ok := ParseTaskRefTeam(in); ok {
			t.Errorf("ParseTaskRefTeam(%q)=(%+v,true), want false", in, got)
		}
	}
}

// TC-C1-2 (REQ-16, human): /topic fc-6583 works like /topic FC-6583 and reuses one topic.
func TestPhase7bC1_LowercaseCustomIDTopic(t *testing.T) {
	e := newB1LinkEnv(t)
	res := &h1Resolver{id: "86canon"}
	e.router.d.ResolveTask = res.fn
	if out := e.topicCmd("fc-6583"); out.code != "" {
		t.Fatalf("/topic fc-6583 code=%q texts=%q", out.code, e.m.texts())
	}
	first := e.lastButtons(t)
	if out := e.topicCmd("FC-6583"); out.code != "" || e.lastButtons(t)[0].URL != first[0].URL {
		t.Fatalf("uppercase opened another topic: %+v", out)
	}
	if res.calls[0] != (TaskRef{ID: "FC-6583"}) || res.calls[1] != (TaskRef{ID: "FC-6583"}) {
		t.Fatalf("resolver must get the normalised id: %+v", res.calls)
	}
	if rows := e.topicRows(p4WS); len(rows) != 1 || rows[0].SubjectKey != "86canon" {
		t.Fatalf("rows=%+v", rows)
	}
	if e.client.createCount() != 1 {
		t.Fatalf("creates=%d", e.client.createCount())
	}
}

// --- command behavior with a task argument ---

type c1Defs struct {
	DefinitionStore
	versions []Version
}

func (d *c1Defs) EnabledVersions(context.Context, string) ([]Version, error) { return d.versions, nil }

type c1Runs struct {
	RunStore
	mu   sync.Mutex
	runs []NewRun
}

func (r *c1Runs) CreateRun(_ context.Context, ws string, n NewRun) (Run, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, n)
	return Run{ID: int64(len(r.runs)), WorkspaceID: ws, CorrelationID: n.CorrelationID}, true, nil
}

func c1Runner(t *testing.T, args []string) (*CommandRunner, *c1Runs) {
	t.Helper()
	doc := Doc{Schema: 1, Name: "fc-cmd", Triggers: []TriggerDoc{{Type: TriggerTelegramCommand, Command: "/fc", Args: args}},
		Instructions: "x", Actions: map[string][]ActionDoc{string(ClassNotify): {{Type: ActionTelegramMessage}}}}
	raw, err := Encode(doc)
	if err != nil {
		t.Fatal(err)
	}
	vars, err := EncodeVars(nil)
	if err != nil {
		t.Fatal(err)
	}
	runs := &c1Runs{}
	cr, err := NewCommandRunner(&c1Defs{versions: []Version{{ID: 1, BehaviorID: 1, Name: "fc-cmd", Version: 1, Document: raw, Vars: vars}}}, runs)
	if err != nil {
		t.Fatal(err)
	}
	return cr, runs
}

func c1Req(args string, update int64) CommandRequest {
	return CommandRequest{WorkspaceID: p4WS, IntegrationID: "bot", ChatID: -100, UpdateID: update, Command: "/fc", Args: args, Actor: "owner"}
}

// TC-C1-3 (REQ-16): a command with a task argument resolves a custom id through the resolver.
func TestPhase7bC1_CommandResolvesCustomID(t *testing.T) {
	ctx := context.Background()
	for _, in := range []string{"FC-6583", "fc-6583", "Fc-6583", "https://app.clickup.com/t/9008218621/fc-6583?comment=1"} {
		t.Run(in, func(t *testing.T) {
			cr, runs := c1Runner(t, []string{CommandArgTask})
			res := &h1Resolver{id: "86canon"}
			cr.SetTaskResolver(taskResolverFunc(res.fn))
			reply, err := cr.TriggerCommand(ctx, c1Req(in, 1))
			if err != nil {
				t.Fatal(err)
			}
			if len(runs.runs) != 1 || runs.runs[0].Subject != TaskSubject("86canon") {
				t.Fatalf("runs=%+v", runs.runs)
			}
			if !strings.Contains(reply, "86canon") {
				t.Fatalf("reply %q", reply)
			}
			wantTeam := ""
			if strings.HasPrefix(in, "https://") {
				wantTeam = "9008218621"
			}
			if res.count() != 1 || res.calls[0] != (TaskRef{ID: "FC-6583", TeamID: wantTeam}) || res.ws[0] != p4WS {
				t.Fatalf("resolver calls=%+v ws=%v", res.calls, res.ws)
			}
		})
	}
	// A canonical id never reaches the resolver; no resolver means the id is used as normalised.
	cr, runs := c1Runner(t, []string{CommandArgTask})
	res := &h1Resolver{id: "should-not-be-used"}
	cr.SetTaskResolver(taskResolverFunc(res.fn))
	if _, err := cr.TriggerCommand(ctx, c1Req("86abc", 2)); err != nil || res.count() != 0 || runs.runs[0].Subject != TaskSubject("86abc") {
		t.Fatalf("canonical: err=%v calls=%d runs=%+v", err, res.count(), runs.runs)
	}
	cr2, runs2 := c1Runner(t, []string{CommandArgTask})
	if _, err := cr2.TriggerCommand(ctx, c1Req("fc-6583", 3)); err != nil || runs2.runs[0].Subject != TaskSubject("FC-6583") {
		t.Fatalf("no resolver: err=%v runs=%+v", err, runs2.runs)
	}
	var nilRunner *CommandRunner
	nilRunner.SetTaskResolver(taskResolverFunc(res.fn)) // must not panic
}

type taskResolverFunc func(ctx context.Context, ws string, ref TaskRef) (string, error)

func (f taskResolverFunc) ResolveTask(ctx context.Context, ws string, ref TaskRef) (string, error) {
	return f(ctx, ws, ref)
}

// TC-C1-4 (REQ-16, abuse): a resolver failure is a usage error without the raw error and starts no run.
func TestPhase7bC1_CommandResolverFailureIsUsageError(t *testing.T) {
	ctx := context.Background()
	cr, runs := c1Runner(t, []string{CommandArgTask})
	raw := errors.New("clickup 404 https://secret.example/x?token=abc123")
	cr.SetTaskResolver(taskResolverFunc((&h1Resolver{err: raw}).fn))
	buf := captureLog(t)
	reply, err := cr.TriggerCommand(ctx, c1Req("fc-6583", 1))
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("want *UsageError, got %v", err)
	}
	if reply != "" || len(runs.runs) != 0 {
		t.Fatalf("reply=%q runs=%+v", reply, runs.runs)
	}
	if !strings.Contains(ue.Usage, "FC-6583") {
		t.Fatalf("usage must name the id the owner typed: %q", ue.Usage)
	}
	for _, leak := range []string{"secret", "token", "abc123", "404", "clickup"} {
		if strings.Contains(ue.Usage, leak) || strings.Contains(err.Error(), leak) || strings.Contains(buf.String(), leak) {
			t.Errorf("raw resolver error leaked %q: usage=%q log=%q", leak, ue.Usage, buf.String())
		}
	}
	// Argument shape problems are usage errors and never call the resolver.
	res := &h1Resolver{id: "86canon"}
	cr.SetTaskResolver(taskResolverFunc(res.fn))
	for _, args := range []string{"", "FC-1 FC-2", "FC-1/../x", "FC-1?x=y", "https://evil.example/t/1/FC-1", "fc 1"} {
		_, err := cr.TriggerCommand(ctx, c1Req(args, 9))
		if !errors.As(err, &ue) {
			t.Errorf("args %q: want usage error, got %v", args, err)
		}
	}
	if res.count() != 0 || len(runs.runs) != 0 {
		t.Fatalf("resolver called %d times, runs=%d", res.count(), len(runs.runs))
	}
}

// TC-C1-5 (REQ-16): a command that declares no arguments ignores the resolver; a bad update id is invalid.
func TestPhase7bC1_CommandWithoutArgsAndBadUpdate(t *testing.T) {
	ctx := context.Background()
	cr, runs := c1Runner(t, nil)
	res := &h1Resolver{id: "86canon"}
	cr.SetTaskResolver(taskResolverFunc(res.fn))
	if _, err := cr.TriggerCommand(ctx, c1Req("", 1)); err != nil || res.count() != 0 || len(runs.runs) != 1 {
		t.Fatalf("err=%v calls=%d runs=%d", err, res.count(), len(runs.runs))
	}
	if _, err := cr.TriggerCommand(ctx, c1Req("", 0)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("update 0: %v", err)
	}
	if _, err := cr.TriggerCommand(ctx, CommandRequest{WorkspaceID: p4WS, UpdateID: 5, Command: "/other"}); !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("unknown command: %v", err)
	}
}

// --- Recover reopen and the lifecycle sweep ---

type c1Call struct{ ws, task string }

// c1Recover builds a router with a task topic for 86abc, closes it in the
// store and returns the chat of its thread.
func c1Setup(t *testing.T, mode, task string) (*b1DeliverEnv, ChatRef, *[]c1Call) {
	t.Helper()
	e := newB1DeliverEnv(t, -1001234567890)
	var calls []c1Call
	e.router.OnTaskTopicOpen(func(ws, id string) { calls = append(calls, c1Call{ws, id}) })
	if _, to, err := e.eng.deliver(context.Background(), e.route(mode, task), OutMessage{Text: "mở"}); err != nil || to.ThreadID == 0 {
		t.Fatalf("setup deliver: to=%+v err=%v", to, err)
	}
	row := e.store.only(t)
	chat := e.chat
	chat.ThreadID = row.ThreadID
	e.store.mu.Lock()
	for _, r := range e.store.rows {
		r.State = TopicClosed
	}
	e.store.mu.Unlock()
	return e, chat, &calls
}

// TC-C1-6 (C1): Recover reopening a closed task topic calls the lifecycle hook once.
func TestPhase7bC1_RecoverReopenCallsHook(t *testing.T) {
	ctx := context.Background()
	e, chat, calls := c1Setup(t, ReportTopicTask, "86abc")
	before := len(*calls)
	out, ok := e.router.Recover(ctx, chat, p6bErr("topic_closed"))
	if !ok || out.ThreadID != chat.ThreadID {
		t.Fatalf("reopen did not retry in the topic: ok=%v chat=%+v", ok, out)
	}
	if len(*calls) != before+1 || (*calls)[before] != (c1Call{p6bWS, "86abc"}) {
		t.Fatalf("hook calls after reopen: %+v", *calls)
	}
	if e.client.reopens != 1 {
		t.Fatalf("reopens=%d", e.client.reopens)
	}
	if st := e.store.only(t).State; st != TopicOpen {
		t.Fatalf("state=%q", st)
	}
}

// TC-C1-7 (C1): the hook is not called for a failed reopen, other errors, or a non-task topic.
func TestPhase7bC1_RecoverHookNotCalledWhenNotReopened(t *testing.T) {
	ctx := context.Background()
	t.Run("reopen fails", func(t *testing.T) {
		e, chat, calls := c1Setup(t, ReportTopicTask, "86abc")
		before := len(*calls)
		e.client.reopenErr = p6bErr("topic_no_rights")
		out, ok := e.router.Recover(ctx, chat, p6bErr("topic_closed"))
		if !ok || out.ThreadID != 0 || len(*calls) != before {
			t.Fatalf("ok=%v chat=%+v calls=%+v", ok, out, *calls)
		}
	})
	t.Run("other send errors", func(t *testing.T) {
		e, chat, calls := c1Setup(t, ReportTopicTask, "86abc")
		before := len(*calls)
		for _, code := range []string{"topic_not_found", "topic_not_forum", "rate_limited", "send_failed"} {
			e.router.Recover(ctx, chat, p6bErr(code))
		}
		if len(*calls) != before {
			t.Fatalf("hook called: %+v", *calls)
		}
	})
	t.Run("workflow topic", func(t *testing.T) {
		e := newB1DeliverEnv(t, -1001234567890)
		var calls []c1Call
		e.router.OnTaskTopicOpen(func(ws, id string) { calls = append(calls, c1Call{ws, id}) })
		c := e.route(ReportTopicWorkflow, "")
		c.topic.ExecutionID = 77
		if _, to, err := e.eng.deliver(ctx, c, OutMessage{Text: "wf"}); err != nil || to.ThreadID == 0 {
			t.Fatalf("to=%+v err=%v", to, err)
		}
		chat := e.chat
		chat.ThreadID = e.store.only(t).ThreadID
		if _, ok := e.router.Recover(ctx, chat, p6bErr("topic_closed")); !ok {
			t.Fatal("workflow topic not reopened")
		}
		if len(calls) != 0 {
			t.Fatalf("workflow topic reported as a task topic: %+v", calls)
		}
	})
	t.Run("no hook, nil router, General", func(t *testing.T) {
		e := newB1DeliverEnv(t, -1001234567890)
		if _, _, err := e.eng.deliver(ctx, e.route(ReportTopicTask, "86abc"), OutMessage{Text: "x"}); err != nil {
			t.Fatal(err)
		}
		chat := e.chat
		chat.ThreadID = e.store.only(t).ThreadID
		if _, ok := e.router.Recover(ctx, chat, p6bErr("topic_closed")); !ok {
			t.Fatal("reopen without a hook must still work")
		}
		var nilRouter *TopicRouter
		if _, ok := nilRouter.Recover(ctx, chat, p6bErr("topic_closed")); ok {
			t.Fatal("nil router recovered")
		}
		general := e.chat
		if _, ok := e.router.Recover(ctx, general, p6bErr("topic_closed")); ok {
			t.Fatal("General (thread 0) must not be recovered")
		}
		if _, ok := e.router.Recover(ctx, chat, nil); ok {
			t.Fatal("nil error recovered")
		}
	})
}

// TC-C1-8 (C1): after Recover reopens a topic, the 30 s sweep does not close it again.
func TestPhase7bC1_RecoverReopenedTopicNotReclosedBySweep(t *testing.T) {
	ctx := context.Background()
	e := newLCEnv(t, "", lcTopic(1, lcWS, "T1", 10))
	e.status, e.stype = "Complete", "done"
	e.event(lcWS, "T1", "Complete", "done")
	if e.store.state(1) != TopicClosed {
		t.Fatal("not closed by the done event")
	}
	d := newB1DeliverEnv(t, -1001234567890)
	d.chat.WorkspaceID = lcWS
	d.router.OnTaskTopicOpen(e.l.TopicReopened)
	if _, _, err := d.eng.deliver(ctx, d.route(ReportTopicTask, "T1"), OutMessage{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	chat := d.chat
	chat.ThreadID = d.store.only(t).ThreadID
	d.store.mu.Lock()
	for _, r := range d.store.rows {
		r.State = TopicClosed
	}
	d.store.mu.Unlock()
	// The send was rejected because the topic was closed: Recover reopens it.
	if _, ok := d.router.Recover(ctx, chat, p6bErr("topic_closed")); !ok {
		t.Fatal("not recovered")
	}
	e.store.SetTopicState(ctx, lcWS, 1, TopicOpen) // the same reopen, in the lifecycle's own view
	for i := 0; i < 3; i++ {
		e.clock = e.clock.Add(30 * time.Second)
		e.l.Sweep(ctx)
	}
	if e.store.state(1) != TopicOpen || len(e.closer.calls()) != 1 || len(e.reads) != 0 {
		t.Fatalf("sweep re-closed: state=%q closes=%v reads=%v", e.store.state(1), e.closer.calls(), e.reads)
	}
	// A new done event closes it again.
	e.event(lcWS, "T1", "Complete", "done")
	if e.store.state(1) != TopicClosed || len(e.closer.calls()) != 2 {
		t.Fatalf("new done event ignored: state=%q closes=%v", e.store.state(1), e.closer.calls())
	}
}
