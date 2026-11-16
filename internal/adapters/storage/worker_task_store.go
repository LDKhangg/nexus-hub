package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	_ behavior.WorkerJobControl = (*WorkerStore)(nil)
	_ behavior.LocalTaskJobs    = (*WorkerStore)(nil)
	_ behavior.LocalTaskStarter = (*WorkerTaskStore)(nil)
	_ behavior.LocalTaskRoutes  = (*WorkerTaskStore)(nil)
	_ behavior.LocalTaskWaker   = (*WorkerTaskStore)(nil)
	_ behavior.WorkerTaskUpkeep = (*WorkerTaskStore)(nil)
)

// JobForStep returns the newest job of a workflow step; ok=false when none.
func (s *WorkerStore) JobForStep(ctx context.Context, workspaceID string, executionID int64, stepID string) (behavior.WorkerJob, bool, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.WorkerJob{}, false, invalid("workspace_id")
	}
	if executionID <= 0 || len(stepID) > 128 {
		return behavior.WorkerJob{}, false, invalid("step")
	}
	row, err := s.q.GetWorkerJobForStep(ctx, sqlc.GetWorkerJobForStepParams{WorkspaceID: ws, ExecutionID: optInt8(executionID), StepID: stepID})
	if isNoRows(err) {
		return behavior.WorkerJob{}, false, nil
	}
	if err != nil {
		return behavior.WorkerJob{}, false, err
	}
	return mapWorkerJob(row), true, nil
}

