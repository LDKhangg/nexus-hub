package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/reporting"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

type executorLLM struct {
	mu    sync.Mutex
	text  string
	usage domain.Usage
	calls int
}

func (m *executorLLM) Generate(context.Context, *domain.LLMRequest) (*domain.LLMResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return &domain.LLMResponse{Text: m.text, Usage: m.usage}, nil
}

func (m *executorLLM) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

type executorPolicy struct{ dryRun bool }

func (p executorPolicy) RunPolicy(context.Context, string, string) (tenant.AgentConfig, error) {
	return tenant.AgentConfig{Enabled: true, ConfidenceThreshold: .8, ClickUpTokenRef: "fake-clickup", Tools: tenant.ToolPolicyConfig{DryRun: p.dryRun}}, nil
}

type executorTask struct{}

func (executorTask) ReadTask(context.Context, string, string, string) (behavior.TaskState, error) {
	return behavior.TaskState{ID: "task-9", Description: "unchanged"}, nil
}

type executorChat struct{}

func (executorChat) OwnerChat(_ context.Context, ws string, _ behavior.TriggerData) (behavior.ChatRef, error) {
	return behavior.ChatRef{WorkspaceID: ws, IntegrationID: "fake-telegram", ChatID: 99}, nil
}

type executorMessenger struct {
	mu   sync.Mutex
	sent []behavior.OutMessage
}

func (m *executorMessenger) texts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.sent))
	for i := range m.sent {
		out[i] = m.sent[i].Text
	}
	return out
}

func (m *executorMessenger) SendMessage(_ context.Context, _ behavior.ChatRef, msg behavior.OutMessage) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return int64(100 + len(m.sent)), nil
}

func (*executorMessenger) RemoveButtons(context.Context, behavior.ChatRef, int64) error { return nil }
func (*executorMessenger) AnswerCallback(context.Context, behavior.ChatRef, string, string) error {
	return nil
}

func (m *executorMessenger) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

type executorCall struct {
	tool  string
	input json.RawMessage
}

type executorCaller struct {
	mu     sync.Mutex
	calls  []executorCall
	cancel context.CancelFunc
}

func (c *executorCaller) Call(_ context.Context, _ int, tool string, input json.RawMessage) tools.CallResult {
	c.mu.Lock()
	c.calls = append(c.calls, executorCall{tool: tool, input: append(json.RawMessage(nil), input...)})
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	switch tool {
	case behavior.ToolPRApprove:
		return tools.CallResult{Status: tools.StatusOK, Content: "pr=trusted/repo#17 outcome=approved review_id=73"}
	case behavior.ToolChatReaction:
		return tools.CallResult{Status: tools.StatusOK, Content: "message_id=message-7 reaction=white_check_mark outcome=reacted"}
	case behavior.ToolChatReactionRemove:
		return tools.CallResult{Status: tools.StatusOK, Content: "message_id=message-7 reaction=white_check_mark outcome=removed"}
	default:
		return tools.CallResult{IsError: true, Status: "unexpected_fake_call"}
	}
}

func (c *executorCaller) snapshot() []executorCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]executorCall(nil), c.calls...)
}

type behaviorExecutorFixture struct {
	e         *wfEnv
	model     *executorLLM
	messenger *executorMessenger
	caller    *executorCaller
	engine    *behavior.Engine
	runWorker *behavior.RunWorker
	worker    *workflow.Worker
}

func newBehaviorExecutorFixture(t *testing.T, e *wfEnv, modelText string, dryRun bool) *behaviorExecutorFixture {
	return newBehaviorExecutorFixtureAt(t, e, &executorLLM{text: modelText}, &executorMessenger{}, &executorCaller{}, dryRun, e.base, nil)
}

