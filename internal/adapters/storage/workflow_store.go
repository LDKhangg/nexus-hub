package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WorkflowStore owns execution lifecycle transactions. StartExecutionTx is
// the explicit exception for callers that must atomically accept a trigger
// and create its execution in an existing transaction; it never commits tx.
type WorkflowStore struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
}

func NewWorkflowStore(pool *pgxpool.Pool) (*WorkflowStore, error) {
	if pool == nil {
		return nil, errors.New("workflow store requires pool")
	}
	return &WorkflowStore{pool: pool, q: sqlc.New(pool)}, nil
}

var _ workflow.Store = (*WorkflowStore)(nil)

func workflowInvalid(field string) error {
	return fmt.Errorf("%w: %s", workflow.ErrInvalidInput, field)
}

func workflowText(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: value != ""}
}

func workflowInt8(value int64) pgtype.Int8 {
	return pgtype.Int8{Int64: value, Valid: value != 0}
}

func validateStartRequest(req workflow.StartRequest) (pgtype.UUID, pgtype.UUID, []byte, error) {
	ws, err := parseDBID(req.WorkspaceID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, nil, workflowInvalid("workspace_id")
	}
	correlation, err := parseDBID(req.CorrelationID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, nil, workflowInvalid("correlation_id")
	}
	if req.VersionID <= 0 || strings.TrimSpace(req.TriggerKey) == "" || len(req.TriggerKey) > 512 {
		return pgtype.UUID{}, pgtype.UUID{}, nil, workflowInvalid("version_id or trigger_key")
	}
	refs := req.References
	if strings.TrimSpace(refs.SourceKind) == "" || len(refs.SourceKind) > 64 ||
		strings.TrimSpace(refs.SourceID) == "" || len(refs.SourceID) > 512 ||
		strings.TrimSpace(refs.OperationKey) == "" || len(refs.OperationKey) > 512 ||
		len(refs.Subject) > 512 || len(refs.Revision) > 512 {
		return pgtype.UUID{}, pgtype.UUID{}, nil, workflowInvalid("references")
	}
	if err := workflow.ValidateDefinition(req.Definition, nil); err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, nil, errors.Join(workflow.ErrInvalidInput, err)
	}
	definition, err := json.Marshal(req.Definition)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, nil, workflowInvalid("definition")
	}
	return ws, correlation, definition, nil
}

func mapWorkflowExecution(row sqlc.WorkflowExecution) (workflow.Execution, error) {
	var definition workflow.Definition
	if err := json.Unmarshal(row.Definition, &definition); err != nil {
		return workflow.Execution{}, fmt.Errorf("decode workflow definition: %w", err)
	}
	return workflow.Execution{
		ID: row.ID, WorkspaceID: uuidString(row.WorkspaceID), VersionID: row.BehaviorVersionID,
		TriggerKey: row.TriggerKey, CorrelationID: uuidString(row.CorrelationID), State: workflow.State(row.State),
		Reason: workflow.ReasonCode(row.ReasonCode.String), Definition: definition,
		References: workflow.References{SourceKind: row.SourceKind, SourceID: row.SourceID, Subject: row.Subject,
			Revision: row.Revision, OperationKey: row.OperationKey, LegacyRunID: row.LegacyRunID.Int64, LegacyJobID: row.LegacyJobID.Int64},
		ParentExecutionID: row.ParentExecutionID.Int64, Deadline: tsPtr(row.DeadlineAt), CancelledAt: tsPtr(row.CancelledAt),
		StartedAt: tsPtr(row.StartedAt), FinishedAt: tsPtr(row.FinishedAt), CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
	}, nil
}

func mapWorkflowStep(row sqlc.ExecutionStep) (workflow.Step, error) {
	var checkpoint *workflow.Checkpoint
	if len(row.Checkpoint) > 0 {
		var value workflow.Checkpoint
		if err := json.Unmarshal(row.Checkpoint, &value); err != nil {
			return workflow.Step{}, fmt.Errorf("decode workflow checkpoint: %w", err)
		}
		checkpoint = &value
	}
	return workflow.Step{
		ID: row.ID, ExecutionID: row.ExecutionID, Key: row.StepKey, Position: int(row.Position), Kind: row.Kind,
		DependsOn: row.DependsOn, State: workflow.StepState(row.State), Attempts: int(row.Attempts),
		Usage: workflow.Usage{InputTokens: row.InputTokens, OutputTokens: row.OutputTokens,
			CacheReadTokens: row.CacheReadTokens, CacheWriteTokens: row.CacheWriteTokens},
		Checkpoint: checkpoint, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
	}, nil
}

