package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/memory"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// This file executes the Phase 2 comment pipeline as a durable workflow
// execution of an immutable internal definition. The agent job stays the
// logical batch of one thread: its identity, cumulative usage, attempt
// count, through-message cursor and report receipt. The workflow job claim
// is the only authority to write it while the execution owns it.
//
// The pipeline is split at the persisted triage, investigation and report
// boundaries (agent.triage, agent.investigate, agent.report). Each step
// stores its validated structured result in agent_batch_results and its
// checkpoint references that row; no model transcript enters workflow
// JSON. Routing, tool policy, batching, owner-comment semantics, budgets,
// fallback notices and report texts are those of the Orchestrator.
//
// Every operation key stays bound to the agent job ID: a step retry or a
// retry execution reuses the tool-run keys and the outbox dedupe key
// "agent_job:<id>", and a batch whose report was committed is never
// reported again (its job status is the receipt).

// WorkflowSourceAgentJob is the execution source kind of an agent batch.
const WorkflowSourceAgentJob = "agent_job"

// InternalPipelineKind is the internal definition kind of the pipeline.
const InternalPipelineKind = "agent_comment_pipeline"

// Pipeline step keys of the internal definition.
const (
	StepKeyTriage      = "triage"
	StepKeyInvestigate = "investigate"
	StepKeyReport      = "report"
)

// AgentAttemptTimeout bounds one attempt of a workflow-owned batch across
// its steps; it equals the legacy lease of one attempt.
const AgentAttemptTimeout = 30 * time.Minute

// Checkpoint reference kinds of pipeline steps.
const (
	CheckpointAgentJob      = "agent_job"                 // ReferenceID: agent job ID
	CheckpointTriage        = "agent_batch_triage"        // ReferenceID: agent_batch_results ID
	CheckpointInvestigation = "agent_batch_investigation" // ReferenceID: result ID, or "skipped"
	CheckpointReport        = "agent_batch_report"        // ReferenceID: final job status
)

// Step failure codes of pipeline steps, besides the TriageError codes.
const (
	CodeWorkflowReference  = "workflow_reference_invalid"
	CodeBatchMissing       = "batch_missing"
	CodeBatchConflict      = "batch_state_conflict"
	CodeCursorMoved        = "cursor_moved"
	CodeStorageUnavailable = "storage_unavailable"
	CodeAttemptsExhausted  = "attempts_exhausted"
	CodeAgentDisabled      = "agent_disabled"
)

// Agent job statuses.
const (
	BatchPending    = "pending"
	BatchProcessing = "processing"
	BatchDone       = "done"
	BatchFailed     = "failed"
)

// Result stages.
const (
	StageTriage        = "triage"
	StageInvestigation = "investigation"
)

var (
	// ErrWorkflowFence means the workflow job claim no longer authorizes a
	// durable batch write (another claimant, lease end, cancellation or
	// deadline). It is both ErrStaleAgentClaim and workflow.ErrStaleClaim.
	ErrWorkflowFence = fmt.Errorf("%w: %w", ErrStaleAgentClaim, workflow.ErrStaleClaim)
	// ErrBatchNotFound means the batch (or the pinned version) is unknown
	// in the workspace.
	ErrBatchNotFound = errors.New("agent batch not found")
	// ErrBatchConflict means the batch is not in the state a write needs.
	ErrBatchConflict = errors.New("agent batch state conflict")
)

// AgentOperationKey is the logical operation identity of one agent batch:
// the execution trigger and operation key (inherited by retry executions)
// and the dedupe key of its Telegram report.
func AgentOperationKey(jobID int64) string { return "agent_job:" + strconv.FormatInt(jobID, 10) }

// --- definition and settings snapshot ---

// PipelineDefinition is the immutable orchestration of the internal
// pipeline. Its step budget keeps the legacy execution-wide budget: five
// attempts in total and the legacy backoff (the batch's own attempt count
// enforces the total across steps), and one attempt is bounded by
// AgentAttemptTimeout across all of its steps.
func PipelineDefinition() workflow.Definition {
	delays := append([]time.Duration(nil), agentBackoff...)
	return workflow.Definition{
		Steps: []workflow.StepDefinition{
			{Key: StepKeyTriage, Kind: workflow.StepKindAgentTriage},
			{Key: StepKeyInvestigate, Kind: workflow.StepKindAgentInvestigate, DependsOn: []string{StepKeyTriage}},
			{Key: StepKeyReport, Kind: workflow.StepKindAgentReport, DependsOn: []string{StepKeyInvestigate}},
		},
		Retry:         workflow.RetryPolicy{MaxAttempts: MaxAgentAttempts, Delays: delays},
		StepTimeout:   AgentAttemptTimeout,
		LeaseDuration: AgentAttemptTimeout + time.Minute,
	}
}

// PipelineSettings are the effective non-secret process settings of the
// pipeline at enqueue (from the DB settings).
type PipelineSettings struct {
	Limits                          tools.Limits
	AllowDangerous                  bool
	MaxInputTokens, MaxOutputTokens int64
	DebounceWindow, DebounceMaxWait time.Duration
}

