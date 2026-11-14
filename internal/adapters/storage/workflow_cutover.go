package storage

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// This file is the cutover from the legacy behavior run and agent job
// claimers to the durable workflow queue, one workspace at a time.
//
// Runtime-mode fence. A workspace is in mode legacy (no row) or workflow
// (workflow_runtime_modes). Every legacy claim (BehaviorStore.ClaimRun,
// AgentStore.Claim) and every durable start (StartPendingBehaviorRuns,
// AgentWorkflowStore.StartDueBatches) takes a KEY SHARE lock on the
// workspace row with SKIP LOCKED and reads the mode in its own
// transaction: a legacy claim proceeds only in mode legacy, a durable start
// only in mode workflow, and both skip a workspace whose cutover is running
// (the cutover holds FOR UPDATE on the row until it commits). So before the
// cutover the dormant durable adapters create no competing executions, and
// after it the legacy claimers take nothing of the workspace.
//
// Cutover (apply) imports, in the transaction that sets the fence:
//   - every pending or running behavior run no execution owns: an execution
//     pinned to the run's version, trigger and operation key
//     behavior_run:<id> (the run's tool-run, message and action keys are
//     bound to the run ID, so receipts are reused and an in-flight action
//     without a receipt stays an unknown outcome, never resent). A running
//     run keeps its legacy claim; its first job is due when that lease
//     ends, and the durable attempt then takes the run over;
//   - every done behavior run that still has an open blocking approval: an
//     execution whose steps find the run's end and wait on the approvals;
//   - the open blocking approvals of those runs, bound as waits of the step
//     that creates approvals (behavior.dispatch or actions), so a decision
//     is applied once by the fenced continuation and never inline.
//
// Agent batches need the process settings snapshot, so they are adopted
// lazily by StartDueBatches once the workspace is in mode workflow: due
// pending batches (debounce and max-wait preserved) and jobs a legacy
// claimer left processing, once their lease or retry wait has ended.
//
// Nothing is deleted: legacy rows stay as identifiers, receipts and read
// projections. A record that cannot be mapped blocks the cutover with its
// ID and a code. A second apply is a no-op.

// Runtime modes of the cutover fence.
const (
	RuntimeModeLegacy   = "legacy"
	RuntimeModeWorkflow = "workflow"
)

// Cutover block codes.
const (
	CutoverCodeVersionInvalid     = "behavior_version_invalid"
	CutoverCodeApprovalUnmappable = "approval_step_unmappable"
)

// CutoverKindBehaviorRun is the record kind of a behavior run in a report.
const CutoverKindBehaviorRun = "behavior_run"

// ErrCutoverBlocked is returned by an apply that found unmappable records;
// nothing was written.
var ErrCutoverBlocked = errors.New("workflow cutover: blocked by unmappable records")

// maxCutoverIssues bounds the blocked records listed in a report.
const maxCutoverIssues = 100

// cutoverPage is the page size of the run listing.
const cutoverPage = 500

// CutoverIssue is one record that blocks the cutover.
type CutoverIssue struct {
	Kind string
	ID   int64
	Code string
}

// CutoverReport holds counts, blocked IDs and codes only.
type CutoverReport struct {
	WorkspaceID string
	// Mode is the workspace's runtime mode after the call.
	Mode string
	// Applied is true when this call imported the records and set the fence.
	Applied bool
	// AlreadyCutOver is true when the workspace was already in mode workflow.
	AlreadyCutOver bool

	// Behavior runs the cutover maps to executions.
	BehaviorRunsPending   int
	BehaviorRunsRunning   int
	BehaviorRunsLeaseLive int // running with an unexpired legacy lease: unavailable until it ends
	BehaviorRunsWaiting   int // done runs with open blocking approvals
	ApprovalWaits         int // open blocking approvals bound as waits
	ImportedExecutions    int // apply only

	// Agent batches the durable starter adopts after the cutover.
	AgentJobsPending    int
	AgentJobsPendingDue int
	AgentJobsProcessing int // left processing by a legacy claimer
	AgentJobsLeaseLive  int // of those, with an unexpired lease or retry wait

	// LegacyRouteApprovals are open blocking approvals of failed or canceled
	// runs: no execution exists for them, so their decision keeps the
	// legacy callback route (one route per decision).
	LegacyRouteApprovals int
	// InFlightToolRuns are tool runs started without a recorded outcome by
	// mapped runs and jobs; they stay unknown outcomes and are never resent.
	InFlightToolRuns int

	Blocked      []CutoverIssue
	BlockedTotal int
}