func newBehaviorExecutorFixtureAt(t *testing.T, e *wfEnv, model *executorLLM, messenger *executorMessenger, caller *executorCaller, dryRun bool, now time.Time, reports *reporting.Renderer) *behaviorExecutorFixture {
	t.Helper()
	policy := executorPolicy{dryRun: dryRun}
	engine, err := behavior.NewEngine(behavior.ActionDeps{
		Store: e.behaviors, Defs: e.behaviors, Profiles: e.behaviors, Policies: policy,
		Tasks: executorTask{}, Chats: executorChat{}, Messenger: messenger, Reports: reports,
		Callers: func(behavior.Run, string, tenant.ToolPolicyConfig, string) behavior.ToolCaller {
			return caller
		},
	}, behavior.ActionSettings{})
	if err != nil {
		t.Fatal(err)
	}
	runWorker := behavior.NewRunWorker(behavior.RunWorkerDeps{
		Runs: e.behaviors, Defs: e.behaviors, Profiles: e.behaviors, Policies: policy,
		Models: func(context.Context, string) (domain.LLMProvider, error) { return model, nil },
		Callers: func(behavior.Run, tenant.ToolPolicyConfig, string, int, int) behavior.ToolCaller {
			return caller
		},
		Dispatcher: engine, RunActions: engine, Notifier: engine,
	}, behavior.RunWorkerSettings{})
	steps, err := behavior.NewWorkflowSteps(runWorker, e.behaviors, e.store)
	if err != nil {
		t.Fatal(err)
	}
	if err := steps.EnableApprovalWaits(e.behaviors, engine); err != nil {
		t.Fatal(err)
	}
	reg := workflow.NewRegistry()
	if err := steps.Register(reg); err != nil {
		t.Fatal(err)
	}
	clock := &manualClock{now: now}
	return &behaviorExecutorFixture{e: e, model: model, messenger: messenger, caller: caller, engine: engine, runWorker: runWorker,
		worker: workflow.NewWorker(e.store, reg, clock, workflow.WorkerSettings{Workers: 1, PollInterval: time.Millisecond})}
}

func (f *behaviorExecutorFixture) drain(t *testing.T) {
	t.Helper()
	for n := 0; ; n++ {
		if n > 20 {
			t.Fatal("behavior workflow did not settle")
		}
		worked, err := f.worker.ProcessNext(f.e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			return
		}
	}
}

func executorDoc(name string, dryRun bool) behavior.Doc {
	notifyInformational := true
	requireCitations := true
	threshold := .8
	return behavior.Doc{
		Schema: 1, Name: name, Triggers: []behavior.TriggerDoc{{Type: behavior.TriggerCommentAdded}},
		Instructions: "Report the supplied event.", DryRun: &dryRun,
		Autonomy: &behavior.AutonomyDoc{ConfidenceThreshold: &threshold, RequireCitations: &requireCitations, NotifyWhenInformational: &notifyInformational},
		Actions: map[string][]behavior.ActionDoc{
			string(behavior.ClassAuto):   {{Type: behavior.ActionTelegramMessage}},
			string(behavior.ClassNotify): {{Type: behavior.ActionTelegramMessage}},
			string(behavior.ClassAsk):    {{Type: behavior.ActionTelegramMessage}},
		},
	}
}

func executorRunDoc(name string, dryRun bool) behavior.Doc {
	doc := executorDoc(name, dryRun)
	doc.Triggers = []behavior.TriggerDoc{{Type: behavior.TriggerChatMessage, Filter: behavior.FilterDoc{Channel: behavior.StringList{"dm"}}}}
	doc.Actions = map[string][]behavior.ActionDoc{behavior.ActionsRun: {
		{Type: behavior.ActionPRApprove, Params: map[string]string{"account": "owner"}},
		{Type: behavior.ActionChatReaction, Params: map[string]string{"reaction": "white_check_mark"}},
		{Type: behavior.ActionTelegramMessage},
	}}
	return doc
}