// SnapshotLimits are the tool limits of a snapshot.
type SnapshotLimits struct {
	MaxCalls         int   `json:"max_calls"`
	MaxDangerous     int   `json:"max_dangerous"`
	ReadTimeoutMS    int64 `json:"read_timeout_ms"`
	NetworkTimeoutMS int64 `json:"network_timeout_ms"`
	OutputCap        int   `json:"output_cap"`
}

// PipelineSnapshot is the document of one internal version: the non-secret
// settings in force when a batch was enqueued. Credentials appear only as
// reference names and the persona only as its SHA-256. A version can only
// narrow what the current configuration allows (Apply); it never widens a
// permission or survives a revocation.
type PipelineSnapshot struct {
	Schema              int                 `json:"schema"`
	Kind                string              `json:"kind"`
	ConfigValid         bool                `json:"config_valid"`
	Enabled             bool                `json:"enabled"`
	ConfidenceThreshold float64             `json:"confidence_threshold"`
	PersonaSHA256       string              `json:"persona_sha256"`
	ClickUpTokenRef     string              `json:"clickup_token_ref"`
	Tools               []string            `json:"tools"`
	DryRun              bool                `json:"dry_run"`
	MaxWritesPerJob     int                 `json:"max_writes_per_job"`
	RepoAllow           map[string][]string `json:"repo_allow"`
	AllowDangerous      bool                `json:"allow_dangerous"`
	MaxInputTokens      int64               `json:"max_input_tokens"`
	MaxOutputTokens     int64               `json:"max_output_tokens"`
	Limits              SnapshotLimits      `json:"limits"`
	DebounceWindowMS    int64               `json:"debounce_window_ms"`
	DebounceMaxWaitMS   int64               `json:"debounce_max_wait_ms"`
	Workflow            workflow.Definition `json:"workflow"`
}

// BuildPipelineSnapshot snapshots cfg (valid reports whether the source's
// agent config parsed) and the process settings.
func BuildPipelineSnapshot(cfg tenant.AgentConfig, valid bool, ps PipelineSettings) PipelineSnapshot {
	snap := PipelineSnapshot{
		Schema: 1, Kind: InternalPipelineKind, ConfigValid: valid, Enabled: valid && cfg.Enabled,
		AllowDangerous: ps.AllowDangerous, MaxInputTokens: ps.MaxInputTokens, MaxOutputTokens: ps.MaxOutputTokens,
		Limits: SnapshotLimits{
			MaxCalls: ps.Limits.MaxCalls, MaxDangerous: ps.Limits.MaxDangerous,
			ReadTimeoutMS: ps.Limits.ReadTimeout.Milliseconds(), NetworkTimeoutMS: ps.Limits.NetworkTimeout.Milliseconds(),
			OutputCap: ps.Limits.OutputCap,
		},
		DebounceWindowMS: ps.DebounceWindow.Milliseconds(), DebounceMaxWaitMS: ps.DebounceMaxWait.Milliseconds(),
		Workflow: PipelineDefinition(), Tools: []string{}, RepoAllow: map[string][]string{},
	}
	if !valid {
		return snap
	}
	snap.ConfidenceThreshold = cfg.ConfidenceThreshold
	if cfg.Persona != "" {
		sum := sha256.Sum256([]byte(cfg.Persona))
		snap.PersonaSHA256 = hex.EncodeToString(sum[:])
	}
	snap.ClickUpTokenRef = cfg.ClickUpTokenRef
	snap.Tools = sortedCopy(cfg.Tools.Enabled)
	snap.DryRun = cfg.Tools.DryRun
	snap.MaxWritesPerJob = cfg.Tools.MaxWritesPerJob
	for repo, grant := range cfg.Tools.Repos {
		snap.RepoAllow[repo] = sortedCopy(grant.Allow)
	}
	return snap
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	slices.Sort(out)
	return slices.Compact(out)
}

// Apply narrows the current configuration cur by the snapshot: the agent
// must be enabled in both, only tools and repo grants present in both are
// allowed, dry run in either applies, the smaller write budget applies, and
// the pinned routing threshold is kept. Repository, account and credential
// details always come from cur, so a revocation applies at once.
func (p PipelineSnapshot) Apply(cur tenant.AgentConfig) tenant.AgentConfig {
	out := cur
	out.Enabled = cur.Enabled && p.Enabled
	out.Tools.Enabled = intersect(cur.Tools.Enabled, p.Tools)
	out.Tools.DryRun = cur.Tools.DryRun || p.DryRun
	out.Tools.MaxWritesPerJob = min(cur.Tools.MaxWritesPerJob, p.MaxWritesPerJob)
	repos := make(map[string]tenant.RepoGrant, len(cur.Tools.Repos))
	for name, grant := range cur.Tools.Repos {
		allow, ok := p.RepoAllow[name]
		if !ok {
			continue
		}
		grant.Allow = intersect(grant.Allow, allow)
		repos[name] = grant
	}
	out.Tools.Repos = repos
	if p.ConfidenceThreshold > 0 {
		out.ConfidenceThreshold = p.ConfidenceThreshold
	}
	return out
}

