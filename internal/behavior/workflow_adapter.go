package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// This file executes behavior runs as durable workflow steps. The run row
// (behavior_runs) stays the run's identity, budget, receipts and read
// projection; the workflow job claim is the only authority to write it.
//
// Steps reuse the run executor's operations (RunWorker) and the action
// library (Dispatcher, RunActionRunner) unchanged, so policy, tool caps,
// dry-run, trusted targets, reports, verdict gates and undo behave as for a
// legacy run. Every operation key stays bound to the run ID: a step retry or
// a retry execution of the same run reuses the tool-run, message and action
// keys, so a call whose receipt is missing is reported as an unknown
// outcome by the existing receipts and never sent again.
//
// Checkpoints hold references only (run, context, verdict, item and action
// identifiers and codes), never message text, model output or tool output.

// WorkflowSourceRun is the execution source kind of a behavior run.
const WorkflowSourceRun = "behavior_run"

// Checkpoint reference kinds of behavior steps.
const (
	CheckpointRun        = "behavior_run"         // ReferenceID: run ID
	CheckpointRunContext = "behavior_run_context" // the run's stored context snapshot
	CheckpointRunVerdict = "behavior_run_verdict" // the run's stored verdict
	CheckpointRunItem    = "run_item"             // ReferenceID: item ID
	CheckpointDispatch   = "behavior_dispatch"    // the run's items were handed off
	CheckpointRunAction  = "behavior_run_action"  // ReferenceID: run ID ":" action index
	CheckpointRunOutcome = "behavior_run_outcome" // ReferenceID: final run status
)

// Step failure codes of behavior steps, besides the run's own codes.
const (
	CodeWorkflowReference  = "workflow_reference_invalid"
	CodeRunMissing         = "run_missing"
	CodeRunFailed          = "run_failed"
	CodeRunStateConflict   = "run_state_conflict"
	CodeStorageUnavailable = "storage_unavailable"
	CodeAttemptsExhausted  = "attempts_exhausted"
)

// ErrWorkflowFence is returned when the workflow job claim no longer
// authorizes a durable run write or action (another claimant, lease end,
// cancellation or deadline). It is both ErrStaleClaim and
// workflow.ErrStaleClaim.
var ErrWorkflowFence = fmt.Errorf("%w: %w", ErrStaleClaim, workflow.ErrStaleClaim)

// RunFence is the workflow job claim a durable run write presents. At is
// the time of the write (the claim's lease must not have ended).
type RunFence struct {
	JobID       int64
	WorkspaceID string
	ExecutionID int64
	StepKey     string
	Attempt     int
	ClaimToken  string
	At          time.Time
}

// DurableRunStore is the run storage of durable behavior steps
// (storage.BehaviorStore). Every write first checks the fence, then that
// the run is the execution's legacy run in the same workspace and holds no
// legacy claim; a refused fence is ErrWorkflowFence and a run in another
// state is ErrConflict.
type DurableRunStore interface {
	GetRun(ctx context.Context, workspaceID string, runID int64) (Run, error)
	ListItems(ctx context.Context, workspaceID string, runID int64) ([]Item, error)
	// BeginDurableRunAttempt moves a pending or running run to running,
	// counts one more attempt and stores the attempt's deadline.
	BeginDurableRunAttempt(ctx context.Context, f RunFence, runID int64, deadline time.Time) (Run, error)
	// SetDurableRunContext stores the context snapshot together with the
	// step checkpoint that references it.
	SetDurableRunContext(ctx context.Context, f RunFence, runID int64, snapshot json.RawMessage, cp workflow.Checkpoint) error
	// AddDurableRunUsage adds one model call to the run's and the step's
	// cumulative usage.
	AddDurableRunUsage(ctx context.Context, f RunFence, runID int64, u Usage) error
	AddDurableRunItems(ctx context.Context, f RunFence, runID int64, items []NewItem) ([]Item, error)
	SetDurableRunActions(ctx context.Context, f RunFence, runID int64, result json.RawMessage) error
	// RetryDurableRun records the retry code; the run stays running.
	RetryDurableRun(ctx context.Context, f RunFence, runID int64, code string) error
	FinishDurableRun(ctx context.Context, f RunFence, runID int64, status RunStatus, code string) error
}

