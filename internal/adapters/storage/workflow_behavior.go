package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/jackc/pgx/v5/pgtype"
)

// This file is the durable side of behavior runs: writes of a run executed
// as workflow steps. Each one runs in a transaction that first locks the
// execution, then the workflow job fence (claim token, lease, execution
// state, cancellation and deadline, step and attempt) and then the run,
// which must be the execution's legacy run in the same workspace and not
// legacy-claimed. A refused fence is behavior.ErrWorkflowFence; nothing is
// written.

var _ behavior.DurableRunStore = (*BehaviorStore)(nil)

type durableRunTx struct {
	q   *sqlc.Queries
	ws  pgtype.UUID
	job sqlc.Job
	run sqlc.BehaviorRun
	req workflow.JobCheckpointRequest
}

func durableFenceRequest(f behavior.RunFence) workflow.JobCheckpointRequest {
	return workflow.JobCheckpointRequest{
		JobID: f.JobID, WorkspaceID: f.WorkspaceID, ExecutionID: f.ExecutionID, StepKey: f.StepKey,
		ExpectedAttempt: f.Attempt, ClaimToken: f.ClaimToken, At: f.At,
	}
}

// withDurableRun runs fn under the fence. running requires the run to be
// running without a legacy claim; otherwise a pending run is accepted too.
func (s *BehaviorStore) withDurableRun(ctx context.Context, f behavior.RunFence, runID int64, running bool, fn func(*durableRunTx) error) error {
	ws, err := parseDBID(f.WorkspaceID)
	if err != nil || runID <= 0 || f.JobID <= 0 || f.ExecutionID <= 0 || strings.TrimSpace(f.StepKey) == "" ||
		f.Attempt < 1 || f.At.IsZero() {
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
	req := durableFenceRequest(f)
	// Lock order everywhere is execution, then job: the reconciler and job
	// results hold the execution before touching its job.
	if _, err := q.LockWorkflowExecution(ctx, sqlc.LockWorkflowExecutionParams{WorkspaceID: ws, ID: f.ExecutionID}); err != nil {
		if isNoRows(err) {
			return behavior.ErrWorkflowFence
		}
		return err
	}
	job, err := lockWorkflowFence(ctx, q, ws, token, req)
	if errors.Is(err, workflow.ErrStaleClaim) {
		return behavior.ErrWorkflowFence
	}
	if err != nil {
		return err
	}
	run, err := q.LockDurableBehaviorRun(ctx, sqlc.LockDurableBehaviorRunParams{WorkspaceID: ws, ID: runID, ExecutionID: f.ExecutionID})
	if isNoRows(err) {
		return behavior.ErrWorkflowFence
	}
	if err != nil {
		return err
	}
	if run.ClaimToken.Valid || !(run.Status == string(behavior.RunRunning) || !running && run.Status == string(behavior.RunPending)) {
		return behavior.ErrConflict
	}
	if err := fn(&durableRunTx{q: q, ws: ws, job: job, run: run, req: req}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func durableRows(n int64, err error) error {
	if err != nil {
		return err
	}
	if n != 1 {
		return behavior.ErrConflict
	}
	return nil
}

// BeginDurableRunAttempt starts a new attempt of the run: running, one more
// attempt, and the attempt's deadline.
func (s *BehaviorStore) BeginDurableRunAttempt(ctx context.Context, f behavior.RunFence, runID int64, deadline time.Time) (behavior.Run, error) {
	if deadline.IsZero() {
		return behavior.Run{}, invalid("deadline")
	}
	var out behavior.Run
	err := s.withDurableRun(ctx, f, runID, false, func(t *durableRunTx) error {
		row, err := t.q.BeginDurableBehaviorRunAttempt(ctx, sqlc.BeginDurableBehaviorRunAttemptParams{
			Deadline: ts(deadline), WorkspaceID: t.ws, ID: runID,
		})
		if isNoRows(err) {
			return behavior.ErrConflict
		}
		if err != nil {
			return err
		}
		out = mapRun(row, "")
		return nil
	})
	return out, err
}

// SetDurableRunContext stores the run's context snapshot and, in the same
// transaction, the step's checkpoint that references it.
func (s *BehaviorStore) SetDurableRunContext(ctx context.Context, f behavior.RunFence, runID int64, snapshot json.RawMessage, cp workflow.Checkpoint) error {
	snap, err := objectJSON(snapshot, "context")
	if err != nil {
		return err
	}
	checkpoint, err := workflow.MarshalCheckpoint(cp)
	if err != nil {
		return invalid("checkpoint")
	}
	return s.withDurableRun(ctx, f, runID, true, func(t *durableRunTx) error {
		if err := durableRows(t.q.SetDurableBehaviorRunContext(ctx, sqlc.SetDurableBehaviorRunContextParams{
			Snapshot: snap, WorkspaceID: t.ws, ID: runID,
		})); err != nil {
			return err
		}
		_, err := updateWorkflowProgress(ctx, t.q, t.ws, t.job, t.req, checkpoint)
		return durableFenceErr(err)
	})
}

// AddDurableRunUsage adds one model call's usage to the run (its budget)
// and to the step, atomically.
func (s *BehaviorStore) AddDurableRunUsage(ctx context.Context, f behavior.RunFence, runID int64, u behavior.Usage) error {
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.CacheReadTokens < 0 || u.CacheWriteTokens < 0 || u.Steps < 0 {
		return invalid("usage")
	}
	return s.withDurableRun(ctx, f, runID, true, func(t *durableRunTx) error {
		if err := durableRows(t.q.AddDurableBehaviorRunUsage(ctx, sqlc.AddDurableBehaviorRunUsageParams{
			Input: int32(u.InputTokens), Output: int32(u.OutputTokens), CacheRead: int32(u.CacheReadTokens),
			CacheWrite: int32(u.CacheWriteTokens), Steps: int32(u.Steps), WorkspaceID: t.ws, ID: runID,
		})); err != nil {
			return err
		}
		req := t.req
		req.Usage = workflow.Usage{
			InputTokens: int64(u.InputTokens), OutputTokens: int64(u.OutputTokens),
			CacheReadTokens: int64(u.CacheReadTokens), CacheWriteTokens: int64(u.CacheWriteTokens),
		}
		_, err := updateWorkflowProgress(ctx, t.q, t.ws, t.job, req, nil)
		return durableFenceErr(err)
	})
}

// AddDurableRunItems stores every item of the run in one transaction; a
// key already stored keeps its stored row.
func (s *BehaviorStore) AddDurableRunItems(ctx context.Context, f behavior.RunFence, runID int64, items []behavior.NewItem) ([]behavior.Item, error) {
	var out []behavior.Item
	err := s.withDurableRun(ctx, f, runID, true, func(t *durableRunTx) error {
		out = make([]behavior.Item, 0, len(items))
		for _, it := range items {
			it.RunID = runID
			item, _, err := addItem(ctx, t.q, t.ws, it)
			if err != nil {
				return err
			}
			out = append(out, item)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetDurableRunActions stores the run-level action outcomes.
func (s *BehaviorStore) SetDurableRunActions(ctx context.Context, f behavior.RunFence, runID int64, result json.RawMessage) error {
	res, err := objectJSON(result, "run actions")
	if err != nil {
		return err
	}
	return s.withDurableRun(ctx, f, runID, true, func(t *durableRunTx) error {
		return durableRows(t.q.SetDurableBehaviorRunActions(ctx, sqlc.SetDurableBehaviorRunActionsParams{
			Result: res, WorkspaceID: t.ws, ID: runID,
		}))
	})
}

// RetryDurableRun records why the attempt is tried again and ends its
// deadline; the workflow job owns the schedule.
func (s *BehaviorStore) RetryDurableRun(ctx context.Context, f behavior.RunFence, runID int64, code string) error {
	if strings.TrimSpace(code) == "" {
		return invalid("code")
	}
	return s.withDurableRun(ctx, f, runID, true, func(t *durableRunTx) error {
		return durableRows(t.q.RetryDurableBehaviorRun(ctx, sqlc.RetryDurableBehaviorRunParams{
			Code: code, WorkspaceID: t.ws, ID: runID,
		}))
	})
}

// FinishDurableRun ends the run's read projection.
func (s *BehaviorStore) FinishDurableRun(ctx context.Context, f behavior.RunFence, runID int64, status behavior.RunStatus, code string) error {
	switch status {
	case behavior.RunDone, behavior.RunFailed, behavior.RunSkipped:
	default:
		return invalid("status")
	}
	return s.withDurableRun(ctx, f, runID, true, func(t *durableRunTx) error {
		return durableRows(t.q.FinishDurableBehaviorRun(ctx, sqlc.FinishDurableBehaviorRunParams{
			Status: string(status), Code: optText(code), WorkspaceID: t.ws, ID: runID,
		}))
	})
}

func durableFenceErr(err error) error {
	if errors.Is(err, workflow.ErrStaleClaim) {
		return behavior.ErrWorkflowFence
	}
	return err
}