// settings narrows the current process settings cur by the snapshot.
func (p PipelineSnapshot) settings(cur OrchestratorSettings) OrchestratorSettings {
	out := cur
	out.AllowDangerous = cur.AllowDangerous && p.AllowDangerous
	out.MaxInputTokens = minBudget(cur.MaxInputTokens, p.MaxInputTokens)
	out.MaxOutputTokens = minBudget(cur.MaxOutputTokens, p.MaxOutputTokens)
	l := cur.Limits
	l.MaxCalls = int(minBudget(int64(l.MaxCalls), int64(p.Limits.MaxCalls)))
	l.MaxDangerous = min(l.MaxDangerous, p.Limits.MaxDangerous)
	l.ReadTimeout = time.Duration(minBudget(int64(l.ReadTimeout), p.Limits.ReadTimeoutMS*int64(time.Millisecond)))
	l.NetworkTimeout = time.Duration(minBudget(int64(l.NetworkTimeout), p.Limits.NetworkTimeoutMS*int64(time.Millisecond)))
	l.OutputCap = int(minBudget(int64(l.OutputCap), int64(p.Limits.OutputCap)))
	out.Limits = l
	return out
}

// minBudget is the smaller positive limit; zero means unlimited.
func minBudget(a, b int64) int64 {
	switch {
	case a <= 0:
		return max(b, 0)
	case b <= 0:
		return a
	default:
		return min(a, b)
	}
}

func intersect(cur, allowed []string) []string {
	out := make([]string, 0, len(cur))
	for _, name := range cur {
		if slices.Contains(allowed, name) {
			out = append(out, name)
		}
	}
	return out
}

// --- durable batch storage ---

// AgentBatch is a stored agent job.
type AgentBatch struct {
	ID                                         int64
	WorkspaceID, ThreadID, SourceIntegrationID string
	Status                                     string
	Attempts                                   int
	Usage                                      domain.Usage
	// ExecutionID is the owning workflow execution; 0 for a legacy job.
	ExecutionID      int64
	AttemptDeadline  *time.Time
	ThroughMessageID int64
	ErrorCode        string
}

func (b AgentBatch) finished() bool { return b.Status == BatchDone || b.Status == BatchFailed }

func (b AgentBatch) job() AgentJob {
	return AgentJob{ID: b.ID, WorkspaceID: b.WorkspaceID, ThreadID: b.ThreadID, SourceIntegrationID: b.SourceIntegrationID,
		Attempts: b.Attempts, Usage: b.Usage}
}

// BatchResult is the validated structured result of one pipeline stage.
// Texts are already cleaned for storage and output; no transcript or tool
// output is kept.
type BatchResult struct {
	ID    int64
	Stage string
	// Triage stage.
	Outcome          Outcome
	ThroughMessageID int64
	LastTriagedID    int64
	MaxMessageID     int64
	Decision         TriageDecision
	ToolsAvailable   bool
	RouterNote       string
	Unseen           int
	// Investigation stage.
	ReportText        string
	ToolsLine         string
	TotalInputTokens  int64
	TotalOutputTokens int64
}

// AgentFence is the workflow job claim a durable batch write presents. At
// is the time of the write (the claim's lease must not have ended).
type AgentFence struct {
	JobID       int64
	WorkspaceID string
	ExecutionID int64
	StepKey     string
	Attempt     int
	ClaimToken  string
	At          time.Time
}

// DurableAgentStore is the batch storage of pipeline steps
// (storage.AgentWorkflowStore). Every write first checks the fence, then
// that the batch belongs to the execution in the same workspace, is
// workflow-owned and still processing; a refused fence is ErrWorkflowFence
// and a batch in another state is ErrBatchConflict. Checkpoints passed to
// a write are stored in the same transaction.
type DurableAgentStore interface {
	GetBatch(ctx context.Context, workspaceID string, jobID int64) (AgentBatch, error)
	// LoadBatch reads the batch's context like AgentStore.Load (current
	// source configuration included).
	LoadBatch(ctx context.Context, b AgentBatch) (JobContext, error)
	// PipelineSnapshot reads the document of an internal version.
	PipelineSnapshot(ctx context.Context, workspaceID string, versionID int64) (PipelineSnapshot, error)
	BatchResults(ctx context.Context, workspaceID string, jobID int64) ([]BatchResult, error)
	// BeginBatchAttempt counts one more attempt and stores its deadline.
	BeginBatchAttempt(ctx context.Context, f AgentFence, jobID int64, deadline time.Time) (AgentBatch, error)
	// AddBatchUsage adds one model call to the batch's and the step's
	// cumulative usage.
	AddBatchUsage(ctx context.Context, f AgentFence, jobID int64, u domain.Usage) error
	// SaveBatchResult stores the stage result once (an existing one is
	// kept and its ID returned) with the checkpoint, which is completed
	// with the result's reference.
	SaveBatchResult(ctx context.Context, f AgentFence, jobID int64, r BatchResult, cp workflow.Checkpoint) (int64, error)
	// RetryBatch records the retry code and ends the attempt's deadline.
	RetryBatch(ctx context.Context, f AgentFence, jobID int64, code string) error
	// CommitBatch is AgentStore.CommitOutcome under the fence: notes,
	// outbox report, done status and cursor, thread refresh and follow-up
	// job, with the checkpoint, in one transaction.
	CommitBatch(ctx context.Context, f AgentFence, jobID int64, r JobResult, cp workflow.Checkpoint) error
	// CommitBatchFallback is AgentStore.CommitFallback under the fence.
	CommitBatchFallback(ctx context.Context, f AgentFence, jobID int64, code, text string, through int64, cp workflow.Checkpoint) error
}