// ActionAuthorizer re-checks a workflow job claim immediately before an
// external action (storage.WorkflowStore).
type ActionAuthorizer interface {
	AuthorizeAction(ctx context.Context, jobID int64, workspaceID, claimToken string, at time.Time) error
}

// RunOperationKey is the logical operation identity of a behavior run: the
// execution trigger and operation key, inherited by retry executions.
func RunOperationKey(runID int64) string { return "behavior_run:" + strconv.FormatInt(runID, 10) }

// WorkflowStartRequest builds the execution start of a stored run: the
// run's workspace, pinned version and correlation, its operation identity
// and the run as legacy reference. def is NormalizeWorkflow of the pinned
// version.
func WorkflowStartRequest(run Run, def workflow.Definition) workflow.StartRequest {
	return workflow.StartRequest{
		WorkspaceID: run.WorkspaceID, VersionID: run.VersionID, TriggerKey: RunOperationKey(run.ID),
		CorrelationID: run.CorrelationID, Definition: def,
		References: workflow.References{
			SourceKind: WorkflowSourceRun, SourceID: strconv.FormatInt(run.ID, 10),
			Subject: run.Subject, Revision: run.Revision, OperationKey: RunOperationKey(run.ID), LegacyRunID: run.ID,
		},
	}
}

// --- action gate ---

type actionGateKey struct{}

// withActionGate makes every external write of the action library under ctx
// re-check gate first. A legacy run has no gate.
func withActionGate(ctx context.Context, gate func(context.Context) error) context.Context {
	return context.WithValue(ctx, actionGateKey{}, gate)
}

// authorizeAction runs ctx's action gate, if any. An error means the write
// must not start.
func authorizeAction(ctx context.Context) error {
	if gate, ok := ctx.Value(actionGateKey{}).(func(context.Context) error); ok && gate != nil {
		return gate(ctx)
	}
	return nil
}

// --- steps ---

// WorkflowSteps executes behavior runs as workflow steps.
type WorkflowSteps struct {
	w    *RunWorker
	runs DurableRunStore
	auth ActionAuthorizer
	now  func() time.Time
}

// NewWorkflowSteps builds the behavior step executors over the run
// executor's collaborators and settings. It starts no worker.
func NewWorkflowSteps(w *RunWorker, runs DurableRunStore, auth ActionAuthorizer) (*WorkflowSteps, error) {
	if w == nil || runs == nil || auth == nil {
		return nil, errors.New("behavior workflow steps: missing dependency")
	}
	return &WorkflowSteps{w: w, runs: runs, auth: auth, now: time.Now}, nil
}

type stepPhase int

const (
	phaseContext stepPhase = iota + 1
	phaseModel
	phaseDispatch
	phaseRunActions
)

// behaviorStepKinds maps each registered kind to the run phases it runs.
// The schema 1 built-in graph (NormalizeWorkflow) uses the generic kinds.
var behaviorStepKinds = []struct {
	kind   string
	phases []stepPhase
}{
	{workflow.StepKindBehaviorContext, []stepPhase{phaseContext}},
	{workflow.StepKindBehaviorModel, []stepPhase{phaseModel}},
	{workflow.StepKindBehaviorDispatch, []stepPhase{phaseDispatch}},
	{workflow.StepKindBehaviorRunActions, []stepPhase{phaseRunActions}},
	{workflow.StepKindCollectContext, []stepPhase{phaseContext}},
	{workflow.StepKindModel, []stepPhase{phaseModel}},
	{workflow.StepKindActions, []stepPhase{phaseDispatch, phaseRunActions}},
}

func isBehaviorStepKind(kind string) bool {
	for _, k := range behaviorStepKinds {
		if k.kind == kind {
			return true
		}
	}
	return false
}

func isContextStepKind(kind string) bool {
	return kind == workflow.StepKindBehaviorContext || kind == workflow.StepKindCollectContext
}

