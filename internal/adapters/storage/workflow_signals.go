package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// This file is the approval bridge of durable executions.
//
//   - A blocking approval created by a durable step is bound to that step
//     (workflow_waits) in the same transaction that creates it, under the
//     step's job fence. Approvals created any other way (legacy runs, alias,
//     preferences, feedback, assistant) have no wait and never block or wake
//     an execution.
//   - Deciding or expiring a bound approval records one signal
//     (workflow_signals) in the same transaction as the decision and, when
//     the step is waiting, queues the step again in that transaction.
//   - The step's fenced continuation applies the decision and marks the
//     signal consumed; a step that ends its attempt in a wait while an
//     unconsumed signal exists is queued again at once (WaitJob).
//   - ReconcileSignals repairs a decision whose signal or wakeup is missing.
//
// Signals hold identifiers and decision codes only; an answer stays on its
// approval row.

var _ behavior.ApprovalWaitStore = (*BehaviorStore)(nil)

// ReasonSignalReceived and ReasonResumed are the transition reasons of a
// woken wait.
const (
	ReasonSignalReceived workflow.ReasonCode = "signal_received"
	ReasonResumed        workflow.ReasonCode = "resumed"
)

// SignalReferenceApproval is the only signal reference kind: an approval ID.
const SignalReferenceApproval = "approval"

func approvalSignalID(approvalID int64) string {
	return "approval:" + strconv.FormatInt(approvalID, 10)
}

func waitKindOf(kind behavior.ApprovalKind) (string, bool) {
	switch kind {
	case behavior.ApprovalItemAnswer:
		return "input", true
	case behavior.ApprovalPostDraft, behavior.ApprovalConfirmAction:
		return "approval", true
	}
	return "", false
}

// wakeWaitingStepTx queues a waiting step again when it has a recorded,
// unconsumed signal and the execution holds no live job. It reports whether
// it queued the step. A running, queued or terminal execution is left alone:
// a running step re-checks its signals before it waits.
func wakeWaitingStepTx(ctx context.Context, q *sqlc.Queries, ws pgtype.UUID, executionID int64, stepKey string,
	at time.Time, reason workflow.ReasonCode, idempotencyKey string) (sqlc.WorkflowExecution, bool, error) {
	row, err := q.LockWorkflowExecution(ctx, sqlc.LockWorkflowExecutionParams{WorkspaceID: ws, ID: executionID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlc.WorkflowExecution{}, false, workflow.ErrNotFound
		}
		return sqlc.WorkflowExecution{}, false, err
	}
	from := workflow.State(row.State)
	if !from.IsWaiting() {
		return row, false, nil
	}
	if row.DeadlineAt.Valid && !row.DeadlineAt.Time.After(at) {
		// Past its deadline the wait is failed by the reconciler, not resumed.
		return row, false, nil
	}
	step, err := q.GetExecutionStep(ctx, sqlc.GetExecutionStepParams{WorkspaceID: ws, ExecutionID: executionID, StepKey: stepKey})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return row, false, nil
		}
		return row, false, err
	}
	if workflow.StepState(step.State) != workflow.StepWaiting {
		return row, false, nil
	}
	pending, err := q.CountPendingStepSignals(ctx, sqlc.CountPendingStepSignalsParams{WorkspaceID: ws, ExecutionID: executionID, StepKey: stepKey})
	if err != nil || pending == 0 {
		return row, false, err
	}
	if _, err := q.GetActiveWorkflowJobForExecution(ctx, sqlc.GetActiveWorkflowJobForExecutionParams{WorkspaceID: ws, ExecutionID: executionID}); err == nil {
		return row, false, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return row, false, err
	}
	var definition workflow.Definition
	if err := json.Unmarshal(row.Definition, &definition); err != nil {
		return row, false, err
	}
	if !workflow.CanTransition(from, workflow.StateQueued) {
		return row, false, fmt.Errorf("%w: %s to %s", errReconcileIllegalTransition, from, workflow.StateQueued)
	}
	if _, err := q.SetExecutionStepState(ctx, sqlc.SetExecutionStepStateParams{
		State: string(workflow.StepPending), AtTime: ts(at), WorkspaceID: ws, ExecutionID: executionID, StepKey: stepKey,
	}); err != nil {
		return row, false, err
	}
	if _, err := q.InsertWorkflowJob(ctx, sqlc.InsertWorkflowJobParams{
		WorkspaceID: ws, ExecutionID: executionID, StepKey: stepKey, DueAt: ts(at), MaxAttempts: int32(definition.Retry.MaxAttempts),
		// The woken job resumes the step's current attempt: its claim keeps
		// the attempt number (fence: job attempts == step attempts) and does
		// not count against max_attempts, so approval rounds never exhaust
		// the retry budget.
		Attempts: step.Attempts, ContinuesAttempt: step.Attempts >= 1,
	}); err != nil {
		return row, false, err
	}
	queued, err := transitionExecutionTx(ctx, q, ws, row, workflow.StateQueued, stepKey, int(step.Attempts), reason, idempotencyKey)
	if err != nil {
		return row, false, err
	}
	return queued, true, nil
}