// ActionAuthorizer re-checks a workflow job claim immediately before an
// external tool call (storage.WorkflowStore).
type ActionAuthorizer interface {
	AuthorizeAction(ctx context.Context, jobID int64, workspaceID, claimToken string, at time.Time) error
}

// WorkflowStartRequest builds the execution start of an adopted batch:
// trigger and operation key "agent_job:<id>", the thread as subject and
// the job as legacy reference. versionID is the internal version holding
// the batch's settings snapshot.
func WorkflowStartRequest(b AgentBatch, versionID int64, correlationID string) workflow.StartRequest {
	return workflow.StartRequest{
		WorkspaceID: b.WorkspaceID, VersionID: versionID, TriggerKey: AgentOperationKey(b.ID),
		CorrelationID: correlationID, Definition: PipelineDefinition(),
		References: workflow.References{
			SourceKind: WorkflowSourceAgentJob, SourceID: strconv.FormatInt(b.ID, 10), Subject: b.ThreadID,
			OperationKey: AgentOperationKey(b.ID), LegacyJobID: b.ID,
		},
	}
}

// --- steps ---

// WorkflowSteps executes agent batches as workflow steps. It reuses the
// Orchestrator's collaborators (models, tool registry, tool-run audit,
// redactor, compressor) and settings. It starts no worker.
type WorkflowSteps struct {
	o     *Orchestrator
	store DurableAgentStore
	auth  ActionAuthorizer
	now   func() time.Time
}

// NewWorkflowSteps builds the pipeline step executors.
func NewWorkflowSteps(o *Orchestrator, store DurableAgentStore, auth ActionAuthorizer) (*WorkflowSteps, error) {
	if o == nil || store == nil || auth == nil {
		return nil, errors.New("agent workflow steps: missing dependency")
	}
	return &WorkflowSteps{o: o, store: store, auth: auth, now: time.Now}, nil
}

// Register adds agent.triage, agent.investigate and agent.report to reg.
func (s *WorkflowSteps) Register(reg *workflow.Registry) error {
	for kind, stage := range map[string]string{
		workflow.StepKindAgentTriage:      StepKeyTriage,
		workflow.StepKindAgentInvestigate: StepKeyInvestigate,
		workflow.StepKindAgentReport:      StepKeyReport,
	} {
		if err := reg.Register(kind, pipelineStep{s: s, stage: stage}); err != nil {
			return fmt.Errorf("register %s: %w", kind, err)
		}
	}
	return nil
}

type pipelineStep struct {
	s     *WorkflowSteps
	stage string
}

func (p pipelineStep) Execute(ctx context.Context, sc workflow.StepContext) (workflow.StepResult, error) {
	return p.s.execute(ctx, sc, p.stage)
}

// batchState collects one step's checkpoint references.
type batchState struct {
	refs []workflow.CheckpointReference
	seen map[workflow.CheckpointReference]bool
}

func newBatchState(jobID int64) *batchState {
	st := &batchState{seen: map[workflow.CheckpointReference]bool{}}
	st.add(CheckpointAgentJob, strconv.FormatInt(jobID, 10), AgentOperationKey(jobID))
	return st
}

func (st *batchState) add(kind, id, op string) {
	r := workflow.CheckpointReference{Kind: kind, ReferenceID: id, OperationKey: op}
	if !st.seen[r] {
		st.seen[r] = true
		st.refs = append(st.refs, r)
	}
}

func (st *batchState) checkpoint(reusable bool) workflow.Checkpoint {
	return workflow.Checkpoint{References: append([]workflow.CheckpointReference(nil), st.refs...), Reusable: reusable}
}

func (st *batchState) result() (workflow.StepResult, error) {
	cp := st.checkpoint(true)
	return workflow.StepResult{Checkpoint: &cp}, nil
}

func stepFail(code string) error { return &workflow.StepError{Code: workflow.ReasonCode(code)} }