// Register adds the behavior step executors to reg.
func (s *WorkflowSteps) Register(reg *workflow.Registry) error {
	for _, k := range behaviorStepKinds {
		if err := reg.Register(k.kind, behaviorStep{s: s, phases: k.phases}); err != nil {
			return fmt.Errorf("register %s: %w", k.kind, err)
		}
	}
	return nil
}

type behaviorStep struct {
	s      *WorkflowSteps
	phases []stepPhase
}

func (b behaviorStep) Execute(ctx context.Context, sc workflow.StepContext) (workflow.StepResult, error) {
	return b.s.execute(ctx, sc, b.phases)
}

// durableState collects one step's checkpoint references.
type durableState struct {
	runID      int64
	prior      *workflow.Checkpoint // the step's checkpoint from an earlier attempt
	refs       []workflow.CheckpointReference
	seen       map[workflow.CheckpointReference]bool
	runActions json.RawMessage // the last stored run-level outcomes
}

func newDurableState(runID int64, prior *workflow.Checkpoint) *durableState {
	st := &durableState{runID: runID, prior: prior, seen: map[workflow.CheckpointReference]bool{}}
	st.add(CheckpointRun, strconv.FormatInt(runID, 10), RunOperationKey(runID))
	return st
}

func (st *durableState) add(kind, id, op string) {
	r := workflow.CheckpointReference{Kind: kind, ReferenceID: id, OperationKey: op}
	if !st.seen[r] {
		st.seen[r] = true
		st.refs = append(st.refs, r)
	}
}

func (st *durableState) runRef(kind string) { st.add(kind, strconv.FormatInt(st.runID, 10), "") }

// priorHas reports whether the step's earlier attempt checkpointed kind.
func (st *durableState) priorHas(kind string) bool {
	if st.prior == nil {
		return false
	}
	for _, r := range st.prior.References {
		if r.Kind == kind {
			return true
		}
	}
	return false
}

func (st *durableState) addItems(items []Item) {
	for _, it := range items {
		st.add(CheckpointRunItem, strconv.FormatInt(it.ID, 10), "")
	}
}

func (st *durableState) addRunActions(raw json.RawMessage) {
	for _, o := range DecodeRunActions(raw).Actions {
		st.add(CheckpointRunAction, fmt.Sprintf("%d:%d", st.runID, o.Index), fmt.Sprintf("behavior_run_action:%d:%d", st.runID, o.Index))
	}
}

func (st *durableState) checkpoint(reusable bool) *workflow.Checkpoint {
	return &workflow.Checkpoint{References: append([]workflow.CheckpointReference(nil), st.refs...), Reusable: reusable}
}

// durableWrites writes a run's attempt under the workflow job fence.
type durableWrites struct {
	s     *WorkflowSteps
	fence RunFence
	st    *durableState
}

func (d durableWrites) f() RunFence {
	f := d.fence
	f.At = d.s.now()
	return f
}

func (d durableWrites) setContext(ctx context.Context, snapshot json.RawMessage) error {
	d.st.runRef(CheckpointRunContext)
	return d.s.runs.SetDurableRunContext(ctx, d.f(), d.st.runID, snapshot, *d.st.checkpoint(false))
}

func (d durableWrites) addUsage(ctx context.Context, u Usage) error {
	return d.s.runs.AddDurableRunUsage(ctx, d.f(), d.st.runID, u)
}

func (d durableWrites) addItems(ctx context.Context, items []NewItem) ([]Item, error) {
	return d.s.runs.AddDurableRunItems(ctx, d.f(), d.st.runID, items)
}

func (d durableWrites) setRunActions(ctx context.Context, result json.RawMessage) error {
	if err := d.s.runs.SetDurableRunActions(ctx, d.f(), d.st.runID, result); err != nil {
		return err
	}
	d.st.runActions = append(json.RawMessage(nil), result...)
	return nil
}

// gate is the action gate of a step: the job claim must still authorize.
func (s *WorkflowSteps) gate(f RunFence) func(context.Context) error {
	return func(ctx context.Context) error {
		err := s.auth.AuthorizeAction(ctx, f.JobID, f.WorkspaceID, f.ClaimToken, s.now())
		if errors.Is(err, workflow.ErrStaleClaim) {
			return ErrWorkflowFence
		}
		return err
	}
}

