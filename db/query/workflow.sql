-- Durable workflow execution lifecycle.

-- name: InsertWorkflowExecution :one
INSERT INTO public.workflow_executions (
  workspace_id, behavior_version_id, trigger_key, correlation_id, state, definition,
  source_kind, source_id, subject, revision, operation_key, legacy_run_id, legacy_job_id, deadline_at)
SELECT sqlc.arg(workspace_id), v.id, sqlc.arg(trigger_key), sqlc.arg(correlation_id), 'RECEIVED', sqlc.arg(definition),
  sqlc.arg(source_kind), sqlc.arg(source_id), sqlc.arg(subject), sqlc.arg(revision), sqlc.arg(operation_key),
  sqlc.narg(legacy_run_id), sqlc.narg(legacy_job_id), sqlc.narg(deadline_at)
FROM public.behavior_versions v
JOIN public.behaviors b ON b.workspace_id = v.workspace_id AND b.id = v.behavior_id
WHERE v.workspace_id = sqlc.arg(workspace_id) AND v.id = sqlc.arg(behavior_version_id) AND b.status = 'enabled'
ON CONFLICT (workspace_id, trigger_key) DO NOTHING
RETURNING *;

-- name: GetWorkflowExecutionByTrigger :one
SELECT * FROM public.workflow_executions
WHERE workspace_id = sqlc.arg(workspace_id) AND trigger_key = sqlc.arg(trigger_key);

-- name: GetWorkflowExecution :one
SELECT * FROM public.workflow_executions
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id);

-- name: LockWorkflowExecution :one
SELECT * FROM public.workflow_executions
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id)
FOR UPDATE;

-- name: InsertExecutionStep :one
INSERT INTO public.execution_steps(workspace_id, execution_id, step_key, position, kind, depends_on)
VALUES (sqlc.arg(workspace_id), sqlc.arg(execution_id), sqlc.arg(step_key), sqlc.arg(position), sqlc.arg(kind), sqlc.arg(depends_on)::text[])
RETURNING *;

-- name: ListExecutionSteps :many
SELECT * FROM public.execution_steps
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id)
ORDER BY position;

-- name: GetExecutionStep :one
SELECT * FROM public.execution_steps
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id)
  AND step_key = sqlc.arg(step_key);

-- name: InsertExecutionEvent :one
INSERT INTO public.execution_events (
  workspace_id, execution_id, sequence, kind, from_state, to_state, step_key, attempt,
  reason_code, idempotency_key, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens)
VALUES (
  sqlc.arg(workspace_id), sqlc.arg(execution_id),
  (SELECT COALESCE(MAX(sequence), 0) + 1 FROM public.execution_events WHERE execution_id = sqlc.arg(execution_id)),
  sqlc.arg(kind), sqlc.narg(from_state), sqlc.narg(to_state), sqlc.narg(step_key), sqlc.arg(attempt),
  sqlc.narg(reason_code), sqlc.narg(idempotency_key), sqlc.arg(input_tokens), sqlc.arg(output_tokens),
  sqlc.arg(cache_read_tokens), sqlc.arg(cache_write_tokens))
RETURNING *;

-- name: GetExecutionEventByIdempotency :one
SELECT * FROM public.execution_events
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id)
  AND idempotency_key = sqlc.arg(idempotency_key);

-- name: UpdateWorkflowExecutionState :one
UPDATE public.workflow_executions
SET state = sqlc.arg(to_state), reason_code = sqlc.narg(reason_code),
    cancelled_at = CASE WHEN sqlc.arg(to_state)::text = 'CANCELLED' THEN now() ELSE cancelled_at END,
    started_at = CASE WHEN sqlc.arg(to_state)::text IN ('TRIAGING','PLANNING','RUNNING','EVALUATING') THEN COALESCE(started_at, now()) ELSE started_at END,
    finished_at = CASE WHEN sqlc.arg(to_state)::text IN ('COMPLETED','FAILED','CANCELLED') THEN now() ELSE NULL END,
    updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND state = sqlc.arg(from_state)
RETURNING *;

-- name: CancelExecutionSteps :execrows
UPDATE public.execution_steps SET state = 'CANCELLED', updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id)
  AND state IN ('PENDING','RUNNING','WAITING');

-- name: RecordExecutionCheckpoint :one
UPDATE public.execution_steps
SET checkpoint = sqlc.arg(checkpoint),
    input_tokens = input_tokens + sqlc.arg(input_tokens),
    output_tokens = output_tokens + sqlc.arg(output_tokens),
    cache_read_tokens = cache_read_tokens + sqlc.arg(cache_read_tokens),
    cache_write_tokens = cache_write_tokens + sqlc.arg(cache_write_tokens),
    updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id)
  AND step_key = sqlc.arg(step_key) AND state = 'RUNNING' AND attempts = sqlc.arg(expected_attempt)
RETURNING *;

-- name: CountUnsafeRetrySteps :one
SELECT COUNT(*) FROM public.execution_steps
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id)
  AND state = 'SUCCEEDED'
  AND (checkpoint IS NULL OR COALESCE((checkpoint->>'reusable')::boolean, false) = false);

-- name: InsertRetryExecution :one
INSERT INTO public.workflow_executions (
  workspace_id, behavior_version_id, trigger_key, correlation_id, state, reason_code, definition,
  source_kind, source_id, subject, revision, operation_key, legacy_run_id, legacy_job_id, parent_execution_id, deadline_at)
SELECT p.workspace_id, p.behavior_version_id, 'retry:' || p.id::text, p.correlation_id, 'RECEIVED', 'manual_retry', p.definition,
  p.source_kind, p.source_id, p.subject, p.revision, p.operation_key, p.legacy_run_id, p.legacy_job_id, p.id,
  CASE WHEN p.deadline_at IS NULL THEN NULL ELSE now() + (p.deadline_at - p.created_at) END
FROM public.workflow_executions p
WHERE p.workspace_id = sqlc.arg(workspace_id) AND p.id = sqlc.arg(parent_execution_id) AND p.state = 'FAILED'
ON CONFLICT (parent_execution_id) DO NOTHING
RETURNING *;

-- name: GetRetryChild :one
SELECT * FROM public.workflow_executions
WHERE workspace_id = sqlc.arg(workspace_id) AND parent_execution_id = sqlc.arg(parent_execution_id);

-- name: CopyRetryExecutionSteps :exec
INSERT INTO public.execution_steps (
  workspace_id, execution_id, step_key, position, kind, depends_on, state, attempts,
  input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, checkpoint)
SELECT s.workspace_id, sqlc.arg(child_execution_id), s.step_key, s.position, s.kind, s.depends_on,
  CASE WHEN s.state = 'SUCCEEDED' THEN 'SUCCEEDED' ELSE 'PENDING' END,
  0, s.input_tokens, s.output_tokens, s.cache_read_tokens, s.cache_write_tokens,
  CASE WHEN s.state = 'SUCCEEDED' THEN s.checkpoint ELSE NULL END
FROM public.execution_steps s
WHERE s.workspace_id = sqlc.arg(workspace_id) AND s.execution_id = sqlc.arg(parent_execution_id)
ORDER BY s.position;