// storageErr classifies a storage failure of a step. A refused fence or a
// cancellation writes nothing more; any other failure is retried like a
// legacy job whose lease ended: stored results, tool receipts and the
// outbox dedupe key keep a retry from repeating work that took effect.
func storageErr(ctx context.Context, err error) error {
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, ErrStaleAgentClaim) || errors.Is(err, workflow.ErrStaleClaim):
		return workflow.ErrStaleClaim
	case errors.Is(err, ErrBatchConflict):
		return &workflow.StepError{Code: CodeBatchConflict, Retryable: true}
	default:
		return &workflow.StepError{Code: CodeStorageUnavailable, Retryable: true}
	}
}

// stepMargin is how long before the job's step timeout a step ends its
// attempt, so its outcome is recorded before the worker times out.
func stepMargin(timeout time.Duration) time.Duration { return min(15*time.Second, timeout/4) }

// writeTimeout bounds a result or commit write after the model work.
const writeTimeout = 30 * time.Second

// batchRun is one step attempt of one batch.
type batchRun struct {
	s        *WorkflowSteps
	fence    AgentFence
	batch    AgentBatch
	snap     PipelineSnapshot
	settings OrchestratorSettings
	tri, inv *BatchResult
	st       *batchState
	stepCtx  context.Context // the worker's context (not the attempt deadline)
}

func (r *batchRun) f() AgentFence {
	f := r.fence
	f.At = r.s.now()
	return f
}

func (r *batchRun) writeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.stepCtx), writeTimeout)
}

