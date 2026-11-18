package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// This file is the durable side of the orchestrator (Phase 7b P4): the
// built-in workflow orchestrated_task and its step executors. The engine
// runs a static step graph, so the plan loop is unrolled into slots:
//
//	plan → run_01 → eval_01 → … → run_NN → eval_NN → report
//
// A run slot runs the next ready task of the active plan (or nothing); the
// following eval slot decides what follows its result. Once a terminal
// decision (COMPLETE or FAIL) is recorded the remaining slots finish at
// once, and report tells the owner the outcome. The slot count bounds the
// number of task runs of one execution, so the loop always ends.
//
// Every step re-reads the durable state (plans, plan tasks and decisions in
// the PlanStore, the budget snapshot in the step checkpoints), so a retried
// or woken step continues where it stopped. A decision a step recorded
// carries the step's marker evidence reference, so a retry applies it again
// instead of asking the model twice. No step performs a side effect other
// than through its ports: worker jobs through the 7a job store, owner
// questions and decisions through the existing approval path, and
// in-process executors that must use the existing action authorizer.

// Built-in definition names.
const (
	// InternalOrchestratedTaskKind is the internal definition kind.
	InternalOrchestratedTaskKind = "orchestrated_task"
	// WorkflowSourceOrchestratedTask is the execution source kind.
	WorkflowSourceOrchestratedTask = "orchestrated_task"
	// StepKeyPlan and StepKeyReport are the first and last steps.
	StepKeyPlan   = "plan"
	StepKeyReport = "report"
	// OrchestratedTaskSlots is the number of run/eval slot pairs: the most
	// task runs one execution can make (1 + 2*30 + 1 = 62 ≤ MaxSteps).
	OrchestratedTaskSlots = 30
	// orchestratedTaskStartDelay lets the starter store the request
	// instruction before the plan step reads it.
	orchestratedTaskStartDelay = 2 * time.Second
	// orchestratedTaskDeadline is the hard end of one execution, whatever
	// its budget extensions.
	orchestratedTaskDeadline = 24 * time.Hour
)

// Checkpoint reference kinds of orchestrator steps.
const (
	// CheckpointBudget holds the budget snapshot (REQ-10); ReferenceID is
	// its base64url JSON.
	CheckpointBudget = "orchestrator_budget"
	// CheckpointTask names the task a run slot ran ("-" for none).
	CheckpointTask = "orchestrator_task"
	// CheckpointWait says what a waiting step waits for.
	CheckpointWait = "orchestrator_wait"
	// CheckpointWorkerJob references the worker job of a run slot.
	CheckpointWorkerJob = "worker_job"
	// CheckpointWakeAt is the durable timer of a worker wait (Phase 10
	// REQ-11): unix seconds of the wall-time budget's end. The worker task
	// sweeper wakes the step once it passed, so the wait always ends.
	CheckpointWakeAt = "orchestrator_wake_at"
	noTask           = "-"
)

// Reason codes of orchestrated executions (decision rows and step
// failures).
const (
	ReasonMissingEvidence     = "missing_evidence"
	ReasonNoExecutor          = "no_executor"
	ReasonNoRunnableTask      = "no_runnable_task"
	ReasonTaskSlotsExhausted  = "task_slots_exhausted"
	ReasonApprovalRejected    = "approval_rejected"
	ReasonApprovalExpired     = "approval_expired"
	ReasonOrchestratorFailed  = "orchestrator_failed"
	ReasonInvalidInputCode    = "invalid_input"
	ReasonLLMError            = "llm_error"
	ReasonStorageUnavailable  = "storage_unavailable"
	ReasonInstructionPending  = "instruction_pending"
	ReasonRouteMissing        = "orchestrator_route_missing"
	ReasonRepoUnresolved      = "repo_unresolved"
	ReasonExecutorUnavailable = "executor_unavailable"
	ReasonExecutorInterrupted = "executor_interrupted"
	ReasonWorkerCancelled     = "worker_cancelled"
	ReasonWorkerFailed        = "worker_failed"
	ReasonClarified           = "clarification_answered"
	ReasonAllTasksSucceeded   = "all_tasks_succeeded"
	ReasonApproved            = "approved"
	// ReasonExecutorFailed: an in-process executor returned an error.
	ReasonExecutorFailed = "executor_failed"
)