// Ready reports whether nothing blocks the cutover.
func (r CutoverReport) Ready() bool { return r.BlockedTotal == 0 }

func (r *CutoverReport) block(kind string, id int64, code string) {
	r.BlockedTotal++
	if len(r.Blocked) < maxCutoverIssues {
		r.Blocked = append(r.Blocked, CutoverIssue{Kind: kind, ID: id, Code: code})
	}
}

// --- runtime-mode fence ---

// runtimeModeShared takes the workspace's KEY SHARE lock without waiting and
// reads its mode. held is false while a cutover holds the workspace (or the
// workspace does not exist): the caller skips it.
func runtimeModeShared(ctx context.Context, q *sqlc.Queries, ws pgtype.UUID) (mode string, held bool, err error) {
	if _, err := q.LockWorkspaceRuntimeShared(ctx, ws); err != nil {
		if isNoRows(err) {
			return "", false, nil
		}
		return "", false, err
	}
	// A separate statement: it sees every cutover committed before the lock.
	mode, err = q.GetWorkflowRuntimeMode(ctx, ws)
	if err != nil {
		return "", false, err
	}
	return mode, true, nil
}

// legacyClaimAllowed reports whether a legacy claimer may take work of the
// workspace in the caller's transaction.
func legacyClaimAllowed(ctx context.Context, q *sqlc.Queries, ws pgtype.UUID) (bool, error) {
	mode, held, err := runtimeModeShared(ctx, q, ws)
	return held && mode == RuntimeModeLegacy, err
}

// workflowStartAllowed reports whether a durable starter may create
// executions of the workspace in the caller's transaction.
func workflowStartAllowed(ctx context.Context, q *sqlc.Queries, ws pgtype.UUID) (bool, error) {
	mode, held, err := runtimeModeShared(ctx, q, ws)
	return held && mode == RuntimeModeWorkflow, err
}

// RuntimeMode returns the workspace's runtime mode (read only).
func (s *WorkflowStore) RuntimeMode(ctx context.Context, workspaceID string) (string, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return "", workflowInvalid("workspace_id")
	}
	return s.q.GetWorkflowRuntimeMode(ctx, ws)
}

// WorkflowWorkspaces lists the cut-over workspaces.
func (s *WorkflowStore) WorkflowWorkspaces(ctx context.Context) ([]string, error) {
	var out []string
	after := pgtype.UUID{Valid: true}
	for {
		page, err := s.q.ListWorkflowModeWorkspaces(ctx, sqlc.ListWorkflowModeWorkspacesParams{AfterID: after, MaxRows: 100})
		if err != nil {
			return nil, err
		}
		for _, id := range page {
			out = append(out, uuidString(id))
		}
		if len(page) < 100 {
			return out, nil
		}
		after = page[len(page)-1]
	}
}

// --- cutover ---

type cutoverRun struct {
	row      sqlc.BehaviorRun
	def      workflow.Definition
	waitStep string
}

// approvalStepKey is the step that creates a behavior run's approvals: the
// step running the dispatch phase.
func approvalStepKey(def workflow.Definition) string {
	for _, step := range def.Steps {
		if step.Kind == workflow.StepKindBehaviorDispatch || step.Kind == workflow.StepKindActions {
			return step.Key
		}
	}
	return ""
}

// errRunDefinitionInvalid marks a pinned version that is missing or cannot
// be resolved and normalized into a workflow definition.
var errRunDefinitionInvalid = errors.New("behavior run definition invalid")

// behaviorRunDefinition normalizes the run's pinned version. A storage
// failure is returned as is; an unusable version is errRunDefinitionInvalid.
func behaviorRunDefinition(ctx context.Context, q *sqlc.Queries, ws pgtype.UUID, versionID int64) (workflow.Definition, error) {
	v, err := q.GetBehaviorVersionByID(ctx, sqlc.GetBehaviorVersionByIDParams{WorkspaceID: ws, ID: versionID})
	if isNoRows(err) {
		return workflow.Definition{}, errRunDefinitionInvalid
	}
	if err != nil {
		return workflow.Definition{}, err
	}
	doc, err := behavior.ResolveVersion(mapVersion(v, ""))
	if err != nil {
		return workflow.Definition{}, errRunDefinitionInvalid
	}
	def, err := behavior.NormalizeWorkflow(doc)
	if err != nil {
		return workflow.Definition{}, errRunDefinitionInvalid
	}
	return def, nil
}