func (s *WorkflowSteps) execute(ctx context.Context, sc workflow.StepContext, stage string) (workflow.StepResult, error) {
	ex := sc.Execution
	jobID := ex.References.LegacyJobID
	if ex.References.SourceKind != WorkflowSourceAgentJob || jobID <= 0 || sc.JobID <= 0 || sc.Attempt < 1 || sc.ClaimToken == "" {
		return workflow.StepResult{}, stepFail(CodeWorkflowReference)
	}
	r := &batchRun{s: s, st: newBatchState(jobID), stepCtx: ctx, fence: AgentFence{
		JobID: sc.JobID, WorkspaceID: ex.WorkspaceID, ExecutionID: ex.ID, StepKey: sc.Step.Key,
		Attempt: sc.Attempt, ClaimToken: sc.ClaimToken,
	}}
	batch, err := s.store.GetBatch(ctx, ex.WorkspaceID, jobID)
	if errors.Is(err, ErrBatchNotFound) {
		return workflow.StepResult{}, stepFail(CodeBatchMissing)
	}
	if err != nil {
		return workflow.StepResult{}, storageErr(ctx, err)
	}
	if batch.ExecutionID == 0 || batch.WorkspaceID != ex.WorkspaceID {
		return workflow.StepResult{}, stepFail(CodeWorkflowReference)
	}
	// The batch's end is its receipt: a committed report or fallback is
	// never committed again (a restart after the commit, or a step that
	// follows a fallback).
	if batch.finished() {
		r.st.add(CheckpointReport, batch.Status, AgentOperationKey(jobID))
		return r.st.result()
	}
	if batch.Status != BatchProcessing {
		return workflow.StepResult{}, stepFail(CodeWorkflowReference)
	}
	snap, err := s.store.PipelineSnapshot(ctx, ex.WorkspaceID, ex.VersionID)
	if errors.Is(err, ErrBatchNotFound) {
		return workflow.StepResult{}, stepFail(CodeWorkflowReference)
	}
	if err != nil {
		return workflow.StepResult{}, storageErr(ctx, err)
	}
	r.snap, r.settings = snap, snap.settings(s.o.settings)
	results, err := s.store.BatchResults(ctx, ex.WorkspaceID, jobID)
	if err != nil {
		return workflow.StepResult{}, storageErr(ctx, err)
	}
	for i := range results {
		switch results[i].Stage {
		case StageTriage:
			r.tri = &results[i]
		case StageInvestigation:
			r.inv = &results[i]
		}
	}
	if r.tri != nil {
		r.st.add(CheckpointTriage, strconv.FormatInt(r.tri.ID, 10), "")
	}

	// A step retry, the batch's first step or an ended attempt deadline
	// starts a new attempt of the batch; otherwise this step continues the
	// current one, so splitting an attempt into steps never adds attempts,
	// time or budget.
	now := s.now()
	if sc.Attempt > 1 || batch.AttemptDeadline == nil || !batch.AttemptDeadline.After(now) {
		if batch.Attempts >= MaxAgentAttempts {
			r.batch = batch
			return r.fallbackFresh(ctx, CodeAttemptsExhausted)
		}
		f := r.fence
		f.At = now
		batch, err = s.store.BeginBatchAttempt(ctx, f, jobID, now.Add(AgentAttemptTimeout))
		if err != nil {
			return workflow.StepResult{}, storageErr(ctx, err)
		}
	}
	r.batch = batch
	deadline := now.Add(AgentAttemptTimeout)
	if batch.AttemptDeadline != nil {
		deadline = *batch.AttemptDeadline
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
	switch stage {
	case StepKeyTriage:
		return r.triage(actx)
	case StepKeyInvestigate:
		return r.investigate(actx)
	case StepKeyReport:
		return r.report(actx)
	}
	return workflow.StepResult{}, stepFail(CodeWorkflowReference)
}

// load reads the batch's context with the snapshot applied to its config.
// After triage the history is cut to what triage saw, so later steps work
// on the same batch and the through-message cursor cannot move.
func (r *batchRun) load(ctx context.Context) (JobContext, error) {
	jc, err := r.s.store.LoadBatch(ctx, r.batch)
	if err != nil {
		return JobContext{}, err
	}
	jc.Config = r.snap.Apply(jc.Config)
	if r.tri == nil {
		return jc, nil
	}
	if jc.LastTriagedID != r.tri.LastTriagedID {
		return JobContext{}, errCursorMoved
	}
	kept := jc.History[:0]
	for _, m := range jc.History {
		if m.ID <= r.tri.MaxMessageID {
			kept = append(kept, m)
		}
	}
	jc.History = kept
	return jc, nil
}

var errCursorMoved = errors.New("agent batch cursor moved")

// loadErr maps a load failure to the step result.
func (r *batchRun) loadErr(ctx context.Context, err error) (workflow.StepResult, error) {
	if errors.Is(err, errCursorMoved) {
		log.Printf("agent batch %d: thread cursor moved under the batch; not reported", r.batch.ID)
		return workflow.StepResult{}, stepFail(CodeCursorMoved)
	}
	return workflow.StepResult{}, storageErr(ctx, err)
}

func (r *batchRun) packet(jc JobContext) memory.ContextPacket {
	return memory.BuildPacket(jc.Title, jc.TaskURL, jc.Summary, jc.History, jc.LastTriagedID, r.s.o.cleanInput)
}

func (r *batchRun) through(jc JobContext) int64 {
	if r.tri != nil {
		return r.tri.ThroughMessageID
	}
	return batchThrough(jc)
}

// gate is the tool gate of a step: the job claim must still authorize.
func (r *batchRun) gate(ctx context.Context) error {
	err := r.s.auth.AuthorizeAction(ctx, r.fence.JobID, r.fence.WorkspaceID, r.fence.ClaimToken, r.s.now())
	if errors.Is(err, workflow.ErrStaleClaim) {
		return ErrWorkflowFence
	}
	return err
}

// models builds the models and the tool executor of the batch, like
// Orchestrator.run.
func (r *batchRun) models(ctx context.Context, cfg tenant.AgentConfig) (Triager, Investigator, *tools.Executor, []tools.ToolSpec, error) {
	o := r.s.o
	if o.models == nil {
		return nil, nil, nil, nil, &TriageError{Code: CodeLLMUnconfigured}
	}
	triager, investigator, err := o.models(ctx, r.batch.WorkspaceID)
	if err != nil || triager == nil {
		return nil, nil, nil, nil, &TriageError{Code: CodeLLMUnconfigured}
	}
	var runs tools.ToolRunStore = gatedRunStore{inner: o.runs, gate: r.gate}
	exec := tools.NewExecutor(o.reg, cfg.Tools, r.settings.Limits, r.settings.AllowDangerous, runs, o.redactor, r.batch.ID, r.batch.WorkspaceID)
	exec.ClickUpTokenRef = cfg.ClickUpTokenRef
	return triager, investigator, exec, exec.Specs(), nil
}

func (r *batchRun) budget() TokenBudget {
	return TokenBudget{MaxInput: r.settings.MaxInputTokens, MaxOutput: r.settings.MaxOutputTokens, Used: r.batch.Usage}
}

func (r *batchRun) addUsage(ctx context.Context, u domain.Usage) error {
	if u == (domain.Usage{}) {
		return nil
	}
	wctx, cancel := r.writeCtx()
	defer cancel()
	if err := r.s.store.AddBatchUsage(wctx, r.f(), r.batch.ID, u); err != nil {
		return err
	}
	r.batch.Usage.Add(u)
	return nil
}

func (r *batchRun) save(res BatchResult) (int64, error) {
	wctx, cancel := r.writeCtx()
	defer cancel()
	return r.s.store.SaveBatchResult(wctx, r.f(), r.batch.ID, res, r.st.checkpoint(true))
}

// triage asks the Tier-1 router once per batch and stores its routed
// decision (agent.triage).
func (r *batchRun) triage(ctx context.Context) (workflow.StepResult, error) {
	if r.tri != nil {
		return r.st.result()
	}
	o := r.s.o
	jc, err := r.load(ctx)
	if err != nil {
		return r.loadErr(ctx, err)
	}
	through := batchThrough(jc)
	if !jc.Config.Enabled {
		return r.fallback(ctx, jc, CodeAgentDisabled, through)
	}
	var maxID int64
	for _, m := range jc.History {
		maxID = max(maxID, m.ID)
	}
	res := BatchResult{Stage: StageTriage, Outcome: OutcomeIgnore, ThroughMessageID: through, LastTriagedID: jc.LastTriagedID, MaxMessageID: maxID}
	packet := r.packet(jc)
	if len(packet.NewBatch) > 0 {
		triager, investigator, _, specs, err := r.models(ctx, jc.Config)
		if err != nil {
			return r.handle(ctx, jc, err, through)
		}
		toolsAvailable := len(specs) > 0 && investigator != nil
		if r.budget().exhausted() {
			return r.handle(ctx, jc, &TriageError{Code: CodeTokenBudget}, through)
		}
		d, u, err := triager.Triage(ctx, packet, o.cleanInput(jc.Config.Persona), toolsAvailable)
		if uerr := r.addUsage(ctx, u); uerr != nil {
			return workflow.StepResult{}, storageErr(r.stepCtx, uerr)
		}
		if err != nil {
			return r.handle(ctx, jc, err, through)
		}
		threshold := jc.Config.ConfidenceThreshold
		if threshold <= 0 {
			threshold = tenant.DefaultConfidenceThreshold
		}
		res.Outcome = Route(d, RoutingPolicy{Threshold: threshold, ToolsAvailable: toolsAvailable})
		res.RouterNote = capBytes(o.routerNote(d, res.Outcome), 32768)
		res.Decision = TriageDecision{Category: d.Category, Urgency: d.Urgency, Confidence: d.Confidence,
			Summary: capBytes(o.clean(d.Summary), 16384), Goal: capBytes(o.cleanInput(d.Goal), 16384)}
		res.ToolsAvailable = toolsAvailable
		res.Unseen = unseenComments(jc, packet)
	}
	id, err := r.save(res)
	if err != nil {
		return workflow.StepResult{}, storageErr(r.stepCtx, err)
	}
	r.st.add(CheckpointTriage, strconv.FormatInt(id, 10), "")
	return r.st.result()
}

// investigate runs the Tier-2 actor once for an Investigate outcome and
// stores its report (agent.investigate). Other outcomes skip it.
func (r *batchRun) investigate(ctx context.Context) (workflow.StepResult, error) {
	if r.tri == nil {
		return workflow.StepResult{}, stepFail(CodeWorkflowReference)
	}
	if r.tri.Outcome != OutcomeInvestigate {
		r.st.add(CheckpointInvestigation, "skipped", "")
		return r.st.result()
	}
	if r.inv != nil {
		r.st.add(CheckpointInvestigation, strconv.FormatInt(r.inv.ID, 10), "")
		return r.st.result()
	}
	o := r.s.o
	jc, err := r.load(ctx)
	if err != nil {
		return r.loadErr(ctx, err)
	}
	through := r.through(jc)
	if !jc.Config.Enabled {
		return r.fallback(ctx, jc, CodeAgentDisabled, through)
	}
	_, investigator, exec, specs, err := r.models(ctx, jc.Config)
	if err == nil && investigator == nil {
		err = &TriageError{Code: CodeLLMUnconfigured}
	}
	if err != nil {
		return r.handle(ctx, jc, err, through)
	}
	budget := r.budget()
	if budget.exhausted() {
		return r.handle(ctx, jc, &TriageError{Code: CodeTokenBudget}, through)
	}
	report, err := investigator.Investigate(ctx, r.packet(jc), r.tri.Decision, o.cleanInput(jc.Config.Persona), specs, exec, budget)
	if uerr := r.addUsage(ctx, report.Usage); uerr != nil {
		return workflow.StepResult{}, storageErr(r.stepCtx, uerr)
	}
	if err != nil {
		return r.handle(ctx, jc, err, through)
	}
	total := r.batch.Usage
	id, err := r.save(BatchResult{
		Stage: StageInvestigation, ReportText: capBytes(o.clean(report.Text), 262144), ToolsLine: capBytes(toolsLine(report.Calls), 16384),
		TotalInputTokens: total.Input + total.CacheRead + total.CacheWrite, TotalOutputTokens: total.Output,
	})
	if err != nil {
		return workflow.StepResult{}, storageErr(r.stepCtx, err)
	}
	r.st.add(CheckpointInvestigation, strconv.FormatInt(id, 10), "")
	return r.st.result()
}

// report composes the Orchestrator's report from the stored results and
// commits it with the cursor, outbox and follow-up job (agent.report).
func (r *batchRun) report(ctx context.Context) (workflow.StepResult, error) {
	if r.tri == nil || r.tri.Outcome == OutcomeInvestigate && r.inv == nil {
		return workflow.StepResult{}, stepFail(CodeWorkflowReference)
	}
	o := r.s.o
	jc, err := r.load(ctx)
	if err != nil {
		return r.loadErr(ctx, err)
	}
	if r.inv != nil {
		r.st.add(CheckpointInvestigation, strconv.FormatInt(r.inv.ID, 10), "")
	}
	d := r.tri.Decision
	res := JobResult{Outcome: r.tri.Outcome, ThroughMessageID: r.tri.ThroughMessageID, RouterNote: r.tri.RouterNote}
	switch r.tri.Outcome {
	case OutcomeNotify:
		res.Text = o.notifyText(jc, d)
	case OutcomeInvestigate:
		res.ActorNote = r.inv.ReportText
		res.Text = o.investigateText(jc, d, r.inv.ReportText, r.inv.ToolsLine, r.inv.TotalInputTokens, r.inv.TotalOutputTokens)
	}
	res.Text = o.withUnseenNote(jc, res.Text, r.tri.Unseen)
	r.st.add(CheckpointReport, BatchDone, AgentOperationKey(r.batch.ID))
	wctx, cancel := r.writeCtx()
	defer cancel()
	if err := r.s.store.CommitBatch(wctx, r.f(), r.batch.ID, res, r.st.checkpoint(true)); err != nil {
		return workflow.StepResult{}, storageErr(r.stepCtx, err)
	}
	// Only after the report commit, like a legacy job.
	o.compress(r.stepCtx, r.batch.job())
	return r.st.result()
}

// handle maps a model or storage failure like Orchestrator.processNext: a
// retryable model failure is retried while the batch has attempts left;
// otherwise the batch falls back to a plain notice.
func (r *batchRun) handle(ctx context.Context, jc JobContext, err error, through int64) (workflow.StepResult, error) {
	if r.stepCtx.Err() != nil {
		return workflow.StepResult{}, r.stepCtx.Err()
	}
	var te *TriageError
	if !errors.As(err, &te) {
		return workflow.StepResult{}, storageErr(r.stepCtx, err)
	}
	if !te.Retryable || r.batch.Attempts >= MaxAgentAttempts {
		return r.fallback(ctx, jc, te.Code, through)
	}
	wctx, cancel := r.writeCtx()
	defer cancel()
	if err := r.s.store.RetryBatch(wctx, r.f(), r.batch.ID, te.Code); err != nil {
		return workflow.StepResult{}, storageErr(r.stepCtx, err)
	}
	cp := r.st.checkpoint(false)
	return workflow.StepResult{Checkpoint: &cp}, &workflow.StepError{Code: workflow.ReasonCode(te.Code), Retryable: true}
}

// fallbackFresh loads the batch and falls back (attempts already used up).
func (r *batchRun) fallbackFresh(ctx context.Context, code string) (workflow.StepResult, error) {
	jc, err := r.load(ctx)
	if err != nil {
		return r.loadErr(ctx, err)
	}
	return r.fallback(ctx, jc, code, r.through(jc))
}

// fallback commits the plain notice of the batch, like
// Orchestrator.fallback. The batch is handled: the step succeeds and the
// later steps find its receipt.
func (r *batchRun) fallback(_ context.Context, jc JobContext, code string, through int64) (workflow.StepResult, error) {
	log.Printf("agent batch %d falls back to a notice (%s)", r.batch.ID, code)
	r.st.add(CheckpointReport, BatchFailed, AgentOperationKey(r.batch.ID))
	wctx, cancel := r.writeCtx()
	defer cancel()
	if err := r.s.store.CommitBatchFallback(wctx, r.f(), r.batch.ID, code, r.s.o.fallbackText(jc, code), through, r.st.checkpoint(true)); err != nil {
		return workflow.StepResult{}, storageErr(r.stepCtx, err)
	}
	return r.st.result()
}

// capBytes cuts s to at most n bytes on a rune boundary.
func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// gatedRunStore re-checks the workflow job claim before every tool call
// begins (Begin is the last step before a tool runs), so a stale claimant
// cannot start a tool call. Denials and finishes are audit only.
type gatedRunStore struct {
	inner tools.ToolRunStore
	gate  func(context.Context) error
}

func (g gatedRunStore) Begin(ctx context.Context, start tools.RunStart) (int64, *tools.PriorRun, error) {
	if g.inner == nil {
		return 0, nil, errors.New("tool run audit unavailable")
	}
	if err := g.gate(ctx); err != nil {
		return 0, nil, err
	}
	return g.inner.Begin(ctx, start)
}

func (g gatedRunStore) Finish(ctx context.Context, f tools.RunFinish) error {
	if g.inner == nil {
		return errors.New("tool run audit unavailable")
	}
	return g.inner.Finish(ctx, f)
}

func (g gatedRunStore) Deny(ctx context.Context, start tools.RunStart, code string) error {
	if g.inner == nil {
		return errors.New("tool run audit unavailable")
	}
	return g.inner.Deny(ctx, start, code)
}

// JobCounts forwards the inner store's budget counter, if any.
func (g gatedRunStore) JobCounts(ctx context.Context, jobID int64) (int, int, error) {
	if c, ok := g.inner.(tools.JobBudgetCounter); ok {
		return c.JobCounts(ctx, jobID)
	}
	return 0, 0, nil
}

// FindPrior forwards the inner store's prior lookup, if any.
func (g gatedRunStore) FindPrior(ctx context.Context, key string) (*tools.PriorRun, error) {
	if pf, ok := g.inner.(tools.PriorFinder); ok {
		return pf.FindPrior(ctx, key)
	}
	return nil, nil
}