func insertWorkflowEvent(ctx context.Context, q *sqlc.Queries, ws pgtype.UUID, executionID int64, kind workflow.EventKind,
	from, to workflow.State, stepKey string, attempt int, reason workflow.ReasonCode, idempotencyKey string, usage workflow.Usage) error {
	_, err := q.InsertExecutionEvent(ctx, sqlc.InsertExecutionEventParams{
		WorkspaceID: ws, ExecutionID: executionID, Kind: string(kind), FromState: workflowText(string(from)),
		ToState: workflowText(string(to)), StepKey: workflowText(stepKey), Attempt: int32(attempt),
		ReasonCode: workflowText(string(reason)), IdempotencyKey: workflowText(idempotencyKey),
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
		CacheReadTokens: usage.CacheReadTokens, CacheWriteTokens: usage.CacheWriteTokens,
	})
	return err
}

func (s *WorkflowStore) StartExecution(ctx context.Context, req workflow.StartRequest) (workflow.Execution, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return workflow.Execution{}, err
	}
	defer tx.Rollback(ctx)
	execution, err := s.StartExecutionTx(ctx, tx, req)
	if err != nil {
		return workflow.Execution{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return workflow.Execution{}, err
	}
	return execution, nil
}

// StartExecutionTx inserts or resolves one execution by workspace trigger key.
// The caller owns commit and rollback of tx.
func (s *WorkflowStore) StartExecutionTx(ctx context.Context, tx pgx.Tx, req workflow.StartRequest) (workflow.Execution, error) {
	if tx == nil {
		return workflow.Execution{}, workflowInvalid("transaction")
	}
	ws, correlation, definition, err := validateStartRequest(req)
	if err != nil {
		return workflow.Execution{}, err
	}
	at := req.At
	if at.IsZero() {
		at = time.Now()
	}
	deadline := pgtype.Timestamptz{}
	if value := startDeadline(req, at); value != nil {
		deadline = ts(*value)
	}
	q := sqlc.New(tx)
	row, err := q.InsertWorkflowExecution(ctx, sqlc.InsertWorkflowExecutionParams{
		WorkspaceID: ws, BehaviorVersionID: req.VersionID, TriggerKey: req.TriggerKey, CorrelationID: correlation,
		Definition: definition, SourceKind: req.References.SourceKind, SourceID: req.References.SourceID,
		Subject: req.References.Subject, Revision: req.References.Revision, OperationKey: req.References.OperationKey,
		LegacyRunID: workflowInt8(req.References.LegacyRunID), LegacyJobID: workflowInt8(req.References.LegacyJobID),
		DeadlineAt: deadline,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		existing, getErr := q.GetWorkflowExecutionByTrigger(ctx, sqlc.GetWorkflowExecutionByTriggerParams{WorkspaceID: ws, TriggerKey: req.TriggerKey})
		if getErr != nil {
			if errors.Is(getErr, pgx.ErrNoRows) {
				return workflow.Execution{}, workflow.ErrNotFound
			}
			return workflow.Execution{}, getErr
		}
		return mapWorkflowExecution(existing)
	}
	if err != nil {
		return workflow.Execution{}, err
	}
	for position, step := range req.Definition.Steps {
		if _, err := q.InsertExecutionStep(ctx, sqlc.InsertExecutionStepParams{
			WorkspaceID: ws, ExecutionID: row.ID, StepKey: step.Key, Position: int32(position), Kind: step.Kind, DependsOn: workflowDependsOn(step.DependsOn),
		}); err != nil {
			return workflow.Execution{}, err
		}
	}
	if err := insertWorkflowEvent(ctx, q, ws, row.ID, workflow.EventStarted, "", workflow.StateReceived, "", 0, workflow.ReasonAccepted, "", workflow.Usage{}); err != nil {
		return workflow.Execution{}, err
	}
	ready, err := q.FindReadyExecutionStep(ctx, sqlc.FindReadyExecutionStepParams{WorkspaceID: ws, ExecutionID: row.ID})
	if err != nil {
		return workflow.Execution{}, err
	}
	if _, err := q.InsertWorkflowJob(ctx, sqlc.InsertWorkflowJobParams{
		WorkspaceID: ws, ExecutionID: row.ID, StepKey: ready.StepKey, DueAt: ts(at), MaxAttempts: int32(req.Definition.Retry.MaxAttempts),
		Attempts: ready.Attempts,
	}); err != nil {
		return workflow.Execution{}, err
	}
	queued, err := q.UpdateWorkflowExecutionState(ctx, sqlc.UpdateWorkflowExecutionStateParams{
		WorkspaceID: ws, ID: row.ID, FromState: string(workflow.StateReceived), ToState: string(workflow.StateQueued),
		ReasonCode: workflowText(string(workflow.ReasonAccepted)),
	})
	if err != nil {
		return workflow.Execution{}, err
	}
	if err := insertWorkflowEvent(ctx, q, ws, row.ID, workflow.EventTransition, workflow.StateReceived, workflow.StateQueued,
		ready.StepKey, 0, workflow.ReasonAccepted, "", workflow.Usage{}); err != nil {
		return workflow.Execution{}, err
	}
	return mapWorkflowExecution(queued)
}

// startDeadline is the earlier of the explicit request deadline and the
// pinned definition's execution deadline measured from at.
func startDeadline(req workflow.StartRequest, at time.Time) *time.Time {
	var deadline *time.Time
	if req.Deadline != nil {
		value := *req.Deadline
		deadline = &value
	}
	if req.Definition.ExecutionDeadline > 0 {
		value := at.Add(req.Definition.ExecutionDeadline)
		if deadline == nil || value.Before(*deadline) {
			deadline = &value
		}
	}
	return deadline
}

// terminateExecutionWork ends the execution's unfinished steps and active job
// when it becomes terminal outside the job-result path, so no live queue row
// remains and a stale worker's claim token no longer matches.
func terminateExecutionWork(ctx context.Context, q *sqlc.Queries, ws pgtype.UUID, executionID int64, reason workflow.ReasonCode, at time.Time) error {
	if _, err := q.CancelExecutionSteps(ctx, sqlc.CancelExecutionStepsParams{WorkspaceID: ws, ExecutionID: executionID}); err != nil {
		return err
	}
	_, err := q.TerminateActiveWorkflowJobs(ctx, sqlc.TerminateActiveWorkflowJobsParams{
		Reason: workflowText(string(reason)), AtTime: ts(at), WorkspaceID: ws, ExecutionID: executionID,
	})
	return err
}

// TransitionExecution is an administrative path: it atomically changes
// lifecycle state and appends history after checking workflow.CanTransition
// and the expected from-state. It holds no job fence, so workers must use the
// JobStore methods instead. A terminal target also ends the execution's
// unfinished steps and active job in the same transaction.
func (s *WorkflowStore) TransitionExecution(ctx context.Context, req workflow.TransitionRequest) (workflow.Execution, error) {
	ws, err := parseDBID(req.WorkspaceID)
	if err != nil || req.ExecutionID <= 0 || !workflow.CanTransition(req.From, req.To) {
		return workflow.Execution{}, workflowInvalid("transition")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return workflow.Execution{}, err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	locked, err := q.LockWorkflowExecution(ctx, sqlc.LockWorkflowExecutionParams{WorkspaceID: ws, ID: req.ExecutionID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return workflow.Execution{}, workflow.ErrNotFound
		}
		return workflow.Execution{}, err
	}
	if locked.State != string(req.From) {
		return workflow.Execution{}, workflow.ErrConflict
	}
	if req.From == req.To {
		return mapWorkflowExecution(locked)
	}
	row, err := q.UpdateWorkflowExecutionState(ctx, sqlc.UpdateWorkflowExecutionStateParams{
		WorkspaceID: ws, ID: req.ExecutionID, FromState: string(req.From), ToState: string(req.To), ReasonCode: workflowText(string(req.Reason)),
	})
	if err != nil {
		return workflow.Execution{}, workflow.ErrConflict
	}
	if req.To.IsTerminal() {
		reason := req.Reason
		if reason == "" {
			reason = workflow.ReasonCode(strings.ToLower(string(req.To)))
		}
		if err := terminateExecutionWork(ctx, q, ws, row.ID, reason, time.Now()); err != nil {
			return workflow.Execution{}, err
		}
	}
	if err := insertWorkflowEvent(ctx, q, ws, row.ID, workflow.EventTransition, req.From, req.To, req.StepKey, req.Attempt, req.Reason, req.IdempotencyKey, workflow.Usage{}); err != nil {
		if isUniqueViolation(err) {
			return workflow.Execution{}, workflow.ErrConflict
		}
		return workflow.Execution{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return workflow.Execution{}, err
	}
	return mapWorkflowExecution(row)
}

func (s *WorkflowStore) CancelExecution(ctx context.Context, command workflow.Command) (workflow.Execution, error) {
	ws, err := parseDBID(command.WorkspaceID)
	if err != nil || command.ExecutionID <= 0 || strings.TrimSpace(command.IdempotencyKey) == "" {
		return workflow.Execution{}, workflowInvalid("cancel command")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return workflow.Execution{}, err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	locked, err := q.LockWorkflowExecution(ctx, sqlc.LockWorkflowExecutionParams{WorkspaceID: ws, ID: command.ExecutionID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return workflow.Execution{}, workflow.ErrNotFound
		}
		return workflow.Execution{}, err
	}
	state := workflow.State(locked.State)
	if state == workflow.StateCancelled {
		return mapWorkflowExecution(locked)
	}
	if state.IsTerminal() {
		return workflow.Execution{}, workflow.ErrNotEligible
	}
	row, err := q.UpdateWorkflowExecutionState(ctx, sqlc.UpdateWorkflowExecutionStateParams{
		WorkspaceID: ws, ID: command.ExecutionID, FromState: locked.State, ToState: string(workflow.StateCancelled), ReasonCode: workflowText(string(workflow.ReasonCancelled)),
	})
	if err != nil {
		return workflow.Execution{}, workflow.ErrConflict
	}
	if err := terminateExecutionWork(ctx, q, ws, command.ExecutionID, workflow.ReasonCancelled, time.Now()); err != nil {
		return workflow.Execution{}, err
	}
	if err := insertWorkflowEvent(ctx, q, ws, row.ID, workflow.EventTransition, state, workflow.StateCancelled, "", 0,
		workflow.ReasonCancelled, command.IdempotencyKey, workflow.Usage{}); err != nil {
		if isUniqueViolation(err) {
			return workflow.Execution{}, workflow.ErrConflict
		}
		return workflow.Execution{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return workflow.Execution{}, err
	}
	return mapWorkflowExecution(row)
}

func (s *WorkflowStore) RetryExecution(ctx context.Context, command workflow.Command) (workflow.Execution, error) {
	ws, err := parseDBID(command.WorkspaceID)
	if err != nil || command.ExecutionID <= 0 || strings.TrimSpace(command.IdempotencyKey) == "" {
		return workflow.Execution{}, workflowInvalid("retry command")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return workflow.Execution{}, err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	parent, err := q.LockWorkflowExecution(ctx, sqlc.LockWorkflowExecutionParams{WorkspaceID: ws, ID: command.ExecutionID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return workflow.Execution{}, workflow.ErrNotFound
		}
		return workflow.Execution{}, err
	}
	child, childErr := q.GetRetryChild(ctx, sqlc.GetRetryChildParams{WorkspaceID: ws, ParentExecutionID: workflowInt8(parent.ID)})
	if childErr == nil {
		return mapWorkflowExecution(child)
	}
	if !errors.Is(childErr, pgx.ErrNoRows) {
		return workflow.Execution{}, childErr
	}
	if workflow.State(parent.State) != workflow.StateFailed {
		return workflow.Execution{}, workflow.ErrNotEligible
	}
	if workflow.ReasonCode(parent.ReasonCode.String) == workflow.ReasonSideEffectUncertain {
		return workflow.Execution{}, workflow.ErrProviderUncertainty
	}
	unsafe, err := q.CountUnsafeRetrySteps(ctx, sqlc.CountUnsafeRetryStepsParams{WorkspaceID: ws, ExecutionID: parent.ID})
	if err != nil {
		return workflow.Execution{}, err
	}
	if unsafe != 0 {
		return workflow.Execution{}, workflow.ErrUnsafeRetry
	}
	var pinned workflow.Definition
	if err := json.Unmarshal(parent.Definition, &pinned); err != nil {
		return workflow.Execution{}, fmt.Errorf("decode workflow definition: %w", err)
	}
	var executionDeadline pgtype.Int8
	if pinned.ExecutionDeadline > 0 {
		executionDeadline = pgtype.Int8{Int64: pinned.ExecutionDeadline.Microseconds(), Valid: true}
	}
	child, err = q.InsertRetryExecution(ctx, sqlc.InsertRetryExecutionParams{
		WorkspaceID: ws, ParentExecutionID: parent.ID, ExecutionDeadlineUs: executionDeadline,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || isUniqueViolation(err) {
			existing, getErr := q.GetRetryChild(ctx, sqlc.GetRetryChildParams{WorkspaceID: ws, ParentExecutionID: workflowInt8(parent.ID)})
			if getErr == nil {
				return mapWorkflowExecution(existing)
			}
		}
		return workflow.Execution{}, err
	}
	if err := q.CopyRetryExecutionSteps(ctx, sqlc.CopyRetryExecutionStepsParams{WorkspaceID: ws, ParentExecutionID: parent.ID, ChildExecutionID: child.ID}); err != nil {
		return workflow.Execution{}, err
	}
	if err := insertWorkflowEvent(ctx, q, ws, child.ID, workflow.EventRetryChild, "", workflow.StateReceived, "", 0,
		workflow.ReasonManualRetry, command.IdempotencyKey, workflow.Usage{}); err != nil {
		return workflow.Execution{}, err
	}
	ready, err := q.FindReadyExecutionStep(ctx, sqlc.FindReadyExecutionStepParams{WorkspaceID: ws, ExecutionID: child.ID})
	if err != nil {
		return workflow.Execution{}, err
	}
	var definition workflow.Definition
	if err := json.Unmarshal(child.Definition, &definition); err != nil {
		return workflow.Execution{}, err
	}
	if _, err := q.InsertWorkflowJob(ctx, sqlc.InsertWorkflowJobParams{
		WorkspaceID: ws, ExecutionID: child.ID, StepKey: ready.StepKey, DueAt: ts(time.Now()),
		MaxAttempts: int32(definition.Retry.MaxAttempts), Attempts: ready.Attempts,
	}); err != nil {
		return workflow.Execution{}, err
	}
	child, err = q.UpdateWorkflowExecutionState(ctx, sqlc.UpdateWorkflowExecutionStateParams{
		WorkspaceID: ws, ID: child.ID, FromState: string(workflow.StateReceived), ToState: string(workflow.StateQueued),
		ReasonCode: workflowText(string(workflow.ReasonManualRetry)),
	})
	if err != nil {
		return workflow.Execution{}, err
	}
	if err := insertWorkflowEvent(ctx, q, ws, child.ID, workflow.EventTransition, workflow.StateReceived, workflow.StateQueued,
		ready.StepKey, 0, workflow.ReasonManualRetry, "", workflow.Usage{}); err != nil {
		return workflow.Execution{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return workflow.Execution{}, err
	}
	return mapWorkflowExecution(child)
}

// RecordCheckpoint atomically updates cumulative usage and appends its event.
// JobID, ClaimToken and At are mandatory so the legacy checkpoint seam cannot
// bypass the same lease fence used by workers.
func (s *WorkflowStore) RecordCheckpoint(ctx context.Context, req workflow.CheckpointRequest) (workflow.Step, error) {
	if req.ExecutionID <= 0 || req.ExpectedAttempt < 1 || strings.TrimSpace(req.StepKey) == "" {
		return workflow.Step{}, workflowInvalid("checkpoint request")
	}
	checkpoint := req.Checkpoint
	step, err := s.CheckpointJob(ctx, workflow.JobCheckpointRequest{
		JobID:           req.JobID,
		WorkspaceID:     req.WorkspaceID,
		ExecutionID:     req.ExecutionID,
		StepKey:         req.StepKey,
		ExpectedAttempt: req.ExpectedAttempt,
		ClaimToken:      req.ClaimToken,
		IdempotencyKey:  req.IdempotencyKey,
		At:              req.At,
		Checkpoint:      &checkpoint,
		Usage:           req.Usage,
	})
	if err != nil {
		return workflow.Step{}, err
	}
	return step, nil
}

// workflowDependsOn returns a non-nil slice so a root step stores an empty
// text[] instead of SQL NULL in the NOT NULL depends_on column.
func workflowDependsOn(deps []string) []string {
	if deps == nil {
		return []string{}
	}
	return deps
}
