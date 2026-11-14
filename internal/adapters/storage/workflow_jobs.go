package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

var _ workflow.JobStore = (*WorkflowStore)(nil)
var _ workflow.ReconcileStore = (*WorkflowStore)(nil)

func workflowUsageEmpty(usage workflow.Usage) bool {
	return usage == (workflow.Usage{})
}

func validateWorkflowUsage(usage workflow.Usage) bool {
	return usage.InputTokens >= 0 && usage.OutputTokens >= 0 && usage.CacheReadTokens >= 0 && usage.CacheWriteTokens >= 0
}

func workflowJobToken(value string) (pgtype.UUID, error) {
	token, err := parseDBID(value)
	if err != nil {
		return pgtype.UUID{}, workflowInvalid("claim_token")
	}
	return token, nil
}

func workflowActiveState(kind string) workflow.State {
	switch kind {
	case "triage", "agent.triage":
		return workflow.StateTriaging
	case "planning":
		return workflow.StatePlanning
	case "evaluation":
		return workflow.StateEvaluating
	default:
		return workflow.StateRunning
	}
}

func (s *WorkflowStore) ClaimJob(ctx context.Context, at time.Time) (*workflow.Job, error) {
	if at.IsZero() {
		return nil, workflowInvalid("claim time")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	candidate, err := q.SelectDueWorkflowJob(ctx, ts(at))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	executionRow, err := q.LockWorkflowExecution(ctx, sqlc.LockWorkflowExecutionParams{WorkspaceID: candidate.WorkspaceID, ID: candidate.ExecutionID})
	if err != nil {
		return nil, err
	}
	execution, err := mapWorkflowExecution(executionRow)
	if err != nil {
		return nil, err
	}
	if execution.State != workflow.StateQueued || execution.Deadline != nil && !execution.Deadline.After(at) {
		return nil, workflow.ErrStaleClaim
	}
	claimID, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	claimToken, err := parseDBID(claimID.String())
	if err != nil {
		return nil, err
	}
	claimed, err := q.ClaimWorkflowJobByID(ctx, sqlc.ClaimWorkflowJobByIDParams{
		ClaimToken: claimToken, LeaseExpiresAt: ts(at.Add(execution.Definition.LeaseDuration)), AtTime: ts(at),
		ID: candidate.ID, WorkspaceID: candidate.WorkspaceID,
	})
	if err != nil {
		return nil, err
	}
	// A resume job continues the step's current attempt: neither the job
	// nor the step counts another attempt, so the fence (job attempts ==
	// step attempts) holds and approval rounds keep the retry budget.
	stepRow, err := q.StartExecutionStepAttempt(ctx, sqlc.StartExecutionStepAttemptParams{
		NewAttempt: !candidate.ContinuesAttempt,
		AtTime:     ts(at), WorkspaceID: candidate.WorkspaceID, ExecutionID: candidate.ExecutionID, StepKey: candidate.StepKey,
	})
	if err != nil {
		return nil, err
	}
	active := workflowActiveState(stepRow.Kind)
	updatedExecution, err := q.UpdateWorkflowExecutionState(ctx, sqlc.UpdateWorkflowExecutionStateParams{
		WorkspaceID: candidate.WorkspaceID, ID: candidate.ExecutionID, FromState: string(workflow.StateQueued), ToState: string(active),
		ReasonCode: workflowText(string(workflow.ReasonAccepted)),
	})
	if err != nil {
		return nil, err
	}
	if err := insertWorkflowEvent(ctx, q, candidate.WorkspaceID, candidate.ExecutionID, workflow.EventTransition,
		workflow.StateQueued, active, candidate.StepKey, int(claimed.Attempts), workflow.ReasonAccepted, "", workflow.Usage{}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	execution, err = mapWorkflowExecution(updatedExecution)
	if err != nil {
		return nil, err
	}
	step, err := mapWorkflowStep(stepRow)
	if err != nil {
		return nil, err
	}
	return &workflow.Job{
		ID: claimed.ID, WorkspaceID: uuidString(claimed.WorkspaceID), ExecutionID: claimed.ExecutionID,
		StepKey: claimed.StepKey, Attempts: int(claimed.Attempts), MaxAttempts: int(claimed.MaxAttempts),
		ClaimToken: uuidString(claimed.ClaimToken), LeaseExpiresAt: claimed.LeaseExpiresAt.Time,
		Execution: execution, Step: step,
	}, nil
}

func (s *WorkflowStore) AuthorizeAction(ctx context.Context, jobID int64, workspaceID, claimToken string, at time.Time) error {
	ws, err := parseDBID(workspaceID)
	if err != nil || jobID <= 0 || at.IsZero() {
		return workflowInvalid("action authorization")
	}
	token, err := workflowJobToken(claimToken)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = sqlc.New(tx).LockWorkflowJobFence(ctx, sqlc.LockWorkflowJobFenceParams{
		WorkspaceID: ws, ID: jobID, ClaimToken: token, AtTime: ts(at),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return workflow.ErrStaleClaim
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func validateJobCheckpointRequest(req workflow.JobCheckpointRequest) (pgtype.UUID, pgtype.UUID, []byte, error) {
	ws, err := parseDBID(req.WorkspaceID)
	if err != nil || req.JobID <= 0 || req.ExecutionID <= 0 || strings.TrimSpace(req.StepKey) == "" ||
		req.ExpectedAttempt < 1 || req.At.IsZero() || !validateWorkflowUsage(req.Usage) {
		return pgtype.UUID{}, pgtype.UUID{}, nil, workflowInvalid("job result")
	}
	token, err := workflowJobToken(req.ClaimToken)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, nil, err
	}
	var checkpoint []byte
	if req.Checkpoint != nil {
		checkpoint, err = workflow.MarshalCheckpoint(*req.Checkpoint)
		if err != nil {
			return pgtype.UUID{}, pgtype.UUID{}, nil, err
		}
	}
	return ws, token, checkpoint, nil
}

func lockWorkflowFence(ctx context.Context, q *sqlc.Queries, ws, token pgtype.UUID, req workflow.JobCheckpointRequest) (sqlc.Job, error) {
	job, err := q.LockWorkflowJobFence(ctx, sqlc.LockWorkflowJobFenceParams{
		WorkspaceID: ws, ID: req.JobID, ClaimToken: token, AtTime: ts(req.At),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlc.Job{}, workflow.ErrStaleClaim
	}
	if err == nil && (job.ExecutionID != req.ExecutionID || job.StepKey != req.StepKey || job.Attempts != int32(req.ExpectedAttempt)) {
		return sqlc.Job{}, workflow.ErrStaleClaim
	}
	return job, err
}

func updateWorkflowProgress(ctx context.Context, q *sqlc.Queries, ws pgtype.UUID, job sqlc.Job, req workflow.JobCheckpointRequest, checkpoint []byte) (workflow.Step, error) {
	row, err := q.UpdateExecutionStepProgress(ctx, sqlc.UpdateExecutionStepProgressParams{
		Checkpoint: checkpoint, InputTokens: req.Usage.InputTokens, OutputTokens: req.Usage.OutputTokens,
		CacheReadTokens: req.Usage.CacheReadTokens, CacheWriteTokens: req.Usage.CacheWriteTokens,
		AtTime: ts(req.At), WorkspaceID: ws, ExecutionID: job.ExecutionID, StepKey: job.StepKey, ExpectedAttempt: job.Attempts,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return workflow.Step{}, workflow.ErrStaleClaim
	}
	if isCheckpointSizeViolation(err) {
		return workflow.Step{}, workflow.ErrInvalidInput
	}
	if err != nil {
		return workflow.Step{}, err
	}
	if len(checkpoint) != 0 || !workflowUsageEmpty(req.Usage) {
		if err := insertWorkflowEvent(ctx, q, ws, job.ExecutionID, workflow.EventCheckpoint, workflow.StateRunning,
			workflow.StateRunning, job.StepKey, int(job.Attempts), workflow.ReasonCheckpoint, req.IdempotencyKey, req.Usage); err != nil {
			if isUniqueViolation(err) {
				return workflow.Step{}, workflow.ErrConflict
			}
			return workflow.Step{}, err
		}
	}
	return mapWorkflowStep(row)
}

func (s *WorkflowStore) CheckpointJob(ctx context.Context, req workflow.JobCheckpointRequest) (workflow.Step, error) {
	ws, token, checkpoint, err := validateJobCheckpointRequest(req)
	if err != nil {
		return workflow.Step{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return workflow.Step{}, err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	job, err := lockWorkflowFence(ctx, q, ws, token, req)
	if err != nil {
		return workflow.Step{}, err
	}
	step, err := updateWorkflowProgress(ctx, q, ws, job, req, checkpoint)
	if err != nil {
		return workflow.Step{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return workflow.Step{}, err
	}
	return step, nil
}

type workflowJobTx struct {
	tx         pgx.Tx
	q          *sqlc.Queries
	ws         pgtype.UUID
	token      pgtype.UUID
	job        sqlc.Job
	execution  sqlc.WorkflowExecution
	checkpoint []byte
}

func (s *WorkflowStore) beginJobResult(ctx context.Context, req workflow.JobResultRequest) (*workflowJobTx, error) {
	ws, token, checkpoint, err := validateJobCheckpointRequest(req.JobCheckpointRequest)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	q := sqlc.New(tx)
	// Lock order everywhere is execution, then job: the reconciler and
	// operator commands hold the execution before touching its job.
	execution, err := q.LockWorkflowExecution(ctx, sqlc.LockWorkflowExecutionParams{WorkspaceID: ws, ID: req.ExecutionID})
	if err != nil {
		tx.Rollback(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, workflow.ErrStaleClaim
		}
		return nil, err
	}
	job, err := lockWorkflowFence(ctx, q, ws, token, req.JobCheckpointRequest)
	if err != nil {
		tx.Rollback(ctx)
		return nil, err
	}
	return &workflowJobTx{tx: tx, q: q, ws: ws, token: token, job: job, execution: execution, checkpoint: checkpoint}, nil
}

func (j *workflowJobTx) rollback(ctx context.Context) { _ = j.tx.Rollback(ctx) }

func (j *workflowJobTx) progress(ctx context.Context, req workflow.JobResultRequest) error {
	_, err := updateWorkflowProgress(ctx, j.q, j.ws, j.job, req.JobCheckpointRequest, j.checkpoint)
	return err
}

func transitionWorkflowJobExecution(ctx context.Context, j *workflowJobTx, to workflow.State, reason workflow.ReasonCode) (sqlc.WorkflowExecution, error) {
	from := workflow.State(j.execution.State)
	row, err := j.q.UpdateWorkflowExecutionState(ctx, sqlc.UpdateWorkflowExecutionStateParams{
		WorkspaceID: j.ws, ID: j.job.ExecutionID, FromState: j.execution.State, ToState: string(to), ReasonCode: workflowText(string(reason)),
	})
	if err != nil {
		return sqlc.WorkflowExecution{}, err
	}
	if err := insertWorkflowEvent(ctx, j.q, j.ws, j.job.ExecutionID, workflow.EventTransition, from, to,
		j.job.StepKey, int(j.job.Attempts), reason, "", workflow.Usage{}); err != nil {
		return sqlc.WorkflowExecution{}, err
	}
	j.execution = row
	return row, nil
}

func (s *WorkflowStore) CompleteJob(ctx context.Context, req workflow.JobResultRequest) error {
	j, err := s.beginJobResult(ctx, req)
	if err != nil {
		return err
	}
	defer j.rollback(ctx)
	if err := j.progress(ctx, req); err != nil {
		return err
	}
	if _, err := j.q.SetExecutionStepState(ctx, sqlc.SetExecutionStepStateParams{
		State: string(workflow.StepSucceeded), AtTime: ts(req.At), WorkspaceID: j.ws,
		ExecutionID: j.job.ExecutionID, StepKey: j.job.StepKey,
	}); err != nil {
		return err
	}
	if _, err := j.q.FinishWorkflowJob(ctx, sqlc.FinishWorkflowJobParams{
		Status: "completed", AtTime: ts(req.At), WorkspaceID: j.ws, ID: j.job.ID, ClaimToken: j.token,
	}); err != nil {
		return err
	}
	ready, readyErr := j.q.FindReadyExecutionStep(ctx, sqlc.FindReadyExecutionStepParams{WorkspaceID: j.ws, ExecutionID: j.job.ExecutionID})
	if readyErr == nil {
		if _, err := j.q.InsertWorkflowJob(ctx, sqlc.InsertWorkflowJobParams{
			WorkspaceID: j.ws, ExecutionID: j.job.ExecutionID, StepKey: ready.StepKey,
			DueAt: ts(req.At), MaxAttempts: j.job.MaxAttempts, Attempts: ready.Attempts,
		}); err != nil {
			return err
		}
		if _, err := transitionWorkflowJobExecution(ctx, j, workflow.StateQueued, workflow.ReasonStepSucceeded); err != nil {
			return err
		}
	} else if errors.Is(readyErr, pgx.ErrNoRows) {
		unfinished, err := j.q.CountUnfinishedExecutionSteps(ctx, sqlc.CountUnfinishedExecutionStepsParams{WorkspaceID: j.ws, ExecutionID: j.job.ExecutionID})
		if err != nil {
			return err
		}
		if unfinished != 0 {
			return workflow.ErrConflict
		}
		if err := closeExecutionWaits(ctx, j.q, j.ws, j.job.ExecutionID, req.At); err != nil {
			return err
		}
		if _, err := transitionWorkflowJobExecution(ctx, j, workflow.StateCompleted, workflow.ReasonStepSucceeded); err != nil {
			return err
		}
	} else {
		return readyErr
	}
	return j.tx.Commit(ctx)
}

func (s *WorkflowStore) WaitJob(ctx context.Context, req workflow.JobResultRequest) error {
	if !req.WaitState.IsWaiting() || len(req.WaitReferences) == 0 {
		return workflowInvalid("wait result")
	}
	checkpoint := workflow.Checkpoint{}
	if req.Checkpoint != nil {
		checkpoint = *req.Checkpoint
	}
	checkpoint.References = append([]workflow.CheckpointReference(nil), req.WaitReferences...)
	req.Checkpoint = &checkpoint
	j, err := s.beginJobResult(ctx, req)
	if err != nil {
		return err
	}
	defer j.rollback(ctx)
	if err := j.progress(ctx, req); err != nil {
		return err
	}
	if _, err := j.q.SetExecutionStepState(ctx, sqlc.SetExecutionStepStateParams{
		State: string(workflow.StepWaiting), AtTime: ts(req.At), WorkspaceID: j.ws,
		ExecutionID: j.job.ExecutionID, StepKey: j.job.StepKey,
	}); err != nil {
		return err
	}
	if _, err := j.q.FinishWorkflowJob(ctx, sqlc.FinishWorkflowJobParams{
		Status: "completed", AtTime: ts(req.At), WorkspaceID: j.ws, ID: j.job.ID, ClaimToken: j.token,
	}); err != nil {
		return err
	}
	if _, err := transitionWorkflowJobExecution(ctx, j, req.WaitState, workflow.ReasonStepWaiting); err != nil {
		return err
	}
	// A decision recorded while the step ran is not lost: the step is
	// queued again at once instead of waiting for it.
	if _, _, err := wakeWaitingStepTx(ctx, j.q, j.ws, j.job.ExecutionID, j.job.StepKey, req.At, ReasonSignalReceived, ""); err != nil {
		return err
	}
	return j.tx.Commit(ctx)
}

func (s *WorkflowStore) RetryJob(ctx context.Context, req workflow.JobResultRequest) error {
	if req.RetryAt.IsZero() || req.RetryAt.Before(req.At) {
		return workflowInvalid("retry time")
	}
	j, err := s.beginJobResult(ctx, req)
	if err != nil {
		return err
	}
	defer j.rollback(ctx)
	if err := j.progress(ctx, req); err != nil {
		return err
	}
	if _, err := j.q.SetExecutionStepState(ctx, sqlc.SetExecutionStepStateParams{
		State: string(workflow.StepPending), AtTime: ts(req.At), WorkspaceID: j.ws,
		ExecutionID: j.job.ExecutionID, StepKey: j.job.StepKey,
	}); err != nil {
		return err
	}
	if _, err := j.q.RetryWorkflowJob(ctx, sqlc.RetryWorkflowJobParams{
		DueAt: ts(req.RetryAt), ErrorCode: workflowText(string(req.Reason)), AtTime: ts(req.At),
		WorkspaceID: j.ws, ID: j.job.ID, ClaimToken: j.token,
	}); err != nil {
		return err
	}
	if _, err := transitionWorkflowJobExecution(ctx, j, workflow.StateQueued, workflow.ReasonStepRetry); err != nil {
		return err
	}
	return j.tx.Commit(ctx)
}

func (s *WorkflowStore) FailJob(ctx context.Context, req workflow.JobResultRequest) error {
	j, err := s.beginJobResult(ctx, req)
	if err != nil {
		return err
	}
	defer j.rollback(ctx)
	if err := j.progress(ctx, req); err != nil {
		return err
	}
	if _, err := j.q.SetExecutionStepState(ctx, sqlc.SetExecutionStepStateParams{
		State: string(workflow.StepFailed), AtTime: ts(req.At), WorkspaceID: j.ws,
		ExecutionID: j.job.ExecutionID, StepKey: j.job.StepKey,
	}); err != nil {
		return err
	}
	if _, err := j.q.FinishWorkflowJob(ctx, sqlc.FinishWorkflowJobParams{
		Status: "dead_letter", ErrorCode: workflowText(string(req.Reason)), DeadLetterReason: workflowText(string(req.Reason)),
		AtTime: ts(req.At), WorkspaceID: j.ws, ID: j.job.ID, ClaimToken: j.token,
	}); err != nil {
		return err
	}
	if _, err := j.q.CancelExecutionSteps(ctx, sqlc.CancelExecutionStepsParams{WorkspaceID: j.ws, ExecutionID: j.job.ExecutionID}); err != nil {
		return err
	}
	if err := closeExecutionWaits(ctx, j.q, j.ws, j.job.ExecutionID, req.At); err != nil {
		return err
	}
	if _, err := transitionWorkflowJobExecution(ctx, j, workflow.StateFailed, req.Reason); err != nil {
		return err
	}
	return j.tx.Commit(ctx)
}

// errReconcileIllegalTransition marks a repair the transition matrix forbids;
// the reconciler never fakes an intermediate state to reach its target.
var errReconcileIllegalTransition = errors.New("workflow: reconcile transition not allowed")

func reconcileTransition(ctx context.Context, q *sqlc.Queries, ws pgtype.UUID, execution sqlc.WorkflowExecution,
	to workflow.State, stepKey string, attempt int, reason workflow.ReasonCode) (sqlc.WorkflowExecution, error) {
	from := workflow.State(execution.State)
	if from == to {
		return execution, nil
	}
	if !workflow.CanTransition(from, to) {
		return sqlc.WorkflowExecution{}, fmt.Errorf("%w: %s to %s", errReconcileIllegalTransition, from, to)
	}
	return transitionExecutionTx(ctx, q, ws, execution, to, stepKey, attempt, reason, "")
}

// transitionExecutionTx moves a locked execution to state to when the
// transition matrix allows it and appends the transition event.
func transitionExecutionTx(ctx context.Context, q *sqlc.Queries, ws pgtype.UUID, execution sqlc.WorkflowExecution,
	to workflow.State, stepKey string, attempt int, reason workflow.ReasonCode, idempotencyKey string) (sqlc.WorkflowExecution, error) {
	from := workflow.State(execution.State)
	if !workflow.CanTransition(from, to) {
		return sqlc.WorkflowExecution{}, fmt.Errorf("%w: %s to %s", errReconcileIllegalTransition, from, to)
	}
	row, err := q.UpdateWorkflowExecutionState(ctx, sqlc.UpdateWorkflowExecutionStateParams{
		WorkspaceID: ws, ID: execution.ID, FromState: execution.State, ToState: string(to), ReasonCode: workflowText(string(reason)),
	})
	if err != nil {
		return sqlc.WorkflowExecution{}, err
	}
	if err := insertWorkflowEvent(ctx, q, ws, execution.ID, workflow.EventTransition, from, to,
		stepKey, attempt, reason, idempotencyKey, workflow.Usage{}); err != nil {
		return sqlc.WorkflowExecution{}, err
	}
	return row, nil
}

func reconcileDelay(definition workflow.Definition, attempts int) (time.Duration, bool) {
	if attempts >= definition.Retry.MaxAttempts {
		return 0, false
	}
	index := attempts - 1
	if index < 0 {
		return 0, true
	}
	if index >= len(definition.Retry.Delays) {
		return 0, false
	}
	return definition.Retry.Delays[index], true
}

// failReconcileExecution fails the execution and, in the same transaction,
// marks the given step failed, cancels unfinished steps and dead-letters any
// active job so no live queue row or valid claim remains.
func failReconcileExecution(ctx context.Context, q *sqlc.Queries, execution sqlc.WorkflowExecution,
	stepKey string, attempt int, at time.Time, reason workflow.ReasonCode) error {
	ws := execution.WorkspaceID
	if stepKey != "" {
		if _, err := q.SetExecutionStepState(ctx, sqlc.SetExecutionStepStateParams{
			State: string(workflow.StepFailed), AtTime: ts(at), WorkspaceID: ws, ExecutionID: execution.ID, StepKey: stepKey,
		}); err != nil {
			return err
		}
	}
	if err := terminateExecutionWork(ctx, q, ws, execution.ID, reason, at); err != nil {
		return err
	}
	_, err := reconcileTransition(ctx, q, ws, execution, workflow.StateFailed, stepKey, attempt, reason)
	return err
}

// reconcileItem runs one repair inside a savepoint so a bad record rolls back
// only its own changes. A savepoint failure is fatal for the whole pass.
func reconcileItem(ctx context.Context, tx pgx.Tx, repair func(*sqlc.Queries) error) (itemErr, fatal error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := repair(sqlc.New(sp)); err != nil {
		if rollbackErr := sp.Rollback(ctx); rollbackErr != nil {
			return nil, errors.Join(err, rollbackErr)
		}
		return err, nil
	}
	if err := sp.Commit(ctx); err != nil {
		return nil, err
	}
	return nil, nil
}

// reconcileExpiredJob recovers one job whose lease expired. The job and its
// execution are already locked by ListExpiredWorkflowJobs.
func reconcileExpiredJob(ctx context.Context, q *sqlc.Queries, job sqlc.Job, at time.Time) error {
	executionRow, err := q.LockWorkflowExecution(ctx, sqlc.LockWorkflowExecutionParams{WorkspaceID: job.WorkspaceID, ID: job.ExecutionID})
	if err != nil {
		return err
	}
	execution, err := mapWorkflowExecution(executionRow)
	if err != nil {
		return err
	}
	if execution.Deadline != nil && !execution.Deadline.After(at) {
		return failReconcileExecution(ctx, q, executionRow, job.StepKey, int(job.Attempts), at, workflow.ReasonDeadlineExceeded)
	}
	delay, retry := reconcileDelay(execution.Definition, int(job.Attempts))
	if !retry {
		return failReconcileExecution(ctx, q, executionRow, job.StepKey, int(job.Attempts), at, workflow.ReasonAttemptsExhausted)
	}
	if !workflow.CanTransition(workflow.State(executionRow.State), workflow.StateQueued) {
		return fmt.Errorf("%w: %s to %s", errReconcileIllegalTransition, executionRow.State, workflow.StateQueued)
	}
	if _, err := q.SetExecutionStepState(ctx, sqlc.SetExecutionStepStateParams{
		State: string(workflow.StepPending), AtTime: ts(at), WorkspaceID: job.WorkspaceID,
		ExecutionID: job.ExecutionID, StepKey: job.StepKey,
	}); err != nil {
		return err
	}
	if _, err := q.ResetExpiredWorkflowJob(ctx, sqlc.ResetExpiredWorkflowJobParams{
		DueAt: ts(at.Add(delay)), ErrorCode: workflowText(string(workflow.ReasonLeaseExpired)), AtTime: ts(at),
		ID: job.ID, WorkspaceID: job.WorkspaceID,
	}); err != nil {
		return err
	}
	_, err = reconcileTransition(ctx, q, job.WorkspaceID, executionRow, workflow.StateQueued,
		job.StepKey, int(job.Attempts), workflow.ReasonLeaseExpired)
	return err
}

// reconcileExecution repairs one nonterminal execution that is past its
// deadline or has neither a live queue row nor a durable wait. The execution
// is already locked by ListReconcileWorkflowExecutions; its job is locked
// second, matching the execution-then-job order of every other path.
func reconcileExecution(ctx context.Context, q *sqlc.Queries, executionRow sqlc.WorkflowExecution, at time.Time) error {
	execution, err := mapWorkflowExecution(executionRow)
	if err != nil {
		return err
	}
	activeJob, activeErr := q.GetActiveWorkflowJobForExecution(ctx, sqlc.GetActiveWorkflowJobForExecutionParams{
		WorkspaceID: executionRow.WorkspaceID, ExecutionID: executionRow.ID,
	})
	if activeErr != nil && !errors.Is(activeErr, pgx.ErrNoRows) {
		return activeErr
	}
	if execution.Deadline != nil && !execution.Deadline.After(at) {
		stepKey, attempt := activeJob.StepKey, int(activeJob.Attempts)
		if activeErr != nil && execution.State.IsWaiting() {
			// A wait past the deadline fails its waiting step; its bound
			// waits are closed with the execution's work, so a later decision
			// is recorded on the approval but never applied.
			waiting, err := q.GetWaitingExecutionStep(ctx, sqlc.GetWaitingExecutionStepParams{
				WorkspaceID: executionRow.WorkspaceID, ExecutionID: executionRow.ID,
			})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if err == nil {
				stepKey, attempt = waiting.StepKey, int(waiting.Attempts)
			}
		}
		return failReconcileExecution(ctx, q, executionRow, stepKey, attempt, at, workflow.ReasonDeadlineExceeded)
	}
	if activeErr == nil || execution.State.IsWaiting() {
		// A live job, or a durable wait (woken by ReconcileSignals).
		return nil
	}
	step, stepErr := q.FindReconcileExecutionStep(ctx, sqlc.FindReconcileExecutionStepParams{
		WorkspaceID: executionRow.WorkspaceID, ExecutionID: executionRow.ID,
	})
	if errors.Is(stepErr, pgx.ErrNoRows) {
		unfinished, countErr := q.CountUnfinishedExecutionSteps(ctx, sqlc.CountUnfinishedExecutionStepsParams{
			WorkspaceID: executionRow.WorkspaceID, ExecutionID: executionRow.ID,
		})
		if countErr != nil {
			return countErr
		}
		if unfinished != 0 {
			return failReconcileExecution(ctx, q, executionRow, "", 0, at, workflow.ReasonNonRetryable)
		}
		if workflow.CanTransition(workflow.State(executionRow.State), workflow.StateCompleted) {
			_, err := reconcileTransition(ctx, q, executionRow.WorkspaceID, executionRow,
				workflow.StateCompleted, "", 0, workflow.ReasonStepSucceeded)
			return err
		}
		// Every step succeeded but the execution never reached an active
		// state (for example QUEUED): completing it would skip the matrix,
		// so fail it with a reason that records the inconsistency.
		return failReconcileExecution(ctx, q, executionRow, "", 0, at, workflow.ReasonInconsistentState)
	}
	if stepErr != nil {
		return stepErr
	}
	if !workflow.CanTransition(workflow.State(executionRow.State), workflow.StateQueued) {
		return fmt.Errorf("%w: %s to %s", errReconcileIllegalTransition, executionRow.State, workflow.StateQueued)
	}
	dueAt := at
	// A pending step woken by a recorded, unapplied decision lost its resume
	// job: the recreated job resumes the same attempt again, so the lost
	// wakeup neither consumes nor exhausts the retry budget.
	resume := false
	if workflow.StepState(step.State) == workflow.StepPending && step.Attempts >= 1 {
		pending, err := q.CountPendingStepSignals(ctx, sqlc.CountPendingStepSignalsParams{
			WorkspaceID: executionRow.WorkspaceID, ExecutionID: executionRow.ID, StepKey: step.StepKey,
		})
		if err != nil {
			return err
		}
		resume = pending > 0
	}
	if !resume && workflow.StepState(step.State) != workflow.StepRunning && int(step.Attempts) >= execution.Definition.Retry.MaxAttempts {
		// A pending step whose job was lost after its last allowed attempt
		// cannot be claimed again; fail instead of restarting the count.
		return failReconcileExecution(ctx, q, executionRow, step.StepKey, int(step.Attempts), at, workflow.ReasonAttemptsExhausted)
	}
	if workflow.StepState(step.State) == workflow.StepRunning {
		delay, retry := reconcileDelay(execution.Definition, int(step.Attempts))
		if !retry {
			return failReconcileExecution(ctx, q, executionRow, step.StepKey, int(step.Attempts), at, workflow.ReasonAttemptsExhausted)
		}
		dueAt = at.Add(delay)
		if _, err := q.SetExecutionStepState(ctx, sqlc.SetExecutionStepStateParams{
			State: string(workflow.StepPending), AtTime: ts(at), WorkspaceID: executionRow.WorkspaceID,
			ExecutionID: executionRow.ID, StepKey: step.StepKey,
		}); err != nil {
			return err
		}
	}
	if _, err := q.InsertWorkflowJob(ctx, sqlc.InsertWorkflowJobParams{
		WorkspaceID: executionRow.WorkspaceID, ExecutionID: executionRow.ID, StepKey: step.StepKey,
		DueAt: ts(dueAt), MaxAttempts: int32(execution.Definition.Retry.MaxAttempts),
		// The recreated job continues the step's attempt numbering so the
		// claim fence (job attempts == step attempts) and max_attempts hold.
		Attempts: step.Attempts, ContinuesAttempt: resume,
	}); err != nil {
		return err
	}
	_, err = reconcileTransition(ctx, q, executionRow.WorkspaceID, executionRow,
		workflow.StateQueued, step.StepKey, int(step.Attempts), workflow.ReasonLeaseExpired)
	return err
}

// ReconcileJobs recovers expired claims and repairs nonterminal executions
// that have neither a live queue row nor a durable wait. Each record is
// repaired in its own savepoint: a failing record is rolled back and
// reported, the rest of the pass still commits.
func (s *WorkflowStore) ReconcileJobs(ctx context.Context, at time.Time) error {
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
	expired, err := q.ListExpiredWorkflowJobs(ctx, ts(at))
	if err != nil {
		return err
	}
	for i := range expired {
		job := expired[i]
		itemErr, fatal := reconcileItem(ctx, tx, func(q *sqlc.Queries) error { return reconcileExpiredJob(ctx, q, job, at) })
		if fatal != nil {
			return fatal
		}
		if itemErr != nil {
			itemErrs = append(itemErrs, fmt.Errorf("reconcile execution %d: %w", job.ExecutionID, itemErr))
		}
	}
	executions, err := q.ListReconcileWorkflowExecutions(ctx, ts(at))
	if err != nil {
		return err
	}
	for i := range executions {
		executionRow := executions[i]
		itemErr, fatal := reconcileItem(ctx, tx, func(q *sqlc.Queries) error { return reconcileExecution(ctx, q, executionRow, at) })
		if fatal != nil {
			return fatal
		}
		if itemErr != nil {
			itemErrs = append(itemErrs, fmt.Errorf("reconcile execution %d: %w", executionRow.ID, itemErr))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	// Signal repair runs after the job pass committed, even when some job
	// records failed, so one bad record never blocks waking other steps.
	if err := s.ReconcileSignals(ctx, at); err != nil {
		itemErrs = append(itemErrs, err)
	}
	return errors.Join(itemErrs...)
}

// isCheckpointSizeViolation reports the execution_steps checkpoint CHECK
// (jsonb object within 64 KiB). Go validates first; the database bound
// measures the stored jsonb size, which can exceed the JSON text length, so
// a late rejection is still the caller's invalid input, not a server error.
func isCheckpointSizeViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23514" && pg.ConstraintName == "execution_steps_checkpoint_check"
}