func stepFail(code string) error {
	return &workflow.StepError{Code: workflow.ReasonCode(safeStepCode(code, CodeRunFailed))}
}

// safeStepCode keeps an engine code (which also fits the workflow reason
// code format) or gives fallback.
func safeStepCode(code, fallback string) string {
	if codeRe.MatchString(code) {
		return code
	}
	return fallback
}

// storageErr classifies a storage failure of a step. A refused fence or a
// cancellation writes nothing more; any other failure is retried like a
// legacy run whose lease ended: the existing receipts keep a retried write
// from repeating one that took effect.
func storageErr(ctx context.Context, err error) error {
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, ErrStaleClaim):
		return workflow.ErrStaleClaim
	case errors.Is(err, ErrConflict):
		return &workflow.StepError{Code: CodeRunStateConflict, Retryable: true}
	default:
		return &workflow.StepError{Code: CodeStorageUnavailable, Retryable: true}
	}
}

// finalStepKey is the behavior step that ends the run's projection: the
// last behavior step in the serial order the job queue runs a definition
// (the ready step with the lowest position first).
func finalStepKey(def workflow.Definition) string {
	done := make(map[string]bool, len(def.Steps))
	last := ""
	for len(done) < len(def.Steps) {
		picked := -1
		for i, step := range def.Steps {
			if done[step.Key] {
				continue
			}
			ready := true
			for _, dep := range step.DependsOn {
				ready = ready && done[dep]
			}
			if ready {
				picked = i
				break
			}
		}
		if picked < 0 {
			break
		}
		done[def.Steps[picked].Key] = true
		if isBehaviorStepKind(def.Steps[picked].Kind) {
			last = def.Steps[picked].Key
		}
	}
	return last
}

func hasContextStep(def workflow.Definition) bool {
	for _, step := range def.Steps {
		if isContextStepKind(step.Kind) {
			return true
		}
	}
	return false
}

// stepMargin is how long before the job's own step timeout a step ends its
// attempt, so the run's outcome is recorded before the worker times out.
func stepMargin(timeout time.Duration) time.Duration {
	return min(15*time.Second, timeout/4)
}