// planCutover reads everything the cutover maps and every reason it is
// blocked; it writes nothing.
func planCutover(ctx context.Context, q *sqlc.Queries, ws pgtype.UUID, now time.Time) ([]cutoverRun, CutoverReport, error) {
	report := CutoverReport{WorkspaceID: uuidString(ws)}
	type versionResult struct {
		def workflow.Definition
		ok  bool
	}
	versions := map[int64]versionResult{}
	var plan []cutoverRun
	after := int64(0)
	for {
		rows, err := q.ListCutoverBehaviorRuns(ctx, sqlc.ListCutoverBehaviorRunsParams{WorkspaceID: ws, AfterID: after, MaxRows: cutoverPage})
		if err != nil {
			return nil, report, err
		}
		for _, row := range rows {
			after = row.ID
			vr, seen := versions[row.BehaviorVersionID]
			if !seen {
				def, err := behaviorRunDefinition(ctx, q, ws, row.BehaviorVersionID)
				if err != nil && !errors.Is(err, errRunDefinitionInvalid) {
					return nil, report, err
				}
				vr = versionResult{def: def, ok: err == nil}
				versions[row.BehaviorVersionID] = vr
			}
			if !vr.ok {
				report.block(CutoverKindBehaviorRun, row.ID, CutoverCodeVersionInvalid)
				continue
			}
			approvals, err := q.ListOpenBlockingApprovals(ctx, sqlc.ListOpenBlockingApprovalsParams{
				WorkspaceID: ws, RunID: pgtype.Int8{Int64: row.ID, Valid: true},
			})
			if err != nil {
				return nil, report, err
			}
			step := ""
			if len(approvals) > 0 {
				if step = approvalStepKey(vr.def); step == "" {
					report.block(CutoverKindBehaviorRun, row.ID, CutoverCodeApprovalUnmappable)
					continue
				}
			}
			switch behavior.RunStatus(row.Status) {
			case behavior.RunPending:
				report.BehaviorRunsPending++
			case behavior.RunRunning:
				report.BehaviorRunsRunning++
				if row.ClaimToken.Valid && row.LockedUntil.Valid && row.LockedUntil.Time.After(now) {
					report.BehaviorRunsLeaseLive++
				}
			case behavior.RunDone:
				if len(approvals) == 0 {
					continue
				}
				report.BehaviorRunsWaiting++
			default:
				continue
			}
			report.ApprovalWaits += len(approvals)
			plan = append(plan, cutoverRun{row: row, def: vr.def, waitStep: step})
		}
		if len(rows) < cutoverPage {
			break
		}
	}
	stats, err := q.CutoverAgentJobStats(ctx, sqlc.CutoverAgentJobStatsParams{AtTime: ts(now), WorkspaceID: ws})
	if err != nil {
		return nil, report, err
	}
	report.AgentJobsPending, report.AgentJobsPendingDue = int(stats.Pending), int(stats.PendingDue)
	report.AgentJobsProcessing, report.AgentJobsLeaseLive = int(stats.Processing), int(stats.LeaseLive)
	legacyRoute, err := q.CountLegacyRouteApprovals(ctx, ws)
	if err != nil {
		return nil, report, err
	}
	report.LegacyRouteApprovals = int(legacyRoute)
	inFlight, err := q.CountInFlightToolRuns(ctx, ws)
	if err != nil {
		return nil, report, err
	}
	report.InFlightToolRuns = int(inFlight)
	return plan, report, nil
}