// KnownWorkflowReasons returns every code an orchestrated execution can end
// a step or the execution with.
func KnownWorkflowReasons() []workflow.ReasonCode {
	out := []workflow.ReasonCode{}
	for _, r := range []string{
		ReasonPlanningBudgetExceeded, ReasonInvalidOutput, ReasonMissingEvidence, ReasonNoExecutor,
		ReasonNoRunnableTask, ReasonTaskSlotsExhausted, ReasonApprovalRejected, ReasonApprovalExpired,
		ReasonOrchestratorFailed, ReasonInvalidInputCode, ReasonLLMError, ReasonStorageUnavailable,
		ReasonInstructionPending, ReasonRouteMissing,
	} {
		out = append(out, workflow.ReasonCode(r))
	}
	return out
}

// RunStepKey and EvalStepKey name slot i (1-based).
func RunStepKey(i int) string  { return fmt.Sprintf("run_%02d", i) }
func EvalStepKey(i int) string { return fmt.Sprintf("eval_%02d", i) }

// OrchestratedTaskDefinition is the immutable orchestration of the built-in
// orchestrated_task workflow. A step makes at most a few model calls (with
// repairs), so its timeout leaves room for slow CLI providers; waits for
// the owner or a worker are durable workflow waits.
func OrchestratedTaskDefinition() workflow.Definition {
	steps := []workflow.StepDefinition{{Key: StepKeyPlan, Kind: workflow.StepKindOrchestratorPlan}}
	prev := StepKeyPlan
	for i := 1; i <= OrchestratedTaskSlots; i++ {
		run, eval := RunStepKey(i), EvalStepKey(i)
		steps = append(steps,
			workflow.StepDefinition{Key: run, Kind: workflow.StepKindOrchestratorRunTask, DependsOn: []string{prev}},
			workflow.StepDefinition{Key: eval, Kind: workflow.StepKindOrchestratorEvaluate, DependsOn: []string{run}})
		prev = eval
	}
	steps = append(steps, workflow.StepDefinition{Key: StepKeyReport, Kind: workflow.StepKindOrchestratorReport, DependsOn: []string{prev}})
	return workflow.Definition{
		Steps:             steps,
		Retry:             workflow.RetryPolicy{MaxAttempts: 3, Delays: []time.Duration{5 * time.Second, 30 * time.Second}},
		StepTimeout:       10 * time.Minute,
		LeaseDuration:     11 * time.Minute,
		ExecutionDeadline: orchestratedTaskDeadline,
	}
}

// OrchestratedTaskSnapshot is the document of the internal version.
type OrchestratedTaskSnapshot struct {
	Schema   int                 `json:"schema"`
	Kind     string              `json:"kind"`
	Workflow workflow.Definition `json:"workflow"`
}

// NewOrchestratedTaskSnapshot snapshots the current definition.
func NewOrchestratedTaskSnapshot() OrchestratedTaskSnapshot {
	return OrchestratedTaskSnapshot{Schema: 1, Kind: InternalOrchestratedTaskKind, Workflow: OrchestratedTaskDefinition()}
}

// TaskRequest asks to start one orchestrated task (/plan, start_task).
// Repo is the repository the owner named explicitly ("" when none). The
// request text is not part of it: the starter stores it as the
// execution's first instruction right after the start.
type TaskRequest struct {
	WorkspaceID   string
	IntegrationID string
	ChatID        int64
	ThreadID      int64
	UpdateID      int64
	Repo          string
	TaskRef       string
	ClickUpTaskID string
}

// TriggerKey is the idempotency key: one execution per inbound update.
func (r TaskRequest) TriggerKey() string {
	return fmt.Sprintf("orchestrated_task:%s:%d:%d", r.IntegrationID, r.ChatID, r.UpdateID)
}