func executorApprovalDoc(name string) behavior.Doc {
	doc := executorDoc(name, true)
	doc.Schema = 2
	*doc.Autonomy.RequireCitations = false
	maxAttempts, stepTimeout, lease, deadline := 1, 60, 90, 3600
	doc.Workflow = &behavior.WorkflowDoc{
		Steps: []behavior.WorkflowStepDoc{
			{Key: "model", Kind: workflow.StepKindModel},
			{Key: "actions", Kind: workflow.StepKindActions, DependsOn: []string{"model"}},
		},
		Retry:                    &behavior.WorkflowRetryDoc{MaxAttempts: &maxAttempts, DelaySeconds: []int{}},
		StepTimeoutSeconds:       &stepTimeout,
		LeaseSeconds:             &lease,
		ExecutionDeadlineSeconds: &deadline,
	}
	return doc
}

func executorTrigger() behavior.TriggerData {
	return behavior.TriggerData{Type: behavior.TriggerChatMessage, IntegrationID: "fake-clickup", MessageID: "message-7", ChannelID: "channel-3", ChannelType: "dm", ChannelName: "release-room", TeamID: "42", PRs: []behavior.PRRef{{Repo: "trusted/repo", Number: 17}}}
}

func executorRun(t *testing.T, e *wfEnv, doc behavior.Doc, trigger behavior.TriggerData) bRun {
	t.Helper()
	v := e.publishEnabled(workspaceA, doc)
	raw, err := json.Marshal(trigger)
	if err != nil {
		t.Fatal(err)
	}
	run, created, err := e.behaviors.CreateRun(e.ctx, workspaceA, behavior.NewRun{BehaviorID: v.BehaviorID, VersionID: v.ID, Version: v.Version, TriggerKind: trigger.Type, Trigger: raw, Subject: behavior.ChatSubject(trigger.MessageID), Revision: "r-" + doc.Name, RunAt: e.base.Add(-time.Minute)})
	if err != nil || !created {
		t.Fatalf("create run: created=%v err=%v", created, err)
	}
	def := e.definitionOf(workspaceA, doc.Name, v.Version)
	req := behavior.WorkflowStartRequest(run, startable(def))
	req.At = e.base.Add(-time.Second)
	ex, err := e.store.StartExecution(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	return bRun{version: v, run: run, ex: ex}
}

func executorLegacyRun(t *testing.T, e *wfEnv, v behavior.Version, doc behavior.Doc, trigger behavior.TriggerData) behavior.Run {
	t.Helper()
	raw, err := json.Marshal(trigger)
	if err != nil {
		t.Fatal(err)
	}
	run, created, err := e.behaviors.CreateRun(e.ctx, workspaceA, behavior.NewRun{BehaviorID: v.BehaviorID, VersionID: v.ID, Version: v.Version, TriggerKind: trigger.Type, Trigger: raw, Subject: behavior.ChatSubject(trigger.MessageID), Revision: "legacy-" + doc.Name, RunAt: e.base.Add(-time.Minute)})
	if err != nil || !created {
		t.Fatalf("create legacy run: created=%v err=%v", created, err)
	}
	return run
}

// TC-C04/C05: the registered behavior model/actions executors use the real
// durable stores and workflow worker, and a second worker pass repeats neither
// the model request nor the Telegram provider effect.
func TestBehaviorWorkflowRegisteredExecutorsAreDurable(t *testing.T) {
	e := newWFEnv(t)
	doc := executorDoc("executor-notify", false)
	v := e.publishEnabled(workspaceA, doc)
	def := e.definitionOf(workspaceA, doc.Name, v.Version)
	br := e.runExecution(workspaceA, doc.Name, "task-9", &def)
	f := newBehaviorExecutorFixture(t, e, `{"items":[{"key":"notice","label":"finding","text":"Needs attention","topic":"quality","confidence":0.99,"informational":true}]}`, false)
	f.drain(t)
	f.drain(t)
	if got := e.execution(workspaceA, br.ex.ID).State; got != workflow.StateCompleted {
		t.Fatalf("execution state=%s", got)
	}
	if f.model.count() != 1 || f.messenger.count() != 1 {
		t.Fatalf("provider counts model=%d telegram=%d", f.model.count(), f.messenger.count())
	}
	items, err := e.behaviors.ListItems(e.ctx, workspaceA, br.run.ID)
	if err != nil || len(items) != 1 || items[0].Status != behavior.ItemDone {
		t.Fatalf("items=%#v err=%v", items, err)
	}
}

// TC-C08/C09/C10/C13: a dry-run auto item creates a bound approval through
// the actual actions executor; deciding it wakes the durable step and the
// registered continuation invokes Engine.ApplyDecision exactly once.
func TestBehaviorWorkflowApprovalContinuesThroughEngine(t *testing.T) {
	e := newWFEnv(t)
	doc := executorApprovalDoc("executor-approval")
	v := e.publishEnabled(workspaceA, doc)
	def := e.definitionOf(workspaceA, doc.Name, v.Version)
	br := e.runExecution(workspaceA, doc.Name, "task-9", &def)
	f := newBehaviorExecutorFixture(t, e, `{"items":[{"key":"draft","label":"finding","text":"Needs attention","proposal":"Post update","citations":[{"locator":"task:task-9:L1","quote":"evidence"}],"topic":"quality","confidence":0.99,"informational":false}]}`, true)
	f.drain(t)
	if got := e.execution(workspaceA, br.ex.ID).State; got != workflow.StateWaitingForApproval {
		t.Fatalf("waiting execution state=%s", got)
	}
	approvals, err := e.behaviors.ListApprovals(e.ctx, workspaceA, br.run.ID)
	if err != nil || len(approvals) != 1 || approvals[0].Kind != behavior.ApprovalPostDraft {
		t.Fatalf("approvals=%#v err=%v", approvals, err)
	}
	restartedWorkflow, err := NewWorkflowStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	restartedBehaviors, err := NewBehaviorStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	e.store, e.behaviors = restartedWorkflow, restartedBehaviors
	parallel(4, func(int) {
		_, _ = e.behaviors.Decide(e.ctx, workspaceA, approvals[0].Nonce, behavior.DecisionApproved, "", "owner", e.base)
	})
	f = newBehaviorExecutorFixtureAt(t, e, f.model, f.messenger, f.caller, true, e.base, nil)
	f.drain(t)
	f.drain(t)
	if got := e.execution(workspaceA, br.ex.ID).State; got != workflow.StateCompleted {
		t.Fatalf("resumed execution state=%s", got)
	}
	if f.model.count() != 1 || f.messenger.count() != 2 {
		t.Fatalf("provider counts model=%d telegram=%d", f.model.count(), f.messenger.count())
	}
	if e.signalCount(br.ex.ID) != 1 {
		t.Fatalf("duplicate callbacks recorded %d signals", e.signalCount(br.ex.ID))
	}
}

// TC-C08/TC-C09: several approvals created by the actual actions executor
// settle independently. Each wake continues the same last attempt, releases
// its lease while waiting and preserves the execution deadline.
func TestBehaviorWorkflowMultipleApprovalRoundsJoinOnLastAttempt(t *testing.T) {
	e := newWFEnv(t)
	doc := executorApprovalDoc("executor-approval-join")
	v := e.publishEnabled(workspaceA, doc)
	def := e.definitionOf(workspaceA, doc.Name, v.Version)
	if def.Retry.MaxAttempts != 1 || len(def.Retry.Delays) != 0 || def.LeaseDuration < def.StepTimeout+30*time.Second {
		t.Fatalf("unsafe normalized definition: %+v", def)
	}
	br := e.runExecution(workspaceA, doc.Name, "task-9", &def)
	model := `{"items":[` +
		`{"key":"one","label":"one","text":"finding one","proposal":"post one","topic":"quality","confidence":0.99,"informational":false},` +
		`{"key":"two","label":"two","text":"finding two","proposal":"post two","topic":"quality","confidence":0.99,"informational":false},` +
		`{"key":"three","label":"three","text":"finding three","proposal":"post three","topic":"quality","confidence":0.99,"informational":false}]}`
	f := newBehaviorExecutorFixture(t, e, model, true)
	f.drain(t)
	approvals, err := e.behaviors.ListApprovals(e.ctx, workspaceA, br.run.ID)
	if err != nil || len(approvals) != 3 {
		t.Fatalf("approvals=%#v err=%v", approvals, err)
	}
	original := e.execution(workspaceA, br.ex.ID)
	if original.State != workflow.StateWaitingForApproval || original.Deadline == nil || len(e.activeJobs(br.ex.ID)) != 0 {
		t.Fatalf("initial wait=%#v jobs=%s", original, describeJobs(e.activeJobs(br.ex.ID)))
	}
	selectedExpiry := e.base.Add(-time.Minute)
	if _, err := e.pool.Exec(e.ctx, `UPDATE approvals SET expires_at=$1 WHERE workspace_id=$2 AND id=$3`, selectedExpiry, workspaceA, approvals[2].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.behaviors.Decide(e.ctx, workspaceA, approvals[1].Nonce, behavior.DecisionRejected, "", "owner", e.base); err != nil {
		t.Fatal(err)
	}
	if worked, err := f.worker.ProcessNext(e.ctx); err != nil || !worked {
		t.Fatalf("reject continuation worked=%v err=%v", worked, err)
	}
	assertExecutorApprovalWait(t, e, br.ex.ID, *original.Deadline)
	if expired, err := e.behaviors.ExpireDue(e.ctx, e.base, 10); err != nil || len(expired) != 1 || expired[0].ID != approvals[2].ID {
		t.Fatalf("expired=%#v err=%v", expired, err)
	}
	if worked, err := f.worker.ProcessNext(e.ctx); err != nil || !worked {
		t.Fatalf("expiry continuation worked=%v err=%v", worked, err)
	}
	assertExecutorApprovalWait(t, e, br.ex.ID, *original.Deadline)
	if _, err := e.behaviors.Decide(e.ctx, workspaceA, approvals[0].Nonce, behavior.DecisionApproved, "", "owner", e.base); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	if got := e.execution(workspaceA, br.ex.ID); got.State != workflow.StateCompleted || got.Deadline == nil || !got.Deadline.Equal(*original.Deadline) {
		t.Fatalf("completed execution=%#v", got)
	}
	for id, want := range map[int64]behavior.ItemStatus{
		approvals[0].ItemID: behavior.ItemDone,
		approvals[1].ItemID: behavior.ItemSkipped,
		approvals[2].ItemID: behavior.ItemExpired,
	} {
		item, err := e.behaviors.GetItem(e.ctx, workspaceA, id)
		if err != nil || item.Status != want {
			t.Fatalf("item %d status=%s want=%s err=%v", id, item.Status, want, err)
		}
	}
}

func assertExecutorApprovalWait(t *testing.T, e *wfEnv, executionID int64, deadline time.Time) {
	t.Helper()
	ex := e.execution(workspaceA, executionID)
	step := e.step(workspaceA, executionID, "actions")
	if ex.State != workflow.StateWaitingForApproval || ex.Deadline == nil || !ex.Deadline.Equal(deadline) || step.Attempts != 1 || len(e.activeJobs(executionID)) != 0 {
		t.Fatalf("approval round execution=%#v step=%#v jobs=%s", ex, step, describeJobs(e.activeJobs(executionID)))
	}
}

// TC-C04: report preferences and templates are applied inside the registered
// workflow action executor. Undo then uses the same Engine and stored trusted
// outcomes, not a test-only rendering or action path.
func TestBehaviorWorkflowReportsAndUndoUseActualEngine(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefs  reporting.Prefs
		custom bool
		check  func(*testing.T, string)
	}{
		{name: "short", prefs: reporting.DefaultOwnerPrefs(), check: func(t *testing.T, text string) {
			if strings.Contains(text, "Model approved trusted target") || strings.Contains(text, "reacted") || strings.Contains(text, "Behavior:") {
				t.Fatalf("short report leaked detailed fields: %q", text)
			}
		}},
		{name: "detailed", prefs: reporting.Prefs{Style: reporting.StyleDetailed, ShowReaction: true, ShowAISummary: true, ShowLinks: true, ShowPRTitles: true, ShowTraceHint: true, Language: "en"}, check: func(t *testing.T, text string) {
			for _, want := range []string{"Model approved trusted target", "reacted", "Behavior:"} {
				if !strings.Contains(text, want) {
					t.Fatalf("detailed report %q lacks %q", text, want)
				}
			}
		}},
		{name: "custom", prefs: reporting.Prefs{Style: reporting.StyleDetailed, ShowReaction: true, ShowAISummary: true, ShowLinks: true, ShowPRTitles: true, ShowTraceHint: true, Language: "en"}, custom: true, check: func(t *testing.T, text string) {
			if text != "CUSTOM executor-report-custom|Model approved trusted target|trusted/repo" {
				t.Fatalf("custom report=%q", text)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newWFEnv(t)
			doc := executorRunDoc("executor-report-"+tc.name, false)
			min := .8
			doc.Verdict = &behavior.VerdictDoc{Labels: []string{"approve_request", "reject"}, MinConfidence: &min}
			br := executorRun(t, e, doc, executorTrigger())
			store := reporting.NewPostgresStore(e.pool)
			if _, err := store.SetPrefs(e.ctx, reporting.DefaultOwnerUserID, workspaceA, doc.Name, tc.prefs); err != nil {
				t.Fatal(err)
			}
			if tc.custom {
				if err := store.SetTemplate(e.ctx, reporting.DefaultOwnerUserID, workspaceA, doc.Name, reporting.KindPRApprove, "CUSTOM {behavior}|{ai_summary}|{pr_repo}"); err != nil {
					t.Fatal(err)
				}
			}
			model := &executorLLM{text: `{"items":[],"verdict":{"label":"approve_request","confidence":0.96,"summary":"Model approved trusted target"}}`}
			messenger := &executorMessenger{}
			caller := &executorCaller{}
			f := newBehaviorExecutorFixtureAt(t, e, model, messenger, caller, false, e.base, reporting.NewRenderer(store, nil))
			f.drain(t)
			texts := messenger.texts()
			if len(texts) != 1 {
				t.Fatalf("Telegram sends=%d, want 1", len(texts))
			}
			tc.check(t, texts[0])
			calls := caller.snapshot()
			if len(calls) != 2 || calls[0].tool != behavior.ToolPRApprove || calls[1].tool != behavior.ToolChatReaction || !strings.Contains(string(calls[0].input), `"repo":"trusted/repo"`) || !strings.Contains(string(calls[1].input), `"team_id":"42"`) {
				t.Fatalf("provider calls=%#v", calls)
			}
			legacyRun := executorLegacyRun(t, e, br.version, doc, executorTrigger())
			legacyModel := &executorLLM{text: model.text}
			legacyMessenger := &executorMessenger{}
			legacyCaller := &executorCaller{}
			legacy := newBehaviorExecutorFixtureAt(t, e, legacyModel, legacyMessenger, legacyCaller, false, e.base, reporting.NewRenderer(store, nil))
			worked, err := legacy.runWorker.ProcessNext(e.ctx)
			if err != nil || !worked {
				t.Fatalf("legacy worker worked=%v err=%v", worked, err)
			}
			legacyTexts := legacyMessenger.texts()
			workflowText := strings.ReplaceAll(texts[0], fmt.Sprintf("#%d", br.run.ID), "#RUN")
			legacyText := ""
			if len(legacyTexts) == 1 {
				legacyText = strings.ReplaceAll(legacyTexts[0], fmt.Sprintf("#%d", legacyRun.ID), "#RUN")
			}
			legacyCalls := legacyCaller.snapshot()
			if workflowText != legacyText || len(legacyCalls) != len(calls) {
				t.Fatalf("workflow report/calls differ from legacy: workflow=%q/%d legacy=%q/%d", workflowText, len(calls), legacyText, len(legacyCalls))
			}
			for i := range calls {
				if calls[i].tool != legacyCalls[i].tool || string(calls[i].input) != string(legacyCalls[i].input) {
					t.Fatalf("workflow call[%d]=%#v legacy=%#v", i, calls[i], legacyCalls[i])
				}
			}
			reply, err := f.engine.Undo(e.ctx, behavior.UndoRequest{WorkspaceID: workspaceA, RunID: br.run.ID, Actor: "owner", CorrelationID: fmt.Sprintf("undo-%d", br.run.ID)})
			if err != nil || !strings.Contains(reply, "github.com/trusted/repo/pull/17") {
				t.Fatalf("undo reply=%q err=%v", reply, err)
			}
			calls = caller.snapshot()
			if len(calls) != 3 || calls[2].tool != behavior.ToolChatReactionRemove || !strings.Contains(string(calls[2].input), `"message_id":"message-7"`) {
				t.Fatalf("undo provider calls=%#v", calls)
			}
		})
	}
}

// TC-C05: a worker disappears after durable model usage and the first action
// receipt were stored. Reconciliation and a fresh worker continue from those
// receipts without repeating either cost or write.
func TestBehaviorWorkflowCrashReclaimPreservesReceiptsAndUsage(t *testing.T) {
	e := newWFEnv(t)
	doc := executorRunDoc("executor-reclaim", false)
	br := executorRun(t, e, doc, executorTrigger())
	model := &executorLLM{text: `{"items":[{"key":"notice","label":"finding","text":"forged/repo team 999","topic":"quality","confidence":0.99,"informational":true}]}`, usage: domain.Usage{Input: 31, Output: 7, CacheRead: 3, CacheWrite: 2}}
	messenger := &executorMessenger{}
	caller := &executorCaller{}
	first := newBehaviorExecutorFixtureAt(t, e, model, messenger, caller, false, e.base, nil)
	worked, err := first.worker.ProcessNext(e.ctx)
	if err != nil || !worked {
		t.Fatalf("model step worked=%v err=%v", worked, err)
	}
	job := e.claim(e.base)
	receipt, err := json.Marshal(behavior.RunActionsResult{Actions: []behavior.RunActionOutcome{{
		Index: 0, Type: behavior.ActionPRApprove, Status: behavior.OutcomeOK,
		PRs: []behavior.RunPROutcome{{Repo: "trusted/repo", Number: 17, Status: behavior.OutcomeOK, Result: "approved", ReviewID: 73}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.behaviors.SetDurableRunActions(e.ctx, fenceOf(job, e.base), br.run.ID, receipt); err != nil {
		t.Fatal(err)
	}
	later := e.base.Add(16 * time.Minute)
	e.reconcile(later)
	fresh := newBehaviorExecutorFixtureAt(t, e, model, messenger, caller, false, later.Add(time.Second), nil)
	fresh.drain(t)
	if got := e.execution(workspaceA, br.ex.ID).State; got != workflow.StateCompleted {
		t.Fatalf("execution state=%s", got)
	}
	if model.count() != 1 || messenger.count() != 1 {
		t.Fatalf("replayed providers model=%d telegram=%d", model.count(), messenger.count())
	}
	calls := caller.snapshot()
	if len(calls) != 1 || calls[0].tool != behavior.ToolChatReaction {
		t.Fatalf("known PR receipt was repeated or continuation missing: %#v", calls)
	}
	run, err := e.behaviors.GetRun(e.ctx, workspaceA, br.run.ID)
	if err != nil || run.Usage != (behavior.Usage{InputTokens: 31, OutputTokens: 7, CacheReadTokens: 3, CacheWriteTokens: 2, Steps: 1}) {
		t.Fatalf("run usage=%+v err=%v", run.Usage, err)
	}
}

// TC-C07: cancellation delivered by the first provider write is observed
// before the executor can begin the next action.
func TestBehaviorWorkflowCancellationBetweenActionsBlocksNextWrite(t *testing.T) {
	e := newWFEnv(t)
	doc := executorRunDoc("executor-cancel-between-actions", false)
	executorRun(t, e, doc, executorTrigger())
	model := &executorLLM{text: `{"items":[]}`}
	messenger := &executorMessenger{}
	caller := &executorCaller{}
	f := newBehaviorExecutorFixtureAt(t, e, model, messenger, caller, false, e.base, nil)
	worked, err := f.worker.ProcessNext(e.ctx)
	if err != nil || !worked {
		t.Fatalf("model step worked=%v err=%v", worked, err)
	}
	ctx, cancel := context.WithCancel(e.ctx)
	caller.cancel = cancel
	worked, err = f.worker.ProcessNext(ctx)
	cancel()
	if !worked || err == nil {
		t.Fatalf("canceled action step worked=%v err=%v", worked, err)
	}
	calls := caller.snapshot()
	if len(calls) != 1 || calls[0].tool != behavior.ToolPRApprove || messenger.count() != 0 {
		t.Fatalf("writes after cancellation: calls=%#v telegram=%d", calls, messenger.count())
	}
}

// TC-C06/C07: an already-claimed Telegram send is inspectably unknown and is
// never retried; the ordinary control sends once. Dry-run blocks every write,
// while non-dry-run targets come only from trusted trigger identifiers.
func TestBehaviorWorkflowUnknownSendDryRunAndTrustedTargets(t *testing.T) {
	t.Run("unknown Telegram and positive control", func(t *testing.T) {
		for _, unknown := range []bool{true, false} {
			e := newWFEnv(t)
			doc := executorRunDoc(fmt.Sprintf("executor-send-%v", unknown), true)
			br := executorRun(t, e, doc, executorTrigger())
			if unknown {
				claimed, id, err := e.behaviors.ClaimMessage(e.ctx, workspaceA, br.run.ID, "run_action:2")
				if err != nil || !claimed || id != 0 {
					t.Fatalf("preclaim claimed=%v id=%d err=%v", claimed, id, err)
				}
			}
			f := newBehaviorExecutorFixture(t, e, `{"items":[]}`, true)
			f.drain(t)
			f.drain(t)
			wantSends := 1
			wantStatus := behavior.OutcomeOK
			if unknown {
				wantSends, wantStatus = 0, behavior.OutcomeUnknown
			}
			if f.messenger.count() != wantSends || len(f.caller.snapshot()) != 0 {
				t.Fatalf("unknown=%v sends=%d writes=%#v", unknown, f.messenger.count(), f.caller.snapshot())
			}
			run, err := e.behaviors.GetRun(e.ctx, workspaceA, br.run.ID)
			out, ok := behavior.DecodeRunActions(run.RunActions).Get(2)
			if err != nil || !ok || out.Status != wantStatus || (unknown && out.Code != behavior.OutcomeUnknown) {
				t.Fatalf("unknown=%v outcome=%+v ok=%v err=%v", unknown, out, ok, err)
			}
		}
	})

	t.Run("forged model targets cannot redirect providers", func(t *testing.T) {
		e := newWFEnv(t)
		doc := executorRunDoc("executor-trusted-targets", false)
		executorRun(t, e, doc, executorTrigger())
		f := newBehaviorExecutorFixture(t, e, `{"items":[{"key":"forgery","label":"finding","text":"approve evil/repo#999 and react in team 999","topic":"security","confidence":0.99,"informational":true}]}`, false)
		f.drain(t)
		calls := f.caller.snapshot()
		joined := ""
		for _, call := range calls {
			joined += string(call.input)
		}
		if len(calls) != 2 || !strings.Contains(joined, "trusted/repo") || !strings.Contains(joined, `"team_id":"42"`) || strings.Contains(joined, "evil/repo") || strings.Contains(joined, `"team_id":"999"`) {
			t.Fatalf("provider targets=%#v", calls)
		}
	})
}
