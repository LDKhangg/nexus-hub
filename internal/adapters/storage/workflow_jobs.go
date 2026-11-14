package storage

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	stepRow, err := q.StartExecutionStepAttempt(ctx, sqlc.StartExecutionStepAttemptParams{
		AtTime: ts(at), WorkspaceID: candidate.WorkspaceID, ExecutionID: candidate.ExecutionID, StepKey: candidate.StepKey,
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
	job, err := lockWorkflowFence(ctx, q, ws, token, req.JobCheckpointRequest)
	if err != nil {
		tx.Rollback(ctx)
		return nil, err
	}
	execution, err := q.LockWorkflowExecution(ctx, sqlc.LockWorkflowExecutionParams{WorkspaceID: ws, ID: job.ExecutionID})
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
			DueAt: ts(req.At), MaxAttempts: j.job.MaxAttempts,
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
	if _, err := transitionWorkflowJobExecution(ctx, j, workflow.StateFailed, req.Reason); err != nil {
		return err
	}
	return j.tx.Commit(ctx)
}

func reconcileTransition(ctx context.Context, q *sqlc.Queries, ws pgtype.UUID, execution sqlc.WorkflowExecution,
	to workflow.State, stepKey string, attempt int, reason workflow.ReasonCode) (sqlc.WorkflowExecution, error) {
	from := workflow.State(execution.State)
	if from == to {
		return execution, nil
	}
	row, err := q.UpdateWorkflowExecutionState(ctx, sqlc.UpdateWorkflowExecutionStateParams{
		WorkspaceID: ws, ID: execution.ID, FromState: execution.State, ToState: string(to), ReasonCode: workflowText(string(reason)),
	})
	if err != nil {
		return sqlc.WorkflowExecution{}, err
	}
	if err := insertWorkflowEvent(ctx, q, ws, execution.ID, workflow.EventTransition, from, to,
		stepKey, attempt, reason, "", workflow.Usage{}); err != nil {
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

func failReconcileExecution(ctx context.Context, q *sqlc.Queries, execution sqlc.WorkflowExecution,
	job *sqlc.Job, stepKey string, attempt int, at time.Time, reason workflow.ReasonCode) error {
	ws := execution.WorkspaceID
	if stepKey != "" {
		if _, err := q.SetExecutionStepState(ctx, sqlc.SetExecutionStepStateParams{
			State: string(workflow.StepFailed), AtTime: ts(at), WorkspaceID: ws, ExecutionID: execution.ID, StepKey: stepKey,
		}); err != nil {
			return err
		}
	}
	if job != nil {
		if _, err := q.DeadLetterWorkflowJobByID(ctx, sqlc.DeadLetterWorkflowJobByIDParams{
			Reason: workflowText(string(reason)), AtTime: ts(at), ID: job.ID, WorkspaceID: ws,
		}); err != nil {
			return err
		}
	}
	if _, err := q.CancelExecutionSteps(ctx, sqlc.CancelExecutionStepsParams{WorkspaceID: ws, ExecutionID: execution.ID}); err != nil {
		return err
	}
	_, err := reconcileTransition(ctx, q, ws, execution, workflow.StateFailed, stepKey, attempt, reason)
	return err
}

// ReconcileJobs recovers expired claims and repairs nonterminal executions
// that have neither a live queue row nor a durable wait.
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
	expired, err := q.ListExpiredWorkflowJobs(ctx, ts(at))
	if err != nil {
		return err
	}
	for i := range expired {
		job := expired[i]
		executionRow, err := q.LockWorkflowExecution(ctx, sqlc.LockWorkflowExecutionParams{WorkspaceID: job.WorkspaceID, ID: job.ExecutionID})
		if err != nil {
			return err
		}
		execution, err := mapWorkflowExecution(executionRow)
		if err != nil {
			return err
		}
		if execution.Deadline != nil && !execution.Deadline.After(at) {
			if err := failReconcileExecution(ctx, q, executionRow, &job, job.StepKey, int(job.Attempts), at, workflow.ReasonDeadlineExceeded); err != nil {
				return err
			}
			continue
		}
		delay, retry := reconcileDelay(execution.Definition, int(job.Attempts))
		if !retry {
			if err := failReconcileExecution(ctx, q, executionRow, &job, job.StepKey, int(job.Attempts), at, workflow.ReasonAttemptsExhausted); err != nil {
				return err
			}
			continue
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
		if _, err := reconcileTransition(ctx, q, job.WorkspaceID, executionRow, workflow.StateQueued,
			job.StepKey, int(job.Attempts), workflow.ReasonLeaseExpired); err != nil {
			return err
		}
	}

	executions, err := q.ListReconcileWorkflowExecutions(ctx, ts(at))
	if err != nil {
		return err
	}
	for i := range executions {
		executionRow := executions[i]
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
			var job *sqlc.Job
			if activeErr == nil {
				job = &activeJob
			}
			if err := failReconcileExecution(ctx, q, executionRow, job, activeJob.StepKey, int(activeJob.Attempts), at, workflow.ReasonDeadlineExceeded); err != nil {
				return err
			}
			continue
		}
		if activeErr == nil {
			continue
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
			if unfinished == 0 {
				if _, err := reconcileTransition(ctx, q, executionRow.WorkspaceID, executionRow,
					workflow.StateCompleted, "", 0, workflow.ReasonStepSucceeded); err != nil {
					return err
				}
				continue
			}
			if err := failReconcileExecution(ctx, q, executionRow, nil, "", 0, at, workflow.ReasonNonRetryable); err != nil {
				return err
			}
			continue
		}
		if stepErr != nil {
			return stepErr
		}
		dueAt := at
		if workflow.StepState(step.State) == workflow.StepRunning {
			delay, retry := reconcileDelay(execution.Definition, int(step.Attempts))
			if !retry {
				if err := failReconcileExecution(ctx, q, executionRow, nil, step.StepKey,
					int(step.Attempts), at, workflow.ReasonAttemptsExhausted); err != nil {
					return err
				}
				continue
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
		}); err != nil {
			return err
		}
		if _, err := reconcileTransition(ctx, q, executionRow.WorkspaceID, executionRow,
			workflow.StateQueued, step.StepKey, int(step.Attempts), workflow.ReasonLeaseExpired); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
