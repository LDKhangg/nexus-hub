package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/orchestrator"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var _ orchestrator.RouteReader = (*OrchestratedTaskStore)(nil)

// OrchestratedTaskStore starts orchestrated tasks and keeps their routes.
type OrchestratedTaskStore struct {
	pool      *pgxpool.Pool
	q         *sqlc.Queries
	workflows *WorkflowStore
}

// NewOrchestratedTaskStore builds the store over the workflow store.
func NewOrchestratedTaskStore(pool *pgxpool.Pool, workflows *WorkflowStore) (*OrchestratedTaskStore, error) {
	if pool == nil || workflows == nil {
		return nil, errors.New("orchestrated task store requires pool and workflow store")
	}
	return &OrchestratedTaskStore{pool: pool, q: sqlc.New(pool), workflows: workflows}, nil
}

// StartOrchestratedTask starts the orchestrated_task execution and stores
// its route in one transaction. A repeated request (same update) returns
// the same execution. The caller stores the request text as the
// execution's first instruction right after. behavior.ErrNotAvailable when
// the workspace does not run durable workflows.
func (s *OrchestratedTaskStore) StartOrchestratedTask(ctx context.Context, req orchestrator.TaskRequest) (workflow.Execution, error) {
	ws, err := parseDBID(req.WorkspaceID)
	if err != nil {
		return workflow.Execution{}, invalid("workspace_id")
	}
	integration, err := parseDBID(req.IntegrationID)
	if err != nil {
		return workflow.Execution{}, invalid("integration_id")
	}
	if (req.Repo != "" && !tenant.ValidRepo(req.Repo)) || len(req.ClickUpTaskID) > 64 || len(req.TaskRef) > 64 || req.ChatID == 0 || req.UpdateID <= 0 {
		return workflow.Execution{}, invalid("orchestrated task")
	}
	correlation, err := uuid.NewRandom()
	if err != nil {
		return workflow.Execution{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return workflow.Execution{}, err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	if allowed, err := workflowStartAllowed(ctx, q, ws); err != nil {
		return workflow.Execution{}, err
	} else if !allowed {
		return workflow.Execution{}, behavior.ErrNotAvailable
	}
	versionID, err := ensureOrchestratedTaskVersion(ctx, q, ws)
	if err != nil {
		return workflow.Execution{}, err
	}
	exec, err := s.workflows.StartExecutionTx(ctx, tx, orchestrator.TaskStartRequest(req, versionID, correlation.String(), time.Now()))
	if err != nil {
		return workflow.Execution{}, err
	}
	if err := q.InsertOrchestratorRoute(ctx, sqlc.InsertOrchestratorRouteParams{
		WorkspaceID: ws, ExecutionID: exec.ID, IntegrationID: integration, ChatID: req.ChatID, ThreadID: req.ThreadID,
		Repo: req.Repo, TaskRef: req.TaskRef, ClickupTaskID: req.ClickUpTaskID,
	}); err != nil {
		return workflow.Execution{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return workflow.Execution{}, err
	}
	return exec, nil
}

// ensureOrchestratedTaskVersion returns the workspace's internal
// orchestrated_task version whose document equals the current definition,
// publishing a new immutable version when the definition changed.
func ensureOrchestratedTaskVersion(ctx context.Context, q *sqlc.Queries, ws pgtype.UUID) (int64, error) {
	doc, err := json.Marshal(orchestrator.NewOrchestratedTaskSnapshot())
	if err != nil {
		return 0, err
	}
	kind := orchestrator.InternalOrchestratedTaskKind
	if err := q.InsertInternalBehavior(ctx, sqlc.InsertInternalBehaviorParams{WorkspaceID: ws, InternalKind: kind}); err != nil {
		return 0, err
	}
	def, err := q.LockInternalBehavior(ctx, sqlc.LockInternalBehaviorParams{WorkspaceID: ws, InternalKind: kind})
	if err != nil {
		return 0, err
	}
	next := int32(1)
	latest, err := q.LatestInternalBehaviorVersion(ctx, sqlc.LatestInternalBehaviorVersionParams{WorkspaceID: ws, BehaviorID: def.ID})
	switch {
	case err == nil:
		var prev orchestrator.OrchestratedTaskSnapshot
		if json.Unmarshal(latest.Document, &prev) == nil {
			if prevDoc, err := json.Marshal(prev); err == nil && bytes.Equal(prevDoc, doc) {
				return latest.ID, nil
			}
		}
		next = latest.Version + 1
	case !isNoRows(err):
		return 0, err
	}
	v, err := q.InsertInternalBehaviorVersion(ctx, sqlc.InsertInternalBehaviorVersionParams{
		WorkspaceID: ws, BehaviorID: def.ID, Version: next, Document: doc,
	})
	if err != nil {
		return 0, err
	}
	return v.ID, nil
}

// GetRoute returns the route of an orchestrated task;
// orchestrator.ErrRouteNotFound for any other execution.
func (s *OrchestratedTaskStore) GetRoute(ctx context.Context, workspaceID string, executionID int64) (orchestrator.Route, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return orchestrator.Route{}, invalid("workspace_id")
	}
	row, err := s.q.GetOrchestratorRoute(ctx, sqlc.GetOrchestratorRouteParams{WorkspaceID: ws, ExecutionID: executionID})
	if isNoRows(err) {
		return orchestrator.Route{}, orchestrator.ErrRouteNotFound
	}
	if err != nil {
		return orchestrator.Route{}, err
	}
	return orchestrator.Route{
		WorkspaceID: uuidString(row.WorkspaceID), ExecutionID: row.ExecutionID, IntegrationID: uuidString(row.IntegrationID),
		ChatID: row.ChatID, ThreadID: row.ThreadID, Repo: row.Repo, TaskRef: row.TaskRef, ClickUpTaskID: row.ClickupTaskID,
	}, nil
}
