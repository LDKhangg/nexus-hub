-- Local tasks (Phase 7a W-4): routes, retry, and the reconcile reads that
-- connect worker jobs to their workflow step. Ids and counters only.

-- name: InsertWorkerTaskRoute :exec
INSERT INTO public.worker_task_routes (workspace_id, execution_id, integration_id, chat_id, thread_id, repo, task_ref, clickup_task_id)
VALUES (sqlc.arg(workspace_id), sqlc.arg(execution_id), sqlc.arg(integration_id), sqlc.arg(chat_id), sqlc.arg(thread_id),
        sqlc.arg(repo), sqlc.arg(task_ref), sqlc.arg(clickup_task_id))
ON CONFLICT (workspace_id, execution_id) DO NOTHING;

-- name: GetWorkerTaskRoute :one
SELECT * FROM public.worker_task_routes
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id);

-- name: SetWorkerTaskProgressMessage :exec
UPDATE public.worker_task_routes
SET progress_message_id = sqlc.arg(progress_message_id), progress_job_attempt = sqlc.arg(progress_job_attempt),
    thread_id = sqlc.arg(thread_id)
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id);

-- name: SetWorkerTaskWaitingNotice :exec
UPDATE public.worker_task_routes
SET waiting_notice = sqlc.arg(waiting_notice)
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id);

-- name: SetWorkerTaskInstructionVersion :exec
-- Moves the delivered instruction version forward only.
UPDATE public.worker_task_routes
SET instruction_version = GREATEST(instruction_version, sqlc.arg(version)::bigint)
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id);

-- name: GetWorkerJobForStep :one
SELECT * FROM public.worker_jobs
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id) AND step_id = sqlc.arg(step_id)
ORDER BY created_at DESC, id
LIMIT 1;

-- name: RetryWorkerJob :one
-- A failed, needs-input or interrupted job of the expected attempt goes back
-- to the queue; device, lease, result and progress are cleared (the attempt
-- counter keeps counting at the next lease). A job whose workflow execution
-- already ended (completed, failed, cancelled) is never queued again.
UPDATE public.worker_jobs j
SET state = 'WAITING_FOR_WORKER', device_id = NULL, lease_token_hash = NULL, lease_expires_at = NULL,
    result = NULL, idempotency_key = NULL, progress = NULL, progress_seq = 0, leased_at = NULL,
    interrupted_at = NULL, finished_at = NULL, updated_at = sqlc.arg(now)
WHERE j.workspace_id = sqlc.arg(workspace_id) AND j.id = sqlc.arg(id) AND j.attempt = sqlc.arg(attempt)
  AND j.state IN ('FAILED', 'NEEDS_INPUT', 'INTERRUPTED')
  AND (j.execution_id IS NULL OR EXISTS (
        SELECT 1 FROM public.workflow_executions e
        WHERE e.workspace_id = j.workspace_id AND e.id = j.execution_id
          AND e.state NOT IN ('COMPLETED', 'FAILED', 'CANCELLED')))
RETURNING *;

-- name: ListWorkerJobsToWake :many
-- Jobs that ended in a way that finishes their workflow step (succeeded or
-- cancelled) while the step still waits: the wake-up was missed.
SELECT j.workspace_id, j.execution_id, j.step_id
FROM public.worker_jobs j
JOIN public.workflow_executions e ON e.workspace_id = j.workspace_id AND e.id = j.execution_id
JOIN public.execution_steps s ON s.workspace_id = j.workspace_id AND s.execution_id = j.execution_id AND s.step_key = j.step_id
WHERE j.state IN ('SUCCEEDED', 'CANCELLED') AND s.state = 'WAITING'
  AND e.state IN ('WAITING_FOR_USER', 'WAITING_FOR_APPROVAL')
ORDER BY j.updated_at
LIMIT sqlc.arg(row_limit);

-- name: ListWorkerJobsOfEndedExecutions :many
-- Unfinished jobs whose execution already ended (cancelled by the owner,
-- failed, past its deadline): they must not run on.
SELECT j.workspace_id, j.id
FROM public.worker_jobs j
JOIN public.workflow_executions e ON e.workspace_id = j.workspace_id AND e.id = j.execution_id
WHERE j.state IN ('WAITING_FOR_WORKER', 'LEASED', 'RUNNING', 'INTERRUPTED')
  AND e.state IN ('COMPLETED', 'FAILED', 'CANCELLED')
ORDER BY j.updated_at
LIMIT sqlc.arg(row_limit);

-- name: ListWorkerTaskRepoConfigs :many
-- The agent configuration documents of the workspace's ClickUp sources; the
-- registered repositories are read from them.
SELECT config FROM public.integrations
WHERE workspace_id = sqlc.arg(workspace_id) AND platform = 'clickup' AND role IN ('source', 'both');

-- name: ResetWorkerTaskInstructionVersion :exec
-- A retried job starts from its payload again, so the instructions given
-- after the payload was built are delivered once more.
UPDATE public.worker_task_routes
SET instruction_version = sqlc.arg(version)::bigint
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id);

-- name: GetLastWorkerTaskRepo :one
-- The repository of the newest local task of one ClickUp task.
SELECT repo FROM public.worker_task_routes
WHERE workspace_id = sqlc.arg(workspace_id) AND clickup_task_id = sqlc.arg(clickup_task_id)
ORDER BY created_at DESC, execution_id DESC
LIMIT 1;