func (s *WorkflowSteps) execute(ctx context.Context, sc workflow.StepContext, phases []stepPhase) (workflow.StepResult, error) {
	ex := sc.Execution
	runID := ex.References.LegacyRunID
	if ex.References.SourceKind != WorkflowSourceRun || runID <= 0 || sc.JobID <= 0 || sc.Attempt < 1 || sc.ClaimToken == "" {
		return workflow.StepResult{}, stepFail(CodeWorkflowReference)
	}
	fence := RunFence{
		JobID: sc.JobID, WorkspaceID: ex.WorkspaceID, ExecutionID: ex.ID, StepKey: sc.Step.Key,
		Attempt: sc.Attempt, ClaimToken: sc.ClaimToken,
	}
	at := func() RunFence { f := fence; f.At = s.now(); return f }
	run, err := s.runs.GetRun(ctx, ex.WorkspaceID, runID)
	if errors.Is(err, ErrNotFound) {
		return workflow.StepResult{}, stepFail(CodeRunMissing)
	}
	if err != nil {
		return workflow.StepResult{}, storageErr(ctx, err)
	}
	if run.VersionID != ex.VersionID {
		return workflow.StepResult{}, stepFail(CodeWorkflowReference)
	}
	st := newDurableState(run.ID, sc.Checkpoint)

	// A run whose projection already ended: known completed work is not
	// repeated (a skipped run, or a step retried after the run's end was
	// recorded).
	switch run.Status {
	case RunDone, RunSkipped:
		st.add(CheckpointRunOutcome, string(run.Status), "")
		return workflow.StepResult{Checkpoint: st.checkpoint(true)}, nil
	case RunFailed:
		return workflow.StepResult{}, stepFail(run.ErrorCode)
	case RunCanceled:
		return workflow.StepResult{}, stepFail(string(workflow.ReasonCancelled))
	}

	// A step retry, a pending run, or an ended attempt deadline starts a new
	// attempt of the run; otherwise this step continues the current one, so
	// splitting an attempt into steps never adds attempts or time.
	now := s.now()
	if sc.Attempt > 1 || run.Status == RunPending || run.WorkflowAttemptDeadline == nil || !run.WorkflowAttemptDeadline.After(now) {
		if run.Attempts >= MaxRunAttempts {
			// Like a legacy run whose lease ended after its last attempt.
			if err := s.runs.FinishDurableRun(ctx, at(), run.ID, RunFailed, CodeAttemptsExhausted); err != nil {
				return workflow.StepResult{}, storageErr(ctx, err)
			}
			log.Printf("behavior run %d corr=%s: failed (%s)", run.ID, run.CorrelationID, CodeAttemptsExhausted)
			return workflow.StepResult{}, stepFail(CodeAttemptsExhausted)
		}
		run, err = s.runs.BeginDurableRunAttempt(ctx, at(), run.ID, now.Add(s.w.s.RunTimeout))
		if err != nil {
			return workflow.StepResult{}, storageErr(ctx, err)
		}
	}
	deadline := now.Add(s.w.s.RunTimeout)
	if run.WorkflowAttemptDeadline != nil {
		deadline = *run.WorkflowAttemptDeadline
	}
	if t := sc.Definition.StepTimeout; t > 0 {
		if d := now.Add(t - stepMargin(t)); d.Before(deadline) {
			deadline = d
		}
	}
	if ex.Deadline != nil {
		d := *ex.Deadline
		if rem := d.Sub(now); rem > 0 {
			d = d.Add(-stepMargin(rem))
		}
		if d.Before(deadline) {
			deadline = d
		}
	}
	actx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	actx = withActionGate(actx, s.gate(fence))

	rc := RunContext{Run: run}
	rc.writes = durableWrites{s: s, fence: fence, st: st}
	rc.lastAttempt = sc.Attempt >= sc.Definition.Retry.MaxAttempts
	log.Printf("behavior run %d corr=%s: attempt %d step %s", run.ID, run.CorrelationID, run.Attempts, sc.Step.Key)

	out, err := s.w.prepareRun(actx, &rc)
	if out == nil && err == nil {
		for _, ph := range phases {
			if out, err = s.runPhase(actx, &rc, sc, ph, st); out != nil || err != nil {
				break
			}
		}
	}
	if ctx.Err() != nil {
		return workflow.StepResult{}, ctx.Err()
	}
	if err != nil {
		if errors.Is(err, ErrStaleClaim) || actx.Err() == nil {
			return workflow.StepResult{}, storageErr(ctx, err)
		}
		out = runRetry(CodeRunTimeout)
	}
	if out == nil {
		if sc.Step.Key != finalStepKey(sc.Definition) {
			return workflow.StepResult{Checkpoint: st.checkpoint(true)}, nil
		}
		out = &runOutcome{status: RunDone, code: verdictEndCode(rc)}
	}
	return s.end(ctx, &rc, at, out, st)
}

// verdictEndCode is the code of a verdict run that ended without a verdict
// (the failure the model step recorded), "" otherwise.
func verdictEndCode(rc RunContext) string {
	if rc.Doc.Verdict == nil {
		return ""
	}
	v := DecodeRunActions(rc.Run.RunActions).Verdict
	if v == nil || v.Available() || v.Code == CodeVerdictUnavailable || !codeRe.MatchString(v.Code) {
		return ""
	}
	return v.Code
}