// Cutover maps the workspace's legacy behavior runs and blocking approvals
// to executions and sets its runtime-mode fence to workflow. apply=false is
// a read-only preflight (a READ ONLY transaction, no lock, no write);
// apply=true locks the workspace, validates, imports and fences it in one
// transaction, or writes nothing and returns ErrCutoverBlocked with the
// report. An apply on a workspace already in mode workflow is a no-op.
func (s *WorkflowStore) Cutover(ctx context.Context, workspaceID string, apply bool) (CutoverReport, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return CutoverReport{}, workflowInvalid("workspace_id")
	}
	now := time.Now()
	if !apply {
		return s.cutoverPreflight(ctx, ws, now)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CutoverReport{}, err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	if _, err := q.LockWorkspaceForCutover(ctx, ws); err != nil {
		if isNoRows(err) {
			return CutoverReport{}, workflow.ErrNotFound
		}
		return CutoverReport{}, err
	}
	mode, err := q.GetWorkflowRuntimeMode(ctx, ws)
	if err != nil {
		return CutoverReport{}, err
	}
	plan, report, err := planCutover(ctx, q, ws, now)
	if err != nil {
		return CutoverReport{}, err
	}
	report.Mode = mode
	if mode == RuntimeModeWorkflow {
		// Already cut over: nothing is imported or changed again; runs
		// created since are started by StartPendingBehaviorRuns.
		report.AlreadyCutOver = true
		return report, nil
	}
	if !report.Ready() {
		return report, ErrCutoverBlocked
	}
	for _, p := range plan {
		imported, err := s.importBehaviorRun(ctx, tx, q, ws, p, now)
		if err != nil {
			return CutoverReport{}, fmt.Errorf("cutover behavior run %d: %w", p.row.ID, err)
		}
		if imported {
			report.ImportedExecutions++
		}
	}
	if err := q.SetWorkflowRuntimeModeWorkflow(ctx, sqlc.SetWorkflowRuntimeModeWorkflowParams{WorkspaceID: ws, AtTime: ts(now)}); err != nil {
		return CutoverReport{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CutoverReport{}, err
	}
	report.Applied, report.Mode = true, RuntimeModeWorkflow
	return report, nil
}

func (s *WorkflowStore) cutoverPreflight(ctx context.Context, ws pgtype.UUID, now time.Time) (CutoverReport, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return CutoverReport{}, err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	if _, err := q.GetWorkspaceByID(ctx, ws); err != nil {
		if isNoRows(err) {
			return CutoverReport{}, workflow.ErrNotFound
		}
		return CutoverReport{}, err
	}
	mode, err := q.GetWorkflowRuntimeMode(ctx, ws)
	if err != nil {
		return CutoverReport{}, err
	}
	_, report, err := planCutover(ctx, q, ws, now)
	if err != nil {
		return CutoverReport{}, err
	}
	report.Mode = mode
	report.AlreadyCutOver = mode == RuntimeModeWorkflow
	return report, nil
}

// importBehaviorRun creates the run's execution and binds its open blocking
// approvals, under the run's row lock. A run that changed state or got an
// execution meanwhile is skipped.
func (s *WorkflowStore) importBehaviorRun(ctx context.Context, tx pgx.Tx, q *sqlc.Queries, ws pgtype.UUID, p cutoverRun, now time.Time) (bool, error) {
	row, err := q.LockCutoverBehaviorRun(ctx, sqlc.LockCutoverBehaviorRunParams{WorkspaceID: ws, ID: p.row.ID})
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	owned, err := q.BehaviorRunHasExecution(ctx, sqlc.BehaviorRunHasExecutionParams{WorkspaceID: ws, RunID: pgtype.Int8{Int64: row.ID, Valid: true}})
	if err != nil || owned {
		return false, err
	}
	approvals, err := q.ListOpenBlockingApprovals(ctx, sqlc.ListOpenBlockingApprovalsParams{
		WorkspaceID: ws, RunID: pgtype.Int8{Int64: row.ID, Valid: true},
	})
	if err != nil {
		return false, err
	}
	due := now
	switch behavior.RunStatus(row.Status) {
	case behavior.RunPending:
		if row.RunAt.Valid && row.RunAt.Time.After(due) {
			due = row.RunAt.Time
		}
	case behavior.RunRunning:
		// An unexpired legacy lease stays unavailable until it ends.
		if row.ClaimToken.Valid && row.LockedUntil.Valid && row.LockedUntil.Time.After(due) {
			due = row.LockedUntil.Time
		}
	case behavior.RunDone:
		if len(approvals) == 0 {
			return false, nil
		}
	default:
		return false, nil
	}
	req := behavior.WorkflowStartRequest(mapRun(row, ""), p.def)
	req.At = due
	execution, err := s.StartExecutionTx(ctx, tx, req)
	if err != nil {
		return false, err
	}
	if execution.References.LegacyRunID != row.ID {
		return false, workflow.ErrConflict
	}
	step := p.waitStep
	if step == "" {
		step = approvalStepKey(p.def)
	}
	if step == "" {
		return true, nil
	}
	for _, a := range approvals {
		locked, err := q.LockOpenApproval(ctx, sqlc.LockOpenApprovalParams{WorkspaceID: ws, ID: a.ID})
		if isNoRows(err) {
			continue // decided meanwhile: its decision already took its route
		}
		if err != nil {
			return false, err
		}
		kind, ok := waitKindOf(behavior.ApprovalKind(locked.Kind))
		if !ok {
			continue
		}
		if _, err := q.InsertWorkflowWait(ctx, sqlc.InsertWorkflowWaitParams{
			WorkspaceID: ws, ExecutionID: execution.ID, StepKey: step, ApprovalID: locked.ID, WaitKind: kind,
		}); err != nil {
			return false, err
		}
	}
	return true, nil
}

// --- durable starters ---

// StartPendingBehaviorRuns starts executions for up to limit due pending
// runs of a cut-over workspace that no execution owns yet (runs created by
// the existing trigger paths). Each run is started in its own transaction
// behind the runtime-mode fence; in a legacy workspace (or while its
// cutover runs) nothing is started. A run whose pinned version cannot be
// normalized ends failed with workflow_reference_invalid.
func (s *WorkflowStore) StartPendingBehaviorRuns(ctx context.Context, workspaceID string, now time.Time, limit int) (int, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil || now.IsZero() || limit < 1 {
		return 0, workflowInvalid("start behavior runs")
	}
	started := 0
	for started < limit {
		ok, err := s.startOneBehaviorRun(ctx, ws, now)
		if err != nil || !ok {
			return started, err
		}
		started++
	}
	return started, nil
}

func (s *WorkflowStore) startOneBehaviorRun(ctx context.Context, ws pgtype.UUID, now time.Time) (bool, error) {
	for {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return false, err
		}
		started, retry, err := s.startBehaviorRunTx(ctx, tx, ws, now)
		if err != nil {
			_ = tx.Rollback(ctx)
			return false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		if !retry {
			return started, nil
		}
	}
}

// startBehaviorRunTx starts the oldest startable run. retry reports that
// an unstartable run was failed and the next one may be tried.
func (s *WorkflowStore) startBehaviorRunTx(ctx context.Context, tx pgx.Tx, ws pgtype.UUID, now time.Time) (started, retry bool, err error) {
	q := sqlc.New(tx)
	allowed, err := workflowStartAllowed(ctx, q, ws)
	if err != nil || !allowed {
		return false, false, err
	}
	row, err := q.SelectStartableBehaviorRun(ctx, sqlc.SelectStartableBehaviorRunParams{WorkspaceID: ws, AtTime: ts(now)})
	if isNoRows(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	def, err := behaviorRunDefinition(ctx, q, ws, row.BehaviorVersionID)
	if err != nil {
		if !errors.Is(err, errRunDefinitionInvalid) {
			return false, false, err
		}
		n, err := q.FailUnstartableBehaviorRun(ctx, sqlc.FailUnstartableBehaviorRunParams{
			Code: behavior.CodeWorkflowReference, WorkspaceID: ws, ID: row.ID,
		})
		if err != nil {
			return false, false, err
		}
		if n != 1 {
			return false, false, workflow.ErrConflict
		}
		log.Printf("behavior run %d corr=%s: failed (%s)", row.ID, uuidString(row.CorrelationID), behavior.CodeWorkflowReference)
		return false, true, nil
	}
	imported, err := s.importBehaviorRun(ctx, tx, q, ws, cutoverRun{row: row, def: def, waitStep: approvalStepKey(def)}, now)
	if err != nil {
		return false, false, err
	}
	return imported, false, nil
}

// --- legacy projections ---

// repairLegacyProjectionTx ends the read projection of the behavior run of
// an execution that becomes FAILED or CANCELLED outside the run's own steps,
// in the transaction that ends the execution. A run the steps already ended
// is left alone. Agent batches are released by ReleaseOrphanBatch, which
// needs the fallback notice text.
func repairLegacyProjectionTx(ctx context.Context, q *sqlc.Queries, ws pgtype.UUID, executionID int64, to workflow.State, reason workflow.ReasonCode) error {
	var status behavior.RunStatus
	switch to {
	case workflow.StateFailed:
		status = behavior.RunFailed
	case workflow.StateCancelled:
		status = behavior.RunCanceled
	default:
		return nil
	}
	code := string(reason)
	if code == "" {
		code = strings.ToLower(string(to))
	}
	_, err := q.RepairBehaviorRunProjection(ctx, sqlc.RepairBehaviorRunProjectionParams{
		Status: string(status), Code: code, WorkspaceID: ws, ExecutionID: executionID,
	})
	return err
}

// --- orphan agent batches (AgentWorkflowStore) ---

var _ agent.BatchReleaseStore = (*AgentWorkflowStore)(nil)

// ListOrphanBatches lists up to limit workflow-owned batches of the
// workspace still processing although their execution failed or was
// cancelled and no live execution (retry child) owns them.
func (s *AgentWorkflowStore) ListOrphanBatches(ctx context.Context, workspaceID string, limit int) ([]agent.OrphanBatch, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil || limit < 1 {
		return nil, workflowInvalid("orphan batches")
	}
	rows, err := s.q.ListOrphanAgentBatches(ctx, sqlc.ListOrphanAgentBatchesParams{WorkspaceID: ws, MaxRows: int32(min(limit, 100))})
	if err != nil {
		return nil, err
	}
	out := make([]agent.OrphanBatch, 0, len(rows))
	for _, r := range rows {
		row, err := s.q.GetAgentBatch(ctx, sqlc.GetAgentBatchParams{WorkspaceID: ws, ID: r.JobID})
		if isNoRows(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, agent.OrphanBatch{Batch: mapAgentBatch(row), ExecutionID: r.ExecutionID, Reason: r.ReasonCode})
	}
	return out, nil
}

// ReleaseOrphanBatch ends an orphan batch like a legacy batch whose
// attempts ran out: failed with code, the fallback notice in the outbox
// (dedupe key agent_job:<id>), the cursor moved to through and the
// follow-up job enqueued, in one transaction. It re-checks under the
// execution lock that the execution ended and no live execution owns the
// batch; a batch already ended is left alone.
func (s *AgentWorkflowStore) ReleaseOrphanBatch(ctx context.Context, o agent.OrphanBatch, code, text string, through int64) error {
	ws, thread, err := parseScopedIDs(o.Batch.WorkspaceID, o.Batch.ThreadID)
	if err != nil || o.Batch.ID <= 0 || o.ExecutionID <= 0 || strings.TrimSpace(code) == "" || through < 0 {
		return workflowInvalid("orphan batch")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	// Lock order: the execution, then the batch.
	execution, err := q.LockWorkflowExecution(ctx, sqlc.LockWorkflowExecutionParams{WorkspaceID: ws, ID: o.ExecutionID})
	if err != nil {
		if isNoRows(err) {
			return agent.ErrBatchNotFound
		}
		return err
	}
	if state := workflow.State(execution.State); state != workflow.StateFailed && state != workflow.StateCancelled {
		return agent.ErrBatchConflict
	}
	live, err := q.CountLiveAgentJobExecutions(ctx, sqlc.CountLiveAgentJobExecutionsParams{WorkspaceID: ws, JobID: pgtype.Int8{Int64: o.Batch.ID, Valid: true}})
	if err != nil {
		return err
	}
	if live != 0 {
		return agent.ErrBatchConflict
	}
	row, err := q.LockOrphanAgentBatch(ctx, sqlc.LockOrphanAgentBatchParams{WorkspaceID: ws, ID: o.Batch.ID, ExecutionID: pgtype.Int8{Int64: o.ExecutionID, Valid: true}})
	if isNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	b := mapAgentBatch(row)
	finish := func(code string) error {
		return batchRows(q.FinishDurableAgentBatch(ctx, sqlc.FinishDurableAgentBatchParams{
			Status: agent.BatchFailed, Through: pgtype.Int8{Int64: through, Valid: through > 0}, Code: optText(code),
			WorkspaceID: ws, ID: b.ID,
		}))
	}
	if err := s.agents.writeBatchOutcome(ctx, q, agentBatchJob(b), ws, thread, code, "", "", text, through, finish); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
