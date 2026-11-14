package storage

import (
	"context"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/jackc/pgx/v5/pgtype"
)

// Workspace-scoped operator reads (workflow.Reader). They select
// identifiers, states, reason codes, counters and times only; approval
// payloads, options and answers are never read.

var _ workflow.Reader = (*WorkflowStore)(nil)

func readWorkspace(id string) (pgtype.UUID, error) {
	ws, err := parseDBID(id)
	if err != nil {
		return pgtype.UUID{}, workflowInvalid("workspace_id")
	}
	return ws, nil
}

// pageNext trims a limit+1 page and returns the next cursor value.
func pageNext(n, limit int, last func() int64) (int, int64) {
	if n > limit {
		return limit, last()
	}
	return n, 0
}

// CheckWorkspace returns workflow.ErrNotFound when the workspace does not exist.
func (s *WorkflowStore) CheckWorkspace(ctx context.Context, workspaceID string) error {
	ws, err := readWorkspace(workspaceID)
	if err != nil {
		return err
	}
	if _, err := s.q.GetWorkspaceByID(ctx, ws); err != nil {
		if isNoRows(err) {
			return workflow.ErrNotFound
		}
		return err
	}
	return nil
}

func mapExecutionView(row sqlc.WorkflowExecution, name string, version int32) (workflow.ExecutionView, error) {
	e, err := mapWorkflowExecution(row)
	if err != nil {
		return workflow.ExecutionView{}, err
	}
	return workflow.ExecutionView{Execution: e, BehaviorName: name, BehaviorVersion: int(version)}, nil
}

func mapJobView(row sqlc.Job) workflow.JobView {
	return workflow.JobView{
		ID: row.ID, ExecutionID: row.ExecutionID, StepKey: row.StepKey, Status: row.Status, DueAt: row.DueAt.Time,
		Attempts: int(row.Attempts), MaxAttempts: int(row.MaxAttempts), LeaseExpiresAt: tsPtr(row.LeaseExpiresAt),
		LastErrorCode: row.LastErrorCode.String, DeadLetterReason: row.DeadLetterReason.String,
		CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
	}
}

// ListExecutions returns a page of the workspace's executions, newest first.
func (s *WorkflowStore) ListExecutions(ctx context.Context, q workflow.ExecutionQuery) ([]workflow.ExecutionView, int64, error) {
	ws, err := readWorkspace(q.WorkspaceID)
	if err != nil {
		return nil, 0, err
	}
	if !workflow.ValidListLimit(q.Limit) || q.BeforeID < 0 || (q.State != "" && !q.State.Valid()) {
		return nil, 0, workflowInvalid("execution query")
	}
	rows, err := s.q.ListWorkflowExecutionsPage(ctx, sqlc.ListWorkflowExecutionsPageParams{
		WorkspaceID: ws, State: workflowText(string(q.State)), BeforeID: workflowInt8(q.BeforeID), RowLimit: int32(q.Limit + 1),
	})
	if err != nil {
		return nil, 0, err
	}
	n, next := pageNext(len(rows), q.Limit, func() int64 { return rows[q.Limit-1].WorkflowExecution.ID })
	out := make([]workflow.ExecutionView, 0, n)
	for _, r := range rows[:n] {
		v, err := mapExecutionView(r.WorkflowExecution, r.BehaviorName, r.BehaviorVersion)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, v)
	}
	return out, next, nil
}

