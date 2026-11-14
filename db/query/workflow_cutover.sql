-- Workflow cutover: the per-workspace runtime-mode fence, the import of
-- legacy behavior runs and their blocking approvals, the durable starters
-- and the repair of legacy read projections. Every query is workspace
-- scoped except the listing of cut-over workspaces.

-- name: LockWorkspaceForCutover :one
-- Held for the whole cutover transaction: claims and starts of the
-- workspace (KEY SHARE ... SKIP LOCKED) skip it meanwhile.
SELECT id FROM public.workspaces WHERE id = sqlc.arg(workspace_id) FOR UPDATE;

-- name: LockWorkspaceRuntimeShared :one
-- Taken by every legacy claim and durable start before it reads the mode.
-- No row means a cutover holds the workspace: the caller skips it.
SELECT id FROM public.workspaces WHERE id = sqlc.arg(workspace_id) FOR KEY SHARE SKIP LOCKED;

-- name: GetWorkflowRuntimeMode :one
SELECT COALESCE((SELECT m.mode FROM public.workflow_runtime_modes m
                 WHERE m.workspace_id = sqlc.arg(workspace_id)), 'legacy')::text AS mode;

-- name: SetWorkflowRuntimeModeWorkflow :exec
INSERT INTO public.workflow_runtime_modes (workspace_id, mode, cutover_at)
VALUES (sqlc.arg(workspace_id), 'workflow', sqlc.arg(at_time)::timestamptz)
ON CONFLICT (workspace_id) DO UPDATE SET mode = 'workflow',
  cutover_at = COALESCE(public.workflow_runtime_modes.cutover_at, EXCLUDED.cutover_at), updated_at = now();

-- name: ListWorkflowModeWorkspaces :many
-- The cut-over workspaces, for the durable starters (a listing of
-- workspaces, like the workspace repository's).
SELECT workspace_id FROM public.workflow_runtime_modes
WHERE mode = 'workflow' AND workspace_id > sqlc.arg(after_id)::uuid
ORDER BY workspace_id
LIMIT sqlc.arg(max_rows)::int;

-- name: ListCutoverBehaviorRuns :many
-- The runs a cutover imports: pending and running runs (with or without a
-- legacy claim) and done runs that still have an open blocking approval,
-- unless an execution already owns the run.
SELECT r.* FROM public.behavior_runs r
WHERE r.workspace_id = sqlc.arg(workspace_id) AND r.id > sqlc.arg(after_id)::bigint
  AND (r.status IN ('pending','running')
       OR (r.status = 'done' AND EXISTS (
             SELECT 1 FROM public.approvals a
             WHERE a.workspace_id = r.workspace_id AND a.run_id = r.id AND a.decision IS NULL
               AND a.item_id IS NOT NULL AND a.kind IN ('item_answer','post_draft','confirm_action'))))
  AND NOT EXISTS (SELECT 1 FROM public.workflow_executions e
                  WHERE e.workspace_id = r.workspace_id AND e.legacy_run_id = r.id)
ORDER BY r.id
LIMIT sqlc.arg(max_rows)::int;

-- name: LockCutoverBehaviorRun :one
SELECT * FROM public.behavior_runs
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id)
FOR UPDATE;

-- name: BehaviorRunHasExecution :one
SELECT EXISTS (SELECT 1 FROM public.workflow_executions e
               WHERE e.workspace_id = sqlc.arg(workspace_id) AND e.legacy_run_id = sqlc.arg(run_id))::bool AS owned;

-- name: ListOpenBlockingApprovals :many
-- The open blocking approvals of a run (identifiers and kinds only).
SELECT id, kind FROM public.approvals
WHERE workspace_id = sqlc.arg(workspace_id) AND run_id = sqlc.arg(run_id) AND decision IS NULL
  AND item_id IS NOT NULL AND kind IN ('item_answer','post_draft','confirm_action')
ORDER BY id;

-- name: LockOpenApproval :one
-- Locks an approval for binding; a decided approval returns no row and
-- stays on the route its decision already took.
SELECT id, kind FROM public.approvals
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND decision IS NULL
FOR UPDATE;

-- name: CountLegacyRouteApprovals :one
-- Open blocking approvals of runs that ended failed or canceled: no
-- execution is created for them, so their decision keeps the legacy route.
SELECT COUNT(*)::int FROM public.approvals a
JOIN public.behavior_runs r ON r.workspace_id = a.workspace_id AND r.id = a.run_id
WHERE a.workspace_id = sqlc.arg(workspace_id) AND a.decision IS NULL
  AND a.kind IN ('item_answer','post_draft','confirm_action')
  AND r.status IN ('failed','canceled')
  AND NOT EXISTS (SELECT 1 FROM public.workflow_executions e
                  WHERE e.workspace_id = r.workspace_id AND e.legacy_run_id = r.id);

-- name: CountInFlightToolRuns :one
-- Tool runs started without a recorded outcome by a run or job a cutover
-- hands to the durable queue. Their outcome stays unknown: the durable
-- steps reuse the same operation keys and never send them again.
SELECT COUNT(*)::int FROM public.tool_runs t
WHERE t.workspace_id = sqlc.arg(workspace_id) AND t.status = 'started'
  AND (t.behavior_run_id IN (SELECT r.id FROM public.behavior_runs r
                             WHERE r.workspace_id = sqlc.arg(workspace_id) AND r.status IN ('pending','running'))
    OR t.agent_job_id IN (SELECT j.id FROM public.agent_jobs j
                          WHERE j.workspace_id = sqlc.arg(workspace_id) AND j.status IN ('pending','processing')
                            AND j.workflow_execution_id IS NULL));