// end records the attempt's outcome on the run projection and maps it to
// the step result: a retry while attempts remain, a finished run, or a
// failed run (told to the owner once, like a legacy run).
func (s *WorkflowSteps) end(ctx context.Context, rc *RunContext, at func() RunFence, out *runOutcome, st *durableState) (workflow.StepResult, error) {
	run := rc.Run
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runActionSaveTimeout)
	defer cancel()
	if out.retry {
		if !rc.finalAttempt() {
			code := safeStepCode(out.code, string(workflow.ReasonStepRetry))
			if err := s.runs.RetryDurableRun(wctx, at(), run.ID, code); err != nil {
				return workflow.StepResult{}, storageErr(ctx, err)
			}
			log.Printf("behavior run %d corr=%s: retry (%s)", run.ID, run.CorrelationID, code)
			return workflow.StepResult{Checkpoint: st.checkpoint(false)}, &workflow.StepError{Code: workflow.ReasonCode(code), Retryable: true}
		}
		out = runFail(out.code)
	}
	if err := s.runs.FinishDurableRun(wctx, at(), run.ID, out.status, out.code); err != nil {
		return workflow.StepResult{}, storageErr(ctx, err)
	}
	if out.code != "" {
		log.Printf("behavior run %d corr=%s: %s (%s)", run.ID, run.CorrelationID, out.status, out.code)
	} else {
		log.Printf("behavior run %d corr=%s: %s", run.ID, run.CorrelationID, out.status)
	}
	st.add(CheckpointRunOutcome, string(out.status), "")
	if out.status != RunFailed {
		return workflow.StepResult{Checkpoint: st.checkpoint(true)}, nil
	}
	nctx := withActionGate(ctx, s.gate(at()))
	if err := s.w.d.Notifier.RunFailed(nctx, *rc, out.code); err != nil && ctx.Err() == nil {
		log.Printf("behavior run %d corr=%s: failure notice not sent", run.ID, run.CorrelationID)
	}
	return workflow.StepResult{Checkpoint: st.checkpoint(false)}, &workflow.StepError{
		Code:      workflow.ReasonCode(safeStepCode(out.code, CodeRunFailed)),
		Uncertain: hasUnknownRunAction(st.runActions, rc.Run.RunActions),
	}
}

// hasUnknownRunAction reports whether a run-level action that fails the run
// has an unknown outcome: its effect cannot be known, so the execution
// fails as side-effect uncertain instead of being retried.
func hasUnknownRunAction(latest, stored json.RawMessage) bool {
	raw := latest
	if len(raw) == 0 {
		raw = stored
	}
	for _, o := range DecodeRunActions(raw).Actions {
		if o.Status == OutcomeUnknown && o.failedRun() {
			return true
		}
	}
	return false
}

func (s *WorkflowSteps) runPhase(ctx context.Context, rc *RunContext, sc workflow.StepContext, ph stepPhase, st *durableState) (*runOutcome, error) {
	switch ph {
	case phaseContext:
		return s.contextPhase(ctx, rc, st)
	case phaseModel:
		return s.modelPhase(ctx, rc, sc, st)
	case phaseDispatch:
		return s.dispatchPhase(ctx, rc, st)
	case phaseRunActions:
		return s.runActionsPhase(ctx, rc, st)
	}
	return runFail(CodeWorkflowReference), nil
}

// modelWorkDone reports whether the run's model work already ended: items
// stored, or a verdict run's model step ended (see modelStepEnded). The
// references are added to the checkpoint.
func (s *WorkflowSteps) modelWorkDone(ctx context.Context, rc *RunContext, st *durableState) (bool, error) {
	items, err := s.runs.ListItems(ctx, rc.Run.WorkspaceID, rc.Run.ID)
	if err != nil {
		return false, err
	}
	if len(items) > 0 {
		st.addItems(items)
		if rc.Doc.Verdict != nil {
			st.runRef(CheckpointRunVerdict)
		}
		return true, nil
	}
	if modelStepEnded(rc) {
		st.runRef(CheckpointRunVerdict)
		return true, nil
	}
	return false, nil
}

// contextPhase collects the run's context once (behavior.context).
func (s *WorkflowSteps) contextPhase(ctx context.Context, rc *RunContext, st *durableState) (*runOutcome, error) {
	if !rc.Doc.HasModelStep() {
		return nil, nil
	}
	if st.priorHas(CheckpointRunContext) && len(rc.Run.Context) > 0 {
		st.runRef(CheckpointRunContext)
		return nil, nil
	}
	if done, err := s.modelWorkDone(ctx, rc, st); done || err != nil {
		return nil, err
	}
	return s.collect(ctx, rc, st)
}