// TaskStartRequest builds the workflow start request.
func TaskStartRequest(r TaskRequest, versionID int64, correlationID string, now time.Time) workflow.StartRequest {
	subject := ""
	if r.ClickUpTaskID != "" {
		subject = "clickup:task:" + r.ClickUpTaskID
	}
	return workflow.StartRequest{
		WorkspaceID: r.WorkspaceID, VersionID: versionID, TriggerKey: r.TriggerKey(), CorrelationID: correlationID,
		Definition: OrchestratedTaskDefinition(),
		References: workflow.References{
			SourceKind: WorkflowSourceOrchestratedTask, SourceID: fmt.Sprintf("%d:%d", r.ChatID, r.UpdateID),
			Subject: subject, OperationKey: r.TriggerKey(),
		},
		At: now.Add(orchestratedTaskStartDelay),
	}
}

// --- ports ---

// ErrRouteNotFound means the execution has no orchestrator route.
var ErrRouteNotFound = errors.New("orchestrator: route not found")

// Route is where the messages of an orchestrated task go and what it works
// on. Ids only.
type Route struct {
	WorkspaceID   string
	ExecutionID   int64
	IntegrationID string
	ChatID        int64
	ThreadID      int64
	Repo          string // explicit repo, "" when none
	TaskRef       string
	ClickUpTaskID string
}

// RouteReader reads routes; ErrRouteNotFound when the execution has none.
type RouteReader interface {
	GetRoute(ctx context.Context, workspaceID string, executionID int64) (Route, error)
}

// StepReader lists the persisted steps of an execution.
type StepReader interface {
	ListSteps(ctx context.Context, workspaceID string, executionID int64) ([]workflow.Step, error)
}

// StepWaker queues a waiting step again (no signal needed); ErrNotEligible
// when it is not waiting. The worker task sweeper also wakes run steps
// whose wake time passed, whose retried job runs again, or whose ClickUp
// draft ended (storage.WorkerTaskStore.SyncWorkerTasks).
type StepWaker interface {
	WakeWorkerStep(ctx context.Context, workspaceID string, executionID int64, stepID string) error
}

// RepoSource lists registered repositories and remembers the last repo of a
// ClickUp task (7a local tasks).
type RepoSource interface {
	RegisteredRepos(ctx context.Context, workspaceID string) ([]string, error)
	LastTaskRepo(ctx context.Context, workspaceID, clickUpTaskID string) (string, error)
}

// Worker job states (the 7a job store's values).
const (
	JobWaiting     = "WAITING_FOR_WORKER"
	JobLeased      = "LEASED"
	JobRunning     = "RUNNING"
	JobCancelling  = "CANCELLING"
	JobInterrupted = "INTERRUPTED"
	JobSucceeded   = "SUCCEEDED"
	JobFailed      = "FAILED"
	JobNeedsInput  = "NEEDS_INPUT"
	JobCancelled   = "CANCELLED"
)

// WorkerJob is what a run slot needs of a 7a worker job and its result.
type WorkerJob struct {
	ID          string
	State       string
	ReasonCode  string
	Branch      string
	PRURL       string
	TestOutcome string
}

// WorkerJobs creates and reads the worker job of a step (7a job store).
type WorkerJobs interface {
	JobForStep(ctx context.Context, workspaceID string, executionID int64, stepKey string) (WorkerJob, bool, error)
	CreateJob(ctx context.Context, workspaceID string, executionID int64, stepKey string, caps []string, payload json.RawMessage) (WorkerJob, error)
}

// Fence is the workflow job claim an owner-decision write presents.
type Fence struct {
	JobID       int64
	WorkspaceID string
	ExecutionID int64
	StepKey     string
	Attempt     int
	ClaimToken  string
	At          time.Time
}

// Gate decisions as stored on the approval.
const (
	GateApproved = "approved"
	GateRejected = "rejected"
	GateExpired  = "expired"
)

// Gate asks the owner for one decision with two buttons, bound to the
// waiting step (the existing approval path: chat-bound, idempotent,
// expiring).
type Gate struct {
	Purpose string // fixed code: approval, budget_<kind>
	Text    string
	Approve string
	Reject  string
	TTL     time.Duration
}

// GateWait is one gate bound to a step and its decision.
type GateWait struct {
	ApprovalID int64
	Purpose    string
	Decision   string // "" while open
	SignalID   int64
	Consumed   bool
}

// Owner is how steps talk to the owner. Say sends a message to the task
// topic (else the workflow topic, else the route chat).
type Owner interface {
	Say(ctx context.Context, route Route, text string) error
	AskGate(ctx context.Context, f Fence, route Route, g Gate) error
	GateWaits(ctx context.Context, workspaceID string, executionID int64, stepKey string) ([]GateWait, error)
	ConsumeGate(ctx context.Context, f Fence, signalID int64) error
}