-- name: CutoverAgentJobStats :one
SELECT
  COUNT(*) FILTER (WHERE status = 'pending')::int AS pending,
  COUNT(*) FILTER (WHERE status = 'pending' AND run_at <= sqlc.arg(at_time)::timestamptz)::int AS pending_due,
  COUNT(*) FILTER (WHERE status = 'processing')::int AS processing,
  COUNT(*) FILTER (WHERE status = 'processing' AND locked_until > sqlc.arg(at_time)::timestamptz)::int AS lease_live
FROM public.agent_jobs
WHERE workspace_id = sqlc.arg(workspace_id) AND workflow_execution_id IS NULL
  AND status IN ('pending','processing');

-- name: SelectStartableBehaviorRun :one
-- The oldest due pending run of the workspace no execution owns yet (a
-- run created by a trigger path after the cutover).
SELECT r.* FROM public.behavior_runs r
WHERE r.workspace_id = sqlc.arg(workspace_id) AND r.status = 'pending' AND r.claim_token IS NULL
  AND r.run_at <= sqlc.arg(at_time)::timestamptz
  AND NOT EXISTS (SELECT 1 FROM public.workflow_executions e
                  WHERE e.workspace_id = r.workspace_id AND e.legacy_run_id = r.id)
ORDER BY r.run_at, r.id
FOR UPDATE OF r SKIP LOCKED LIMIT 1;

-- name: FailUnstartableBehaviorRun :execrows
-- A pending run whose pinned version cannot be normalized into a workflow
-- definition ends failed instead of blocking the starter.
UPDATE public.behavior_runs SET status = 'failed', error_code = sqlc.arg(code)::text,
  finished_at = now(), updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND status = 'pending' AND claim_token IS NULL;

-- name: RepairBehaviorRunProjection :execrows
-- The read projection of a behavior run whose execution ended failed or
-- cancelled outside the run's own steps (deadline, timeout, exhaustion,
-- reconciliation or an operator cancel): a run still pending or running is
-- ended with the execution's reason, unless another live execution (a
-- retry child) owns it.
UPDATE public.behavior_runs r SET status = sqlc.arg(status)::text, error_code = sqlc.arg(code)::text,
  claim_token = NULL, locked_until = NULL, workflow_attempt_deadline = NULL,
  finished_at = now(), updated_at = now()
FROM public.workflow_executions e
WHERE e.workspace_id = sqlc.arg(workspace_id) AND e.id = sqlc.arg(execution_id) AND e.source_kind = 'behavior_run'
  AND r.workspace_id = e.workspace_id AND r.id = e.legacy_run_id
  AND r.status IN ('pending','running')
  AND NOT EXISTS (SELECT 1 FROM public.workflow_executions o
                  WHERE o.workspace_id = e.workspace_id AND o.legacy_run_id = r.id AND o.id <> e.id
                    AND o.state NOT IN ('COMPLETED','FAILED','CANCELLED'));

-- name: ReopenBehaviorRunForRetry :execrows
-- A manual retry child of a failed behavior execution reopens the run's
-- failed projection as pending with a fresh run-attempt budget; usage,
-- items, receipts and run-action outcomes are kept, so known side effects
-- are not repeated and token budgets are not multiplied.
UPDATE public.behavior_runs r SET status = 'pending', attempts = 0, error_code = NULL,
  claim_token = NULL, locked_until = NULL, workflow_attempt_deadline = NULL,
  finished_at = NULL, updated_at = now()
FROM public.workflow_executions e
WHERE e.workspace_id = sqlc.arg(workspace_id) AND e.id = sqlc.arg(execution_id) AND e.source_kind = 'behavior_run'
  AND r.workspace_id = e.workspace_id AND r.id = e.legacy_run_id
  AND r.status = 'failed';

-- name: ListOrphanAgentBatches :many
-- Workflow-owned batches still processing although no live execution owns
-- them any more (the execution failed or was cancelled before the batch
-- was committed): they block their thread until released.
SELECT j.id AS job_id, e.id AS execution_id, e.state, COALESCE(e.reason_code, '')::text AS reason_code
FROM public.agent_jobs j
JOIN public.workflow_executions e ON e.workspace_id = j.workspace_id AND e.id = j.workflow_execution_id
WHERE j.workspace_id = sqlc.arg(workspace_id) AND j.status = 'processing'
  AND NOT EXISTS (SELECT 1 FROM public.workflow_executions o
                  WHERE o.workspace_id = j.workspace_id AND o.legacy_job_id = j.id
                    AND o.state NOT IN ('COMPLETED','FAILED','CANCELLED'))
  AND NOT EXISTS (SELECT 1 FROM public.workflow_executions c
                  WHERE c.workspace_id = j.workspace_id AND c.legacy_job_id = j.id
                    AND c.state = 'COMPLETED')
ORDER BY j.id
LIMIT sqlc.arg(max_rows)::int;

-- name: CountLiveAgentJobExecutions :one
SELECT COUNT(*)::int FROM public.workflow_executions
WHERE workspace_id = sqlc.arg(workspace_id) AND legacy_job_id = sqlc.arg(job_id)
  AND state NOT IN ('COMPLETED','FAILED','CANCELLED');

-- name: LockOrphanAgentBatch :one
SELECT * FROM public.agent_jobs
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND status = 'processing'
  AND workflow_execution_id = sqlc.arg(execution_id)
FOR UPDATE;