func (s *WorkflowSteps) collect(ctx context.Context, rc *RunContext, st *durableState) (*runOutcome, error) {
	out, viaNoVerdict, err := s.w.collectContext(ctx, rc)
	if err != nil {
		return out, err
	}
	if out != nil {
		if viaNoVerdict {
			return s.noVerdict(ctx, rc, out, st)
		}
		return out, nil
	}
	return nil, nil
}

// noVerdict is the run executor's noVerdict without the inline hand-off:
// the unavailable verdict is stored and the later steps hand off and run
// the actions (gated actions are skipped).
func (s *WorkflowSteps) noVerdict(ctx context.Context, rc *RunContext, out *runOutcome, st *durableState) (*runOutcome, error) {
	if rc.Doc.Verdict == nil || out.status == RunSkipped {
		return out, nil
	}
	if out.retry && !rc.finalAttempt() {
		return out, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	log.Printf("behavior run %d corr=%s: no verdict (%s); gated run actions are skipped", rc.Run.ID, rc.Run.CorrelationID, out.code)
	if err := s.w.saveVerdict(ctx, rc, &RunVerdict{Code: out.code}); err != nil {
		return nil, err
	}
	st.runRef(CheckpointRunVerdict)
	return nil, nil
}

// modelPhase asks the model once per run (behavior.model). Without a
// context step in the definition it collects the context first.
func (s *WorkflowSteps) modelPhase(ctx context.Context, rc *RunContext, sc workflow.StepContext, st *durableState) (*runOutcome, error) {
	if !rc.Doc.HasModelStep() {
		return nil, nil
	}
	if done, err := s.modelWorkDone(ctx, rc, st); done || err != nil {
		return nil, err
	}
	collected := len(rc.Run.Context) > 0 && (hasContextStep(sc.Definition) || st.priorHas(CheckpointRunContext))
	if collected {
		st.runRef(CheckpointRunContext)
	} else {
		if out, err := s.collect(ctx, rc, st); out != nil || err != nil {
			return out, err
		}
		if modelStepEnded(rc) {
			return nil, nil
		}
	}
	stored, out, err := s.w.modelPhase(ctx, rc)
	if err != nil {
		return nil, err
	}
	if out != nil {
		return s.noVerdict(ctx, rc, out, st)
	}
	st.addItems(stored)
	if rc.Doc.Verdict != nil {
		st.runRef(CheckpointRunVerdict)
	}
	return nil, nil
}

// dispatchPhase hands the run's stored items to the Dispatcher
// (behavior.dispatch); it is idempotent per run, item and action.
func (s *WorkflowSteps) dispatchPhase(ctx context.Context, rc *RunContext, st *durableState) (*runOutcome, error) {
	if !rc.Doc.HasModelStep() {
		return nil, nil
	}
	items, err := s.runs.ListItems(ctx, rc.Run.WorkspaceID, rc.Run.ID)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		items = nil
	}
	if err := s.w.d.Dispatcher.Dispatch(ctx, *rc, items); err != nil {
		if errors.Is(err, ErrStaleClaim) {
			return nil, err
		}
		return runRetry(CodeDispatchFailed), nil
	}
	st.addItems(items)
	st.runRef(CheckpointDispatch)
	return nil, nil
}

// runActionsPhase runs the run-level actions (behavior.run_actions); stored
// final outcomes are never run again. A run that ends well is finished by
// the definition's last behavior step.
func (s *WorkflowSteps) runActionsPhase(ctx context.Context, rc *RunContext, st *durableState) (*runOutcome, error) {
	out, err := s.w.runActions(ctx, *rc)
	if err != nil {
		return nil, err
	}
	raw := st.runActions
	if len(raw) == 0 {
		raw = rc.Run.RunActions
	}
	st.addRunActions(raw)
	if out != nil && out.status == RunDone && !out.retry && out.code == "" {
		return nil, nil
	}
	return out, nil
}