// FinalSayer is the optional part of an Owner that sends the final report
// of an execution (it may add buttons to it). Without it the report is sent
// with Say.
type FinalSayer interface {
	SayFinal(ctx context.Context, route Route, text string) error
}

// TaskRun is one task handed to an in-process executor.
type TaskRun struct {
	WorkspaceID string
	ExecutionID int64
	StepKey     string
	Task        PlanTask
	Route       Route
	Fence       Fence
	// Request is the owner's request (untrusted text); executors bound it
	// and pass it only as data.
	Request string
	// TokensLeft is what the execution's token budget has left when the
	// task starts; an executor never plans to spend more.
	TokensLeft int64
	// Inputs are the bounded results of the task's succeeded dependencies
	// (summary, PR URL, branch; untrusted text), one line each, in
	// dependency order. Executors pass them only as data.
	Inputs []string
}

// TaskOutcome is an in-process executor's structured result. Evidence must
// reference what the executor produced in this execution.
type TaskOutcome struct {
	Succeeded  bool
	ReasonCode string
	Evidence   []EvidenceRef
	Usage      Usage
	// Summary is a short untrusted result text; it is stored bounded in the
	// task result.
	Summary string
	// Wait, when not empty, means the task is not finished: it waits for
	// the owner (for example a ClickUp draft's confirm, Phase 10 REQ-10).
	// It is the executor's own reference (at most 128 bytes, no spaces);
	// the run step waits in WAITING_FOR_APPROVAL and, once woken, asks the
	// executor again through TaskResumer. Only a TaskResumer may wait.
	Wait string
	// Blocked marks a task that did not fail but cannot go on (the owner
	// cancelled, or a draft expired): the task becomes BLOCKED and
	// Evaluate decides.
	Blocked bool
}

// TaskResumer is implemented by an in-process executor whose tasks may wait
// (TaskOutcome.Wait). ResumeTask reads the task's state again: waitRef is
// the reference the executor returned, or "" when the step only knows the
// task started (an attempt interrupted before it recorded the wait); the
// executor then looks its work up by run, or reports
// executor_interrupted when it cannot tell. It never repeats a side effect.
type TaskResumer interface {
	ResumeTask(ctx context.Context, run TaskRun, waitRef string) (TaskOutcome, error)
}

// WorkerJobRoutes is optionally implemented by WorkerJobs: PrepareJob is
// called right before a run slot creates its worker job, so the job's topic
// messages (progress, Retry/Cancel) go to the execution's topic like a 7a
// /task job (Phase 10 REQ-12). An error is logged and the job still runs.
type WorkerJobRoutes interface {
	PrepareJob(ctx context.Context, route Route, stepKey, repo string, instructionVersion int64) error
}

// TaskExecutor runs one task in process. An executor never performs a side
// effect directly: writes go through the existing action authorizer and
// approval path.
type TaskExecutor interface {
	RunTask(ctx context.Context, run TaskRun) (TaskOutcome, error)
}

// StepSettings are the trusted settings of the steps.
type StepSettings struct {
	Caps BudgetCaps
	// MaxRepair is the configured repair bound per call (also capped by
	// the repair budget).
	MaxRepair int
	// MaxTasks bounds plan tasks; <= 0 means MaxPlanTasks.
	MaxTasks int
	// PlanEstimate and EvaluateEstimate are the tokens a call is expected
	// to spend; a call is not started when they do not fit.
	PlanEstimate, EvaluateEstimate int64
	// ExecuteEstimate is the tokens an in-process executor is expected to
	// spend; the task is not started when they do not fit.
	ExecuteEstimate int64
	// GateTTL is how long the owner has to decide; <= 0 means 24 h.
	GateTTL time.Duration
}

// DefaultStepSettings returns the O5a defaults.
func DefaultStepSettings() StepSettings {
	return StepSettings{
		Caps: DefaultBudgetCaps(), MaxRepair: DefaultMaxRepair,
		PlanEstimate: 4000, EvaluateEstimate: 2000, ExecuteEstimate: 4000, GateTTL: 24 * time.Hour,
	}
}

