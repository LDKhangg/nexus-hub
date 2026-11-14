package storage

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

type executorLLM struct {
	mu    sync.Mutex
	text  string
	calls int
}

func (m *executorLLM) Generate(context.Context, *domain.LLMRequest) (*domain.LLMResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return &domain.LLMResponse{Text: m.text}, nil
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

type executorCaller struct{}

func (executorCaller) Call(context.Context, int, string, json.RawMessage) tools.CallResult {
	return tools.CallResult{IsError: true, Status: "unexpected_fake_call"}
}

type behaviorExecutorFixture struct {
	e         *wfEnv
	model     *executorLLM
	messenger *executorMessenger
	worker    *workflow.Worker
}

func newBehaviorExecutorFixture(t *testing.T, e *wfEnv, modelText string, dryRun bool) *behaviorExecutorFixture {
	t.Helper()
	model := &executorLLM{text: modelText}
	messenger := &executorMessenger{}
	policy := executorPolicy{dryRun: dryRun}
	engine, err := behavior.NewEngine(behavior.ActionDeps{
		Store: e.behaviors, Defs: e.behaviors, Profiles: e.behaviors, Policies: policy,
		Tasks: executorTask{}, Chats: executorChat{}, Messenger: messenger,
		Callers: func(behavior.Run, string, tenant.ToolPolicyConfig, string) behavior.ToolCaller {
			return executorCaller{}
		},
	}, behavior.ActionSettings{})
	if err != nil {
		t.Fatal(err)
	}
	runWorker := behavior.NewRunWorker(behavior.RunWorkerDeps{
		Runs: e.behaviors, Defs: e.behaviors, Profiles: e.behaviors, Policies: policy,
		Models: func(context.Context, string) (domain.LLMProvider, error) { return model, nil },
		Callers: func(behavior.Run, tenant.ToolPolicyConfig, string, int, int) behavior.ToolCaller {
			return executorCaller{}
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
	clock := &manualClock{now: e.base}
	return &behaviorExecutorFixture{e: e, model: model, messenger: messenger,
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
	doc := executorDoc("executor-approval", true)
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
	if _, err := e.behaviors.Decide(e.ctx, workspaceA, approvals[0].Nonce, behavior.DecisionApproved, "", "owner", e.base); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	f.drain(t)
	if got := e.execution(workspaceA, br.ex.ID).State; got != workflow.StateCompleted {
		t.Fatalf("resumed execution state=%s", got)
	}
	if f.model.count() != 1 || f.messenger.count() != 2 {
		t.Fatalf("provider counts model=%d telegram=%d", f.model.count(), f.messenger.count())
	}
}