// recordApprovalSignalTx records the signal of a decided approval when it
// is bound to an execution step, and wakes the step. It returns the bound
// execution ID, 0 for an approval without a wait.
func recordApprovalSignalTx(ctx context.Context, q *sqlc.Queries, approval sqlc.Approval, at time.Time) (int64, error) {
	if !approval.Decision.Valid {
		return 0, nil
	}
	wait, err := q.GetWorkflowWaitByApproval(ctx, sqlc.GetWorkflowWaitByApprovalParams{WorkspaceID: approval.WorkspaceID, ApprovalID: approval.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if err := insertWaitSignal(ctx, q, wait, approval.Decision.String, approvalSignalID(approval.ID)); err != nil {
		return 0, err
	}
	if _, _, err := wakeWaitingStepTx(ctx, q, wait.WorkspaceID, wait.ExecutionID, wait.StepKey, at, ReasonSignalReceived, ""); err != nil {
		return 0, err
	}
	return wait.ExecutionID, nil
}

// insertWaitSignal inserts the wait's one signal; a repeat is a no-op.
func insertWaitSignal(ctx context.Context, q *sqlc.Queries, wait sqlc.WorkflowWait, decision, signalID string) error {
	_, err := q.InsertWorkflowSignal(ctx, sqlc.InsertWorkflowSignalParams{
		WorkspaceID: wait.WorkspaceID, SignalID: signalID, ExecutionID: wait.ExecutionID, StepKey: wait.StepKey,
		WaitID: wait.ID, ReferenceID: wait.ApprovalID, Decision: decision,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

// listMissingSignalWaits lists bound approvals whose decision is recorded
// without a signal (executionID 0: any execution).
func listMissingSignalWaits(ctx context.Context, q *sqlc.Queries, ws pgtype.UUID, executionID int64) ([]sqlc.WorkflowWait, error) {
	params := sqlc.ListUnsignalledDecidedWaitsParams{}
	if executionID != 0 {
		params = sqlc.ListUnsignalledDecidedWaitsParams{ExecutionID: pgtype.Int8{Int64: executionID, Valid: true}, WorkspaceID: ws}
	}
	return q.ListUnsignalledDecidedWaits(ctx, params)
}

// repairWaitSignalTx records the signal of one bound approval whose
// decision is recorded without one.
func repairWaitSignalTx(ctx context.Context, q *sqlc.Queries, wait sqlc.WorkflowWait) error {
	approval, err := q.GetApproval(ctx, sqlc.GetApprovalParams{WorkspaceID: wait.WorkspaceID, ID: wait.ApprovalID})
	if err != nil {
		return err
	}
	if !approval.Decision.Valid {
		return nil
	}
	return insertWaitSignal(ctx, q, wait, approval.Decision.String, approvalSignalID(approval.ID))
}

// repairMissingSignalsTx records the signals of bound approvals whose
// decision is recorded without one (executionID 0: any execution).
func repairMissingSignalsTx(ctx context.Context, q *sqlc.Queries, ws pgtype.UUID, executionID int64) error {
	waits, err := listMissingSignalWaits(ctx, q, ws, executionID)
	if err != nil {
		return err
	}
	for _, wait := range waits {
		if err := repairWaitSignalTx(ctx, q, wait); err != nil {
			return err
		}
	}
	return nil
}

// closeExecutionWaits closes the open waits of an execution that ended, in
// the transaction that ends it.
func closeExecutionWaits(ctx context.Context, q *sqlc.Queries, ws pgtype.UUID, executionID int64, at time.Time) error {
	_, err := q.CloseWorkflowWaits(ctx, sqlc.CloseWorkflowWaitsParams{AtTime: ts(at), WorkspaceID: ws, ExecutionID: executionID})
	return err
}

// --- behavior side (BehaviorStore) ---

// decideApprovalTx takes the decision and records its signal atomically.
func (s *BehaviorStore) decideApprovalTx(ctx context.Context, params sqlc.DecideApprovalParams, now time.Time) (behavior.Approval, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return behavior.Approval{}, err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	row, err := q.DecideApproval(ctx, params)
	if err != nil {
		return behavior.Approval{}, err
	}
	executionID, err := recordApprovalSignalTx(ctx, q, row, now)
	if err != nil {
		return behavior.Approval{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return behavior.Approval{}, err
	}
	a := mapApproval(row)
	a.ExecutionID = executionID
	return a, nil
}

// expireApprovalsTx expires due approvals and records the signals of bound
// ones atomically.
func (s *BehaviorStore) expireApprovalsTx(ctx context.Context, now time.Time, limit int) ([]behavior.Approval, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	rows, err := q.ExpireApprovals(ctx, sqlc.ExpireApprovalsParams{AtTime: ts(now), MaxRows: int32(clamp(limit, 1, 1000))})
	if err != nil {
		return nil, err
	}
	out := make([]behavior.Approval, 0, len(rows))
	for _, r := range rows {
		executionID, err := recordApprovalSignalTx(ctx, q, r, now)
		if err != nil {
			return nil, err
		}
		a := mapApproval(r)
		a.ExecutionID = executionID
		out = append(out, a)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// withStepFence runs fn under the job fence of f (claim token, lease,
// execution state, cancellation, deadline, step and attempt). A refused
// fence is behavior.ErrWorkflowFence; nothing is written.
func (s *BehaviorStore) withStepFence(ctx context.Context, f behavior.RunFence, fn func(q *sqlc.Queries, ws pgtype.UUID) error) error {
	ws, err := parseDBID(f.WorkspaceID)
	if err != nil || f.JobID <= 0 || f.ExecutionID <= 0 || strings.TrimSpace(f.StepKey) == "" || f.Attempt < 1 || f.At.IsZero() {
		return invalid("run fence")
	}
	token, err := workflowJobToken(f.ClaimToken)
	if err != nil {
		return invalid("run fence")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	// Lock order everywhere is execution, then job: the wait insert's foreign
	// key would otherwise wait on an execution held by the reconciler.
	if _, err := q.LockWorkflowExecution(ctx, sqlc.LockWorkflowExecutionParams{WorkspaceID: ws, ID: f.ExecutionID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return behavior.ErrWorkflowFence
		}
		return err
	}
	if _, err := lockWorkflowFence(ctx, q, ws, token, durableFenceRequest(f)); err != nil {
		if errors.Is(err, workflow.ErrStaleClaim) {
			return behavior.ErrWorkflowFence
		}
		return err
	}
	if err := fn(q, ws); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreateWaitApproval creates a blocking approval of the fenced step's run
// and binds it to the step in one transaction.
func (s *BehaviorStore) CreateWaitApproval(ctx context.Context, f behavior.RunFence, a behavior.NewApproval) (behavior.Approval, error) {
	waitKind, ok := waitKindOf(a.Kind)
	if !ok || a.RunID <= 0 {
		return behavior.Approval{}, invalid("wait approval")
	}
	ws, err := parseDBID(f.WorkspaceID)
	if err != nil {
		return behavior.Approval{}, err
	}
	params, err := createApprovalParams(ws, a)
	if err != nil {
		return behavior.Approval{}, err
	}
	var out behavior.Approval
	err = s.withStepFence(ctx, f, func(q *sqlc.Queries, ws pgtype.UUID) error {
		execution, err := q.GetWorkflowExecution(ctx, sqlc.GetWorkflowExecutionParams{WorkspaceID: ws, ID: f.ExecutionID})
		if err != nil {
			return err
		}
		if !execution.LegacyRunID.Valid || execution.LegacyRunID.Int64 != a.RunID {
			return behavior.ErrWorkflowFence
		}
		row, err := q.CreateApproval(ctx, params)
		if err != nil {
			if isUniqueViolation(err) {
				return behavior.ErrConflict
			}
			return err
		}
		if _, err := q.InsertWorkflowWait(ctx, sqlc.InsertWorkflowWaitParams{
			WorkspaceID: ws, ExecutionID: f.ExecutionID, StepKey: f.StepKey, ApprovalID: row.ID, WaitKind: waitKind,
		}); err != nil {
			return err
		}
		out = mapApproval(row)
		out.ExecutionID = f.ExecutionID
		return nil
	})
	if err != nil {
		return behavior.Approval{}, err
	}
	return out, nil
}

// ListStepWaits lists the step's bound approvals with their signal state.
func (s *BehaviorStore) ListStepWaits(ctx context.Context, workspaceID string, executionID int64, stepKey string) ([]behavior.StepWait, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, err
	}
	if executionID <= 0 || strings.TrimSpace(stepKey) == "" {
		return nil, invalid("step")
	}
	rows, err := s.q.ListStepWaits(ctx, sqlc.ListStepWaitsParams{WorkspaceID: ws, ExecutionID: executionID, StepKey: stepKey})
	if err != nil {
		return nil, err
	}
	out := make([]behavior.StepWait, 0, len(rows))
	for _, r := range rows {
		a := mapApproval(r.Approval)
		a.ExecutionID = executionID
		out = append(out, behavior.StepWait{
			WaitID: r.WaitID, Approval: a, Input: r.WaitKind == "input",
			SignalID: r.SignalRowID, Consumed: r.ConsumedAt.Valid,
		})
	}
	return out, nil
}

// ConsumeWaitSignal marks a signal of the fenced step applied; an already
// consumed signal is a no-op.
func (s *BehaviorStore) ConsumeWaitSignal(ctx context.Context, f behavior.RunFence, signalID int64) error {
	if signalID <= 0 {
		return invalid("signal")
	}
	return s.withStepFence(ctx, f, func(q *sqlc.Queries, ws pgtype.UUID) error {
		_, err := q.ConsumeWorkflowSignal(ctx, sqlc.ConsumeWorkflowSignalParams{
			AtTime: ts(f.At), WorkspaceID: ws, ID: signalID, ExecutionID: f.ExecutionID, StepKey: f.StepKey,
		})
		return err
	})
}

// --- workflow side (WorkflowStore) ---

func lockCommandExecution(ctx context.Context, q *sqlc.Queries, ws pgtype.UUID, executionID int64) (sqlc.WorkflowExecution, error) {
	row, err := q.LockWorkflowExecution(ctx, sqlc.LockWorkflowExecutionParams{WorkspaceID: ws, ID: executionID})
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlc.WorkflowExecution{}, workflow.ErrNotFound
	}
	return row, err
}

// HandleSignal records an already-authorized decision of a bound approval
// and wakes its waiting step. The approval must belong to the workspace, be
// bound to the signal's execution and step, and already carry its decision:
// a signal never creates or decides an approval. A repeated signal (same
// signal ID, or a second signal for the same wait) is a no-op.
func (s *WorkflowStore) HandleSignal(ctx context.Context, signal workflow.Signal) error {
	ws, err := parseDBID(signal.WorkspaceID)
	if err != nil || signal.ExecutionID <= 0 || strings.TrimSpace(signal.SignalID) == "" || len(signal.SignalID) > 200 ||
		strings.TrimSpace(signal.BlockedStepKey) == "" || len(signal.IdempotencyKey) > 512 {
		return workflowInvalid("signal")
	}
	if signal.ReferenceKind != SignalReferenceApproval {
		return workflowInvalid("signal reference_kind")
	}
	approvalID, err := strconv.ParseInt(signal.ReferenceID, 10, 64)
	if err != nil || approvalID <= 0 {
		return workflowInvalid("signal reference_id")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	execution, err := lockCommandExecution(ctx, q, ws, signal.ExecutionID)
	if err != nil {
		return err
	}
	wait, err := q.GetWorkflowWaitByApproval(ctx, sqlc.GetWorkflowWaitByApprovalParams{WorkspaceID: ws, ApprovalID: approvalID})
	if errors.Is(err, pgx.ErrNoRows) {
		return workflow.ErrNotEligible
	}
	if err != nil {
		return err
	}
	if wait.ExecutionID != signal.ExecutionID || wait.StepKey != signal.BlockedStepKey {
		return workflow.ErrNotEligible
	}
	approval, err := q.GetApproval(ctx, sqlc.GetApprovalParams{WorkspaceID: ws, ID: approvalID})
	if err != nil {
		return err
	}
	if !approval.Decision.Valid || !approval.DecidedBy.Valid {
		return workflow.ErrNotEligible
	}
	if workflow.State(execution.State).IsTerminal() {
		return workflow.ErrNotEligible
	}
	if err := insertWaitSignal(ctx, q, wait, approval.Decision.String, signal.SignalID); err != nil {
		return err
	}
	if signal.IdempotencyKey != "" {
		if _, err := q.GetExecutionEventByIdempotency(ctx, sqlc.GetExecutionEventByIdempotencyParams{
			WorkspaceID: ws, ExecutionID: signal.ExecutionID, IdempotencyKey: workflowText(signal.IdempotencyKey),
		}); err == nil {
			return tx.Commit(ctx)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	if _, _, err := wakeWaitingStepTx(ctx, q, ws, signal.ExecutionID, wait.StepKey, time.Now(), ReasonSignalReceived, signal.IdempotencyKey); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ResumeExecution queues a waiting execution's step again only when a
// decision of one of its bound waits is already recorded and not yet
// applied. It never creates, decides or bypasses an approval; without such
// a decision it is ErrNotEligible. A repeated command returns the
// execution unchanged.
func (s *WorkflowStore) ResumeExecution(ctx context.Context, command workflow.Command) (workflow.Execution, error) {
	ws, err := parseDBID(command.WorkspaceID)
	if err != nil || command.ExecutionID <= 0 || strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 512 {
		return workflow.Execution{}, workflowInvalid("resume command")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return workflow.Execution{}, err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	execution, err := lockCommandExecution(ctx, q, ws, command.ExecutionID)
	if err != nil {
		return workflow.Execution{}, err
	}
	if _, err := q.GetExecutionEventByIdempotency(ctx, sqlc.GetExecutionEventByIdempotencyParams{
		WorkspaceID: ws, ExecutionID: command.ExecutionID, IdempotencyKey: workflowText(command.IdempotencyKey),
	}); err == nil {
		return mapWorkflowExecution(execution)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return workflow.Execution{}, err
	}
	if !workflow.State(execution.State).IsWaiting() {
		return workflow.Execution{}, workflow.ErrNotEligible
	}
	step, err := q.GetWaitingExecutionStep(ctx, sqlc.GetWaitingExecutionStepParams{WorkspaceID: ws, ExecutionID: command.ExecutionID})
	if errors.Is(err, pgx.ErrNoRows) {
		return workflow.Execution{}, workflow.ErrNotEligible
	}
	if err != nil {
		return workflow.Execution{}, err
	}
	if err := repairMissingSignalsTx(ctx, q, ws, command.ExecutionID); err != nil {
		return workflow.Execution{}, err
	}
	row, woke, err := wakeWaitingStepTx(ctx, q, ws, command.ExecutionID, step.StepKey, time.Now(), ReasonResumed, command.IdempotencyKey)
	if err != nil {
		return workflow.Execution{}, err
	}
	if !woke {
		return workflow.Execution{}, workflow.ErrNotEligible
	}
	if err := tx.Commit(ctx); err != nil {
		return workflow.Execution{}, err
	}
	return mapWorkflowExecution(row)
}

// ReconcileSignals repairs the approval bridge after a restart: decided
// bound approvals without a signal get one, and waiting steps with an
// unapplied signal but no live job are queued again. Live jobs, open waits
// and terminal executions are left alone. Each record is repaired in its own
// savepoint: a failing record is rolled back and reported, the rest of the
// pass still commits.
func (s *WorkflowStore) ReconcileSignals(ctx context.Context, at time.Time) error {
	if at.IsZero() {
		return workflowInvalid("reconcile time")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	var itemErrs []error
	waits, err := listMissingSignalWaits(ctx, q, pgtype.UUID{}, 0)
	if err != nil {
		return err
	}
	for i := range waits {
		wait := waits[i]
		itemErr, fatal := reconcileItem(ctx, tx, func(q *sqlc.Queries) error { return repairWaitSignalTx(ctx, q, wait) })
		if fatal != nil {
			return fatal
		}
		if itemErr != nil {
			itemErrs = append(itemErrs, fmt.Errorf("reconcile signal of execution %d: %w", wait.ExecutionID, itemErr))
		}
	}
	steps, err := q.ListWakeableWaitingSteps(ctx)
	if err != nil {
		return err
	}
	for i := range steps {
		step := steps[i]
		itemErr, fatal := reconcileItem(ctx, tx, func(q *sqlc.Queries) error {
			_, _, err := wakeWaitingStepTx(ctx, q, step.WorkspaceID, step.ExecutionID, step.StepKey, at, ReasonSignalReceived, "")
			return err
		})
		if fatal != nil {
			return fatal
		}
		if itemErr != nil {
			itemErrs = append(itemErrs, fmt.Errorf("reconcile execution %d: %w", step.ExecutionID, itemErr))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return errors.Join(itemErrs...)
}