// GetExecutionDetail returns one execution with its retry child, live job
// and waits.
func (s *WorkflowStore) GetExecutionDetail(ctx context.Context, workspaceID string, executionID int64) (workflow.ExecutionDetail, error) {
	ws, err := readWorkspace(workspaceID)
	if err != nil {
		return workflow.ExecutionDetail{}, err
	}
	if executionID <= 0 {
		return workflow.ExecutionDetail{}, workflowInvalid("execution_id")
	}
	row, err := s.q.GetWorkflowExecutionView(ctx, sqlc.GetWorkflowExecutionViewParams{WorkspaceID: ws, ID: executionID})
	if err != nil {
		if isNoRows(err) {
			return workflow.ExecutionDetail{}, workflow.ErrNotFound
		}
		return workflow.ExecutionDetail{}, err
	}
	view, err := mapExecutionView(row.WorkflowExecution, row.BehaviorName, row.BehaviorVersion)
	if err != nil {
		return workflow.ExecutionDetail{}, err
	}
	detail := workflow.ExecutionDetail{ExecutionView: view}
	child, err := s.q.GetRetryChild(ctx, sqlc.GetRetryChildParams{WorkspaceID: ws, ParentExecutionID: workflowInt8(executionID)})
	switch {
	case err == nil:
		detail.RetryChildID = child.ID
	case !isNoRows(err):
		return workflow.ExecutionDetail{}, err
	}
	job, err := s.q.GetLiveWorkflowJobView(ctx, sqlc.GetLiveWorkflowJobViewParams{WorkspaceID: ws, ExecutionID: executionID})
	switch {
	case err == nil:
		j := mapJobView(job)
		detail.LiveJob = &j
	case !isNoRows(err):
		return workflow.ExecutionDetail{}, err
	}
	waits, err := s.q.ListExecutionWaitViews(ctx, sqlc.ListExecutionWaitViewsParams{WorkspaceID: ws, ExecutionID: executionID})
	if err != nil {
		return workflow.ExecutionDetail{}, err
	}
	for _, w := range waits {
		detail.Waits = append(detail.Waits, workflow.WaitView{
			ID: w.ID, StepKey: w.StepKey, ApprovalID: w.ApprovalID, Kind: w.WaitKind, Decision: w.Decision.String,
			DecidedAt: tsPtr(w.DecidedAt), ExpiresAt: tsPtr(w.ExpiresAt), ClosedAt: tsPtr(w.ClosedAt),
			Signalled: w.Signalled, SignalConsumedAt: tsPtr(w.ConsumedAt), CreatedAt: w.CreatedAt.Time,
		})
	}
	return detail, nil
}

// requireExecution returns workflow.ErrNotFound unless the execution
// belongs to the workspace.
func (s *WorkflowStore) requireExecution(ctx context.Context, ws pgtype.UUID, executionID int64) error {
	if executionID <= 0 {
		return workflowInvalid("execution_id")
	}
	if _, err := s.q.GetWorkflowExecution(ctx, sqlc.GetWorkflowExecutionParams{WorkspaceID: ws, ID: executionID}); err != nil {
		if isNoRows(err) {
			return workflow.ErrNotFound
		}
		return err
	}
	return nil
}

// ListSteps returns the execution's steps in definition order (at most 64).
func (s *WorkflowStore) ListSteps(ctx context.Context, workspaceID string, executionID int64) ([]workflow.Step, error) {
	ws, err := readWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	if err := s.requireExecution(ctx, ws, executionID); err != nil {
		return nil, err
	}
	rows, err := s.q.ListExecutionSteps(ctx, sqlc.ListExecutionStepsParams{WorkspaceID: ws, ExecutionID: executionID})
	if err != nil {
		return nil, err
	}
	out := make([]workflow.Step, 0, len(rows))
	for _, r := range rows {
		step, err := mapWorkflowStep(r)
		if err != nil {
			return nil, err
		}
		out = append(out, step)
	}
	return out, nil
}