// RetryJob queues a FAILED, NEEDS_INPUT or INTERRUPTED job again when it is
// still on the given attempt. ErrConflict otherwise, ErrNotFound if unknown.
func (s *WorkerStore) RetryJob(ctx context.Context, workspaceID, jobID string, attempt int) (behavior.WorkerJob, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.WorkerJob{}, invalid("workspace_id")
	}
	id, err := parseDBID(jobID)
	if err != nil {
		return behavior.WorkerJob{}, behavior.ErrNotFound
	}
	if attempt < 0 || attempt > 1<<30 {
		return behavior.WorkerJob{}, invalid("attempt")
	}
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return behavior.WorkerJob{}, err
	}
	defer s.rollback(tx)
	q := s.q.WithTx(tx)
	before, err := q.LockWorkerJob(ctx, sqlc.LockWorkerJobParams{WorkspaceID: ws, ID: id})
	if err != nil {
		return behavior.WorkerJob{}, notFound(err)
	}
	row, err := q.RetryWorkerJob(ctx, sqlc.RetryWorkerJobParams{Now: ts(now), WorkspaceID: ws, ID: id, Attempt: int32(attempt)})
	if err != nil {
		if isNoRows(err) {
			return behavior.WorkerJob{}, behavior.ErrConflict
		}
		return behavior.WorkerJob{}, err
	}
	if err := q.InsertWorkerEvent(ctx, workerEvent(ws, before.DeviceID, row.ID, behavior.WorkerEventRetried, "")); err != nil {
		return behavior.WorkerJob{}, err
	}
	if before.DeviceID.Valid {
		if err := s.refreshDevice(ctx, q, before.DeviceID, now); err != nil {
			return behavior.WorkerJob{}, err
		}
	}
	if row.ExecutionID.Valid {
		var payload struct {
			InstructionVersion int64 `json:"instruction_version"`
		}
		_ = json.Unmarshal(row.Payload, &payload)
		if err := q.ResetWorkerTaskInstructionVersion(ctx, sqlc.ResetWorkerTaskInstructionVersionParams{
			Version: max(payload.InstructionVersion, 0), WorkspaceID: ws, ExecutionID: row.ExecutionID.Int64,
		}); err != nil {
			return behavior.WorkerJob{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return behavior.WorkerJob{}, err
	}
	s.signal()
	return mapWorkerJob(row), nil
}

// WorkerTaskStore starts local tasks, keeps their routes and connects worker
// jobs to their workflow step.
type WorkerTaskStore struct {
	pool      *pgxpool.Pool
	q         *sqlc.Queries
	workers   *WorkerStore
	workflows *WorkflowStore
}

// NewWorkerTaskStore builds the store over the worker and workflow stores.
func NewWorkerTaskStore(pool *pgxpool.Pool, workers *WorkerStore, workflows *WorkflowStore) (*WorkerTaskStore, error) {
	if pool == nil || workers == nil || workflows == nil {
		return nil, errors.New("worker task store requires pool and stores")
	}
	return &WorkerTaskStore{pool: pool, q: sqlc.New(pool), workers: workers, workflows: workflows}, nil
}

// StartLocalTask starts the local-task execution and stores its route in one
// transaction. A repeated request (same update) returns the same execution.
func (s *WorkerTaskStore) StartLocalTask(ctx context.Context, req behavior.LocalTaskRequest) (workflow.Execution, error) {
	ws, err := parseDBID(req.WorkspaceID)
	if err != nil {
		return workflow.Execution{}, invalid("workspace_id")
	}
	integration, err := parseDBID(req.IntegrationID)
	if err != nil {
		return workflow.Execution{}, invalid("integration_id")
	}
	if !tenant.ValidRepo(req.Repo) || req.ClickUpTaskID == "" || len(req.ClickUpTaskID) > 64 || len(req.TaskRef) > 64 || req.ChatID == 0 {
		return workflow.Execution{}, invalid("local task")
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
	// Only a cut-over workspace runs durable workflows.
	if allowed, err := workflowStartAllowed(ctx, q, ws); err != nil {
		return workflow.Execution{}, err
	} else if !allowed {
		return workflow.Execution{}, behavior.ErrNotAvailable
	}
	versionID, err := ensureLocalTaskVersion(ctx, q, ws)
	if err != nil {
		return workflow.Execution{}, err
	}
	exec, err := s.workflows.StartExecutionTx(ctx, tx, behavior.LocalTaskStartRequest(req, versionID, correlation.String(), time.Now()))
	if err != nil {
		return workflow.Execution{}, err
	}
	if err := q.InsertWorkerTaskRoute(ctx, sqlc.InsertWorkerTaskRouteParams{
		WorkspaceID: ws, ExecutionID: exec.ID, IntegrationID: integration, ChatID: req.ChatID,
		Repo: req.Repo, TaskRef: req.TaskRef, ClickupTaskID: req.ClickUpTaskID,
	}); err != nil {
		return workflow.Execution{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return workflow.Execution{}, err
	}
	return exec, nil
}

// ensureLocalTaskVersion returns the workspace's internal local-task version
// whose document equals the current definition, publishing a new immutable
// version when the definition changed.
func ensureLocalTaskVersion(ctx context.Context, q *sqlc.Queries, ws pgtype.UUID) (int64, error) {
	doc, err := json.Marshal(behavior.NewLocalTaskSnapshot())
	if err != nil {
		return 0, err
	}
	if err := q.InsertInternalBehavior(ctx, sqlc.InsertInternalBehaviorParams{WorkspaceID: ws, InternalKind: behavior.InternalLocalTaskKind}); err != nil {
		return 0, err
	}
	def, err := q.LockInternalBehavior(ctx, sqlc.LockInternalBehaviorParams{WorkspaceID: ws, InternalKind: behavior.InternalLocalTaskKind})
	if err != nil {
		return 0, err
	}
	next := int32(1)
	latest, err := q.LatestInternalBehaviorVersion(ctx, sqlc.LatestInternalBehaviorVersionParams{WorkspaceID: ws, BehaviorID: def.ID})
	switch {
	case err == nil:
		var prev behavior.LocalTaskSnapshot
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

// RegisteredRepos lists the repositories registered on the workspace's
// ClickUp sources (agent.tools.github.repos), sorted and without repeats.
func (s *WorkerTaskStore) RegisteredRepos(ctx context.Context, workspaceID string) ([]string, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, invalid("workspace_id")
	}
	configs, err := s.q.ListWorkerTaskRepoConfigs(ctx, ws)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, raw := range configs {
		var cfg struct {
			Agent struct {
				Tools struct {
					GitHub struct {
						Repos map[string]json.RawMessage `json:"repos"`
					} `json:"github"`
				} `json:"tools"`
			} `json:"agent"`
		}
		if json.Unmarshal(raw, &cfg) != nil {
			continue
		}
		for name := range cfg.Agent.Tools.GitHub.Repos {
			if tenant.ValidRepo(name) {
				seen[name] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// --- routes ---

func mapWorkerTaskRoute(r sqlc.WorkerTaskRoute) behavior.LocalTaskRoute {
	return behavior.LocalTaskRoute{
		WorkspaceID: uuidString(r.WorkspaceID), ExecutionID: r.ExecutionID, IntegrationID: uuidString(r.IntegrationID),
		ChatID: r.ChatID, ThreadID: r.ThreadID, Repo: r.Repo, TaskRef: r.TaskRef, ClickUpTaskID: r.ClickupTaskID,
		ProgressMessageID: r.ProgressMessageID, ProgressAttempt: int(r.ProgressJobAttempt),
		WaitingNotice: r.WaitingNotice, InstructionVersion: r.InstructionVersion,
	}
}

// GetRoute returns the route of a local task; ErrNotFound for any other
// execution.
func (s *WorkerTaskStore) GetRoute(ctx context.Context, workspaceID string, executionID int64) (behavior.LocalTaskRoute, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.LocalTaskRoute{}, invalid("workspace_id")
	}
	row, err := s.q.GetWorkerTaskRoute(ctx, sqlc.GetWorkerTaskRouteParams{WorkspaceID: ws, ExecutionID: executionID})
	if err != nil {
		return behavior.LocalTaskRoute{}, notFound(err)
	}
	return mapWorkerTaskRoute(row), nil
}

func (s *WorkerTaskStore) SetProgressMessage(ctx context.Context, workspaceID string, executionID, messageID int64, attempt int, threadID int64) error {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return invalid("workspace_id")
	}
	return s.q.SetWorkerTaskProgressMessage(ctx, sqlc.SetWorkerTaskProgressMessageParams{
		ProgressMessageID: messageID, ProgressJobAttempt: int32(attempt), ThreadID: threadID, WorkspaceID: ws, ExecutionID: executionID,
	})
}

func (s *WorkerTaskStore) SetWaitingNotice(ctx context.Context, workspaceID string, executionID int64, notice string) error {
	ws, err := parseDBID(workspaceID)
	if err != nil || len(notice) > 64 {
		return invalid("waiting notice")
	}
	return s.q.SetWorkerTaskWaitingNotice(ctx, sqlc.SetWorkerTaskWaitingNoticeParams{WaitingNotice: notice, WorkspaceID: ws, ExecutionID: executionID})
}

func (s *WorkerTaskStore) SetInstructionVersion(ctx context.Context, workspaceID string, executionID, version int64) error {
	ws, err := parseDBID(workspaceID)
	if err != nil || version < 0 {
		return invalid("instruction version")
	}
	return s.q.SetWorkerTaskInstructionVersion(ctx, sqlc.SetWorkerTaskInstructionVersionParams{Version: version, WorkspaceID: ws, ExecutionID: executionID})
}

// --- job to step ---

// ReasonWorkerJobEnded is the transition reason of a step woken because its
// worker job ended.
const ReasonWorkerJobEnded workflow.ReasonCode = "worker_job_ended"

// WakeWorkerStep queues the waiting worker.run step again; the step then
// reads its job and finishes. A step that is not waiting (still running, or
// already finished) is left alone: ErrNotEligible reports it.
func (s *WorkerTaskStore) WakeWorkerStep(ctx context.Context, workspaceID string, executionID int64, stepID string) error {
	ws, err := parseDBID(workspaceID)
	if err != nil || executionID <= 0 || strings.TrimSpace(stepID) == "" {
		return workflowInvalid("worker step")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, woke, err := wakeStepTx(ctx, sqlc.New(tx), ws, executionID, stepID, time.Now(), ReasonWorkerJobEnded, "", false)
	if err != nil {
		return err
	}
	if !woke {
		return workflow.ErrNotEligible
	}
	return tx.Commit(ctx)
}

const workerTaskUpkeepBatch = 50

// SyncWorkerTasks repairs what an observer notification could not: waiting
// steps whose job already succeeded or was cancelled are woken, and
// unfinished jobs of ended executions are cancelled.
func (s *WorkerTaskStore) SyncWorkerTasks(ctx context.Context) error {
	var firstErr error
	wake, err := s.q.ListWorkerJobsToWake(ctx, workerTaskUpkeepBatch)
	if err != nil {
		firstErr = err
	}
	for _, w := range wake {
		if err := s.WakeWorkerStep(ctx, uuidString(w.WorkspaceID), w.ExecutionID.Int64, w.StepID); err != nil && !errors.Is(err, workflow.ErrNotEligible) {
			log.Print("worker tasks: a waiting step was not woken")
			firstErr = err
		}
	}
	ended, err := s.q.ListWorkerJobsOfEndedExecutions(ctx, workerTaskUpkeepBatch)
	if err != nil && firstErr == nil {
		firstErr = err
	}
	for _, j := range ended {
		if _, err := s.workers.CancelJob(ctx, uuidString(j.WorkspaceID), uuidString(j.ID)); err != nil &&
			!errors.Is(err, behavior.ErrConflict) && !errors.Is(err, behavior.ErrNotFound) {
			log.Print("worker tasks: a job of an ended execution was not cancelled")
			firstErr = err
		}
	}
	return firstErr
}
