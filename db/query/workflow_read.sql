-- Workspace-scoped operator reads of durable workflow executions. Every query
-- filters by workspace; none selects approval payloads, options or answers.

-- name: ListWorkflowExecutionsPage :many
SELECT sqlc.embed(e), b.name AS behavior_name, v.version AS behavior_version
FROM public.workflow_executions e
JOIN public.behavior_versions v ON v.workspace_id = e.workspace_id AND v.id = e.behavior_version_id
JOIN public.behaviors b ON b.workspace_id = v.workspace_id AND b.id = v.behavior_id
WHERE e.workspace_id = sqlc.arg(workspace_id)
  AND (sqlc.narg(state)::text IS NULL OR e.state = sqlc.narg(state)::text)
  AND (sqlc.narg(before_id)::bigint IS NULL OR e.id < sqlc.narg(before_id)::bigint)
ORDER BY e.id DESC
LIMIT sqlc.arg(row_limit);

-- name: GetWorkflowExecutionView :one
SELECT sqlc.embed(e), b.name AS behavior_name, v.version AS behavior_version
FROM public.workflow_executions e
JOIN public.behavior_versions v ON v.workspace_id = e.workspace_id AND v.id = e.behavior_version_id
JOIN public.behaviors b ON b.workspace_id = v.workspace_id AND b.id = v.behavior_id
WHERE e.workspace_id = sqlc.arg(workspace_id) AND e.id = sqlc.arg(id);

-- name: ListExecutionEventsPage :many
SELECT * FROM public.execution_events
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id)
  AND sequence > sqlc.arg(after_sequence)
ORDER BY sequence
LIMIT sqlc.arg(row_limit);

-- name: ListWorkflowJobsPage :many
SELECT * FROM public.jobs
WHERE workspace_id = sqlc.arg(workspace_id)
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
  AND (sqlc.narg(execution_id)::bigint IS NULL OR execution_id = sqlc.narg(execution_id)::bigint)
  AND (sqlc.narg(before_id)::bigint IS NULL OR id < sqlc.narg(before_id)::bigint)
ORDER BY id DESC
LIMIT sqlc.arg(row_limit);

-- name: GetLiveWorkflowJobView :one
-- The execution's pending or processing job, read without locking it.
SELECT * FROM public.jobs
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id)
  AND status IN ('pending','processing');

-- name: ListExecutionWaitViews :many
SELECT w.id, w.step_key, w.approval_id, w.wait_kind, w.closed_at, w.created_at,
  a.decision, a.decided_at, a.expires_at,
  (sg.id IS NOT NULL)::boolean AS signalled, sg.consumed_at
FROM public.workflow_waits w
JOIN public.approvals a ON a.workspace_id = w.workspace_id AND a.id = w.approval_id
LEFT JOIN public.workflow_signals sg ON sg.workspace_id = w.workspace_id AND sg.wait_id = w.id
WHERE w.workspace_id = sqlc.arg(workspace_id) AND w.execution_id = sqlc.arg(execution_id)
ORDER BY w.id;

-- name: ListExecutionLinksByLegacyRun :many
SELECT id, state, reason_code, parent_execution_id FROM public.workflow_executions
WHERE workspace_id = sqlc.arg(workspace_id) AND legacy_run_id = sqlc.arg(legacy_run_id)
ORDER BY id;