// Steps executes the orchestrated task step kinds.
type Steps struct {
	// Runtime returns the model runtime of a workspace.
	Runtime func(ctx context.Context, workspaceID string) (AIRuntime, error)
	// Template carries models, MaxRepair and vocabularies; Runtime and
	// BeforeAttempt are set per call.
	Template     Orchestrator
	Registry     *CapabilityRegistry
	Plans        PlanStore
	Steps        StepReader
	Instructions workflow.InstructionReader
	Routes       RouteReader
	Repos        RepoSource
	Jobs         WorkerJobs
	Owner        Owner
	// Executors are the in-process executors by executor name
	// (ExecutorInvestigate, ExecutorBehaviorActions). Plan is offered only
	// the capabilities of wired executors; a missing one fails its tasks
	// with executor_unavailable.
	Executors map[string]TaskExecutor
	Settings  StepSettings
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// offeredCapabilities are the capabilities Plan may use: only those of
// executors that are wired (an in-process executor in Executors, a worker
// profile when Jobs is set). Unwired executors' capabilities are never
// offered.
func (s *Steps) offeredCapabilities() []string {
	return s.Registry.CapabilitiesFor(func(p ExecutorProfile) bool {
		if p.Worker {
			return s.Jobs != nil
		}
		return s.Executors[p.Name] != nil
	})
}

func (s *Steps) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

type stepKind struct {
	s    *Steps
	kind string
}

func (k stepKind) Execute(ctx context.Context, sc workflow.StepContext) (workflow.StepResult, error) {
	r, err := k.s.load(ctx, sc)
	if err != nil {
		return workflow.StepResult{}, err
	}
	switch k.kind {
	case workflow.StepKindOrchestratorPlan:
		return r.planStep()
	case workflow.StepKindOrchestratorRunTask:
		return r.runStep()
	case workflow.StepKindOrchestratorEvaluate:
		return r.evalStep()
	case workflow.StepKindOrchestratorReport:
		return r.reportStep()
	}
	return workflow.StepResult{}, &workflow.StepError{Code: workflow.ReasonNonRetryable}
}

// Register adds the four step kinds to the registry.
func (s *Steps) Register(reg *workflow.Registry) error {
	if s == nil || s.Runtime == nil || s.Registry == nil || s.Plans == nil || s.Steps == nil ||
		s.Instructions == nil || s.Routes == nil || s.Owner == nil {
		return workflow.ErrInvalidInput
	}
	for _, kind := range []string{
		workflow.StepKindOrchestratorPlan, workflow.StepKindOrchestratorRunTask,
		workflow.StepKindOrchestratorEvaluate, workflow.StepKindOrchestratorReport,
	} {
		if err := reg.Register(kind, stepKind{s: s, kind: kind}); err != nil {
			return err
		}
	}
	return nil
}

// InstructionWaker wakes the waiting orchestrator step of an execution
// after the owner added an instruction (the answer to a clarification).
// A step that does not wait, or waits for something else, re-checks and
// waits again.
type InstructionWaker struct {
	Steps StepReader
	Waker StepWaker
}

// InstructionAdded wakes the execution's waiting orchestrator step, if any.
// Errors are returned without detail; the caller ignores them (the owner can
// still /resume).
func (w *InstructionWaker) InstructionAdded(ctx context.Context, workspaceID string, executionID int64) error {
	if w == nil || w.Steps == nil || w.Waker == nil || executionID <= 0 {
		return nil
	}
	steps, err := w.Steps.ListSteps(ctx, workspaceID, executionID)
	if err != nil {
		return err
	}
	for _, st := range steps {
		if st.State != workflow.StepWaiting || !isOrchestratorKind(st.Kind) {
			continue
		}
		if err := w.Waker.WakeWorkerStep(ctx, workspaceID, executionID, st.Key); err != nil && !errors.Is(err, workflow.ErrNotEligible) {
			return err
		}
	}
	return nil
}

func isOrchestratorKind(kind string) bool {
	switch kind {
	case workflow.StepKindOrchestratorPlan, workflow.StepKindOrchestratorRunTask,
		workflow.StepKindOrchestratorEvaluate, workflow.StepKindOrchestratorReport:
		return true
	}
	return false
}