// ListEvents returns a page of the execution's history in sequence order.
// The next cursor value is the last returned sequence.
func (s *WorkflowStore) ListEvents(ctx context.Context, workspaceID string, executionID, afterSequence int64, limit int) ([]workflow.Event, int64, error) {
	ws, err := readWorkspace(workspaceID)
	if err != nil {
		return nil, 0, err
	}
	if !workflow.ValidListLimit(limit) || afterSequence < 0 {
		return nil, 0, workflowInvalid("event query")
	}
	if err := s.requireExecution(ctx, ws, executionID); err != nil {
		return nil, 0, err
	}
	rows, err := s.q.ListExecutionEventsPage(ctx, sqlc.ListExecutionEventsPageParams{
		WorkspaceID: ws, ExecutionID: executionID, AfterSequence: afterSequence, RowLimit: int32(limit + 1),
	})
	if err != nil {
		return nil, 0, err
	}
	n, next := pageNext(len(rows), limit, func() int64 { return rows[limit-1].Sequence })
	out := make([]workflow.Event, 0, n)
	for _, r := range rows[:n] {
		out = append(out, workflow.Event{
			ID: r.ID, ExecutionID: r.ExecutionID, Sequence: r.Sequence, Kind: workflow.EventKind(r.Kind),
			FromState: workflow.State(r.FromState.String), ToState: workflow.State(r.ToState.String),
			StepKey: r.StepKey.String, Attempt: int(r.Attempt), Reason: workflow.ReasonCode(r.ReasonCode.String),
			IdempotencyKey: r.IdempotencyKey.String,
			Usage: workflow.Usage{InputTokens: r.InputTokens, OutputTokens: r.OutputTokens,
				CacheReadTokens: r.CacheReadTokens, CacheWriteTokens: r.CacheWriteTokens},
			CreatedAt: r.CreatedAt.Time,
		})
	}
	return out, next, nil
}

// ListJobs returns a page of the workspace's queue rows, newest first.
// Status "dead_letter" lists the dead letters with their reasons.
func (s *WorkflowStore) ListJobs(ctx context.Context, q workflow.JobQuery) ([]workflow.JobView, int64, error) {
	ws, err := readWorkspace(q.WorkspaceID)
	if err != nil {
		return nil, 0, err
	}
	if !workflow.ValidListLimit(q.Limit) || q.BeforeID < 0 || q.ExecutionID < 0 || (q.Status != "" && !workflow.ValidJobStatus(q.Status)) {
		return nil, 0, workflowInvalid("job query")
	}
	if q.ExecutionID > 0 {
		if err := s.requireExecution(ctx, ws, q.ExecutionID); err != nil {
			return nil, 0, err
		}
	}
	rows, err := s.q.ListWorkflowJobsPage(ctx, sqlc.ListWorkflowJobsPageParams{
		WorkspaceID: ws, Status: workflowText(q.Status), ExecutionID: workflowInt8(q.ExecutionID),
		BeforeID: workflowInt8(q.BeforeID), RowLimit: int32(q.Limit + 1),
	})
	if err != nil {
		return nil, 0, err
	}
	n, next := pageNext(len(rows), q.Limit, func() int64 { return rows[q.Limit-1].ID })
	out := make([]workflow.JobView, 0, n)
	for _, r := range rows[:n] {
		out = append(out, mapJobView(r))
	}
	return out, next, nil
}

// ExecutionLinksForRun returns the executions that carry a legacy behavior
// run's ID (the imported or durable execution and its retry children).
func (s *WorkflowStore) ExecutionLinksForRun(ctx context.Context, workspaceID string, runID int64) ([]behavior.TraceExecution, error) {
	ws, err := readWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	if runID <= 0 {
		return nil, workflowInvalid("run_id")
	}
	rows, err := s.q.ListExecutionLinksByLegacyRun(ctx, sqlc.ListExecutionLinksByLegacyRunParams{WorkspaceID: ws, LegacyRunID: workflowInt8(runID)})
	if err != nil {
		return nil, err
	}
	out := make([]behavior.TraceExecution, 0, len(rows))
	for _, r := range rows {
		out = append(out, behavior.TraceExecution{ExecutionID: r.ID, State: r.State, Reason: r.ReasonCode.String,
			ParentExecutionID: r.ParentExecutionID.Int64})
	}
	return out, nil
}
