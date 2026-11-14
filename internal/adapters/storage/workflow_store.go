package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	q := sqlc.New(tx)
	row, err := q.InsertWorkflowExecution(ctx, sqlc.InsertWorkflowExecutionParams{
		WorkspaceID: ws, BehaviorVersionID: req.VersionID, TriggerKey: req.TriggerKey, CorrelationID: correlation,
		Definition: definition, SourceKind: req.References.SourceKind, SourceID: req.References.SourceID,
		Subject: req.References.Subject, Revision: req.References.Revision, OperationKey: req.References.OperationKey,
		LegacyRunID: workflowInt8(req.References.LegacyRunID), LegacyJobID: workflowInt8(req.References.LegacyJobID),
		DeadlineAt: func() pgtype.Timestamptz {
			if req.Deadline == nil {
				return pgtype.Timestamptz{}
			}
			return ts(*req.Deadline)
		}(),
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
			WorkspaceID: ws, ExecutionID: row.ID, StepKey: step.Key, Position: int32(position), Kind: step.Kind, DependsOn: step.DependsOn,
		}); err != nil {
			return workflow.Execution{}, err
		}
	}
	if err := insertWorkflowEvent(ctx, q, ws, row.ID, workflow.EventStarted, "", workflow.StateReceived, "", 0, workflow.ReasonAccepted, "", workflow.Usage{}); err != nil {
		return workflow.Execution{}, err
	}
	return mapWorkflowExecution(row)
}

// TransitionExecution atomically changes lifecycle state and appends history.
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
	if _, err := q.CancelExecutionSteps(ctx, sqlc.CancelExecutionStepsParams{WorkspaceID: ws, ExecutionID: command.ExecutionID}); err != nil {
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
	child, err = q.InsertRetryExecution(ctx, sqlc.InsertRetryExecutionParams{WorkspaceID: ws, ParentExecutionID: parent.ID})
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
	if err := tx.Commit(ctx); err != nil {
		return workflow.Execution{}, err
	}
	return mapWorkflowExecution(child)
}

// RecordCheckpoint atomically updates cumulative usage and appends its event.
func (s *WorkflowStore) RecordCheckpoint(ctx context.Context, req workflow.CheckpointRequest) (workflow.Step, error) {
	ws, err := parseDBID(req.WorkspaceID)
	if err != nil || req.ExecutionID <= 0 || req.ExpectedAttempt < 1 || strings.TrimSpace(req.StepKey) == "" {
		return workflow.Step{}, workflowInvalid("checkpoint request")
	}
	if req.Usage.InputTokens < 0 || req.Usage.OutputTokens < 0 || req.Usage.CacheReadTokens < 0 || req.Usage.CacheWriteTokens < 0 {
		return workflow.Step{}, workflowInvalid("usage")
	}
	checkpoint, err := workflow.MarshalCheckpoint(req.Checkpoint)
	if err != nil {
		return workflow.Step{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return workflow.Step{}, err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	execution, err := q.LockWorkflowExecution(ctx, sqlc.LockWorkflowExecutionParams{WorkspaceID: ws, ID: req.ExecutionID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return workflow.Step{}, workflow.ErrNotFound
		}
		return workflow.Step{}, err
	}
	if !workflow.State(execution.State).IsActive() {
		return workflow.Step{}, workflow.ErrStaleClaim
	}
	row, err := q.RecordExecutionCheckpoint(ctx, sqlc.RecordExecutionCheckpointParams{
		WorkspaceID: ws, ExecutionID: req.ExecutionID, StepKey: req.StepKey, ExpectedAttempt: int32(req.ExpectedAttempt),
		Checkpoint: checkpoint, InputTokens: req.Usage.InputTokens, OutputTokens: req.Usage.OutputTokens,
		CacheReadTokens: req.Usage.CacheReadTokens, CacheWriteTokens: req.Usage.CacheWriteTokens,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return workflow.Step{}, workflow.ErrStaleClaim
		}
		return workflow.Step{}, err
	}
	if err := insertWorkflowEvent(ctx, q, ws, req.ExecutionID, workflow.EventCheckpoint, workflow.State(execution.State), workflow.State(execution.State),
		req.StepKey, req.ExpectedAttempt, workflow.ReasonCheckpoint, req.IdempotencyKey, req.Usage); err != nil {
		if isUniqueViolation(err) {
			return workflow.Step{}, workflow.ErrConflict
		}
		return workflow.Step{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return workflow.Step{}, err
	}
	return mapWorkflowStep(row)
}

// Signal persistence and eligibility are introduced with the approval bridge.
func (s *WorkflowStore) HandleSignal(context.Context, workflow.Signal) error {
	return workflow.ErrNotEligible
}

// Resume cannot manufacture a signal; P5 adds the persisted eligibility path.
func (s *WorkflowStore) ResumeExecution(context.Context, workflow.Command) (workflow.Execution, error) {
	return workflow.Execution{}, workflow.ErrNotEligible
}
