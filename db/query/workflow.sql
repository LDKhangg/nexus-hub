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
  -- A retry child gets a fresh deadline from its own start: the earlier of
  -- the parent's relative deadline and the pinned definition's execution
  -- deadline (LEAST ignores NULL).
  LEAST(CASE WHEN p.deadline_at IS NULL THEN NULL ELSE now() + (p.deadline_at - p.created_at) END,
        now() + sqlc.narg(execution_deadline_us)::bigint * interval '1 microsecond')
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

-- name: InsertWorkflowJob :one
INSERT INTO public.jobs (workspace_id, execution_id, step_key, due_at, max_attempts, attempts, continues_attempt)
VALUES (sqlc.arg(workspace_id), sqlc.arg(execution_id), sqlc.arg(step_key), sqlc.arg(due_at), sqlc.arg(max_attempts), sqlc.arg(attempts),
  sqlc.arg(continues_attempt))
ON CONFLICT (execution_id) WHERE status IN ('pending','processing') DO NOTHING
RETURNING *;

-- name: SelectDueWorkflowJob :one
SELECT j.* FROM public.jobs j
JOIN public.workflow_executions e
  ON e.workspace_id = j.workspace_id AND e.id = j.execution_id
WHERE j.status = 'pending' AND j.due_at <= sqlc.arg(at_time)::timestamptz
  -- A resume job continues the step's current attempt, so it is claimable
  -- even when that attempt was the last one.
  AND (j.attempts < j.max_attempts OR j.continues_attempt)
  AND e.state NOT IN ('WAITING_FOR_USER','WAITING_FOR_APPROVAL','COMPLETED','FAILED','CANCELLED')
  AND (e.deadline_at IS NULL OR e.deadline_at > sqlc.arg(at_time)::timestamptz)
ORDER BY CASE WHEN j.status = 'processing' THEN 0 ELSE 1 END, j.due_at, j.id
-- Lock the job and its execution together without waiting, so a claim never
-- holds a job while blocking on an execution (lock order: execution, job).
FOR UPDATE OF j, e SKIP LOCKED
LIMIT 1;

-- name: ClaimWorkflowJobByID :one
UPDATE public.jobs
SET status = 'processing', attempts = attempts + CASE WHEN continues_attempt THEN 0 ELSE 1 END,
    continues_attempt = false, claim_token = sqlc.arg(claim_token),
    lease_expires_at = sqlc.arg(lease_expires_at), last_error_code = NULL, updated_at = sqlc.arg(at_time)
WHERE id = sqlc.arg(id) AND workspace_id = sqlc.arg(workspace_id)
  AND status IN ('pending','processing')
RETURNING *;

-- name: LockWorkflowJobFence :one
SELECT j.* FROM public.jobs j
JOIN public.workflow_executions e
  ON e.workspace_id = j.workspace_id AND e.id = j.execution_id
WHERE j.workspace_id = sqlc.arg(workspace_id) AND j.id = sqlc.arg(id)
  AND j.status = 'processing' AND j.claim_token = sqlc.arg(claim_token)
  AND j.lease_expires_at > sqlc.arg(at_time)::timestamptz
  AND e.state IN ('TRIAGING','PLANNING','RUNNING','EVALUATING')
  AND e.cancelled_at IS NULL
  AND (e.deadline_at IS NULL OR e.deadline_at > sqlc.arg(at_time)::timestamptz)
FOR UPDATE OF j;

-- name: StartExecutionStepAttempt :one
UPDATE public.execution_steps
SET state = 'RUNNING', attempts = attempts + CASE WHEN sqlc.arg(new_attempt)::boolean THEN 1 ELSE 0 END,
    updated_at = sqlc.arg(at_time)
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id)
  AND step_key = sqlc.arg(step_key) AND state IN ('PENDING','RUNNING')
RETURNING *;

-- name: UpdateExecutionStepProgress :one
UPDATE public.execution_steps
SET checkpoint = COALESCE(sqlc.narg(checkpoint), checkpoint),
    input_tokens = input_tokens + sqlc.arg(input_tokens),
    output_tokens = output_tokens + sqlc.arg(output_tokens),
    cache_read_tokens = cache_read_tokens + sqlc.arg(cache_read_tokens),
    cache_write_tokens = cache_write_tokens + sqlc.arg(cache_write_tokens),
    updated_at = sqlc.arg(at_time)
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id)
  AND step_key = sqlc.arg(step_key) AND state = 'RUNNING' AND attempts = sqlc.arg(expected_attempt)
RETURNING *;

-- name: SetExecutionStepState :one
UPDATE public.execution_steps
SET state = sqlc.arg(state), updated_at = sqlc.arg(at_time)
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id)
  AND step_key = sqlc.arg(step_key)
RETURNING *;

-- name: FinishWorkflowJob :one
UPDATE public.jobs
SET status = sqlc.arg(status), claim_token = NULL, lease_expires_at = NULL,
    last_error_code = sqlc.narg(error_code), dead_letter_reason = sqlc.narg(dead_letter_reason),
    updated_at = sqlc.arg(at_time)
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id)
  AND status = 'processing' AND claim_token = sqlc.arg(claim_token)
RETURNING *;

-- name: RetryWorkflowJob :one
UPDATE public.jobs
SET status = 'pending', due_at = sqlc.arg(due_at), claim_token = NULL, lease_expires_at = NULL,
    last_error_code = sqlc.arg(error_code), updated_at = sqlc.arg(at_time)
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id)
  AND status = 'processing' AND claim_token = sqlc.arg(claim_token)
RETURNING *;

-- name: FindReadyExecutionStep :one
SELECT s.* FROM public.execution_steps s
WHERE s.workspace_id = sqlc.arg(workspace_id) AND s.execution_id = sqlc.arg(execution_id)
  AND s.state = 'PENDING'
  AND NOT EXISTS (
    SELECT 1 FROM unnest(s.depends_on) dependency
    WHERE NOT EXISTS (
      SELECT 1 FROM public.execution_steps completed
      WHERE completed.workspace_id = s.workspace_id AND completed.execution_id = s.execution_id
        AND completed.step_key = dependency AND completed.state = 'SUCCEEDED'))
ORDER BY s.position
LIMIT 1;

-- name: CountUnfinishedExecutionSteps :one
SELECT COUNT(*) FROM public.execution_steps
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id)
  AND state <> 'SUCCEEDED';

-- name: ListReconcileWorkflowExecutions :many
-- Nonterminal executions past their deadline (waiting ones included), and
-- non-waiting ones without a live queue row.
SELECT e.* FROM public.workflow_executions e
WHERE e.state NOT IN ('COMPLETED','FAILED','CANCELLED')
  AND (e.deadline_at IS NOT NULL AND e.deadline_at <= sqlc.arg(at_time)::timestamptz
       OR e.state NOT IN ('WAITING_FOR_USER','WAITING_FOR_APPROVAL')
          AND NOT EXISTS (SELECT 1 FROM public.jobs j WHERE j.execution_id = e.id AND j.status IN ('pending','processing')))
ORDER BY e.updated_at, e.id
FOR UPDATE OF e SKIP LOCKED
LIMIT 100;

-- name: ListExpiredWorkflowJobs :many
SELECT j.* FROM public.jobs j
JOIN public.workflow_executions e ON e.workspace_id = j.workspace_id AND e.id = j.execution_id
WHERE j.status = 'processing' AND j.lease_expires_at <= sqlc.arg(at_time)::timestamptz
  AND e.state NOT IN ('COMPLETED','FAILED','CANCELLED')
ORDER BY j.lease_expires_at, j.id
FOR UPDATE OF j, e SKIP LOCKED
LIMIT 100;

-- name: ResetExpiredWorkflowJob :one
UPDATE public.jobs
SET status = 'pending', due_at = sqlc.arg(due_at), claim_token = NULL, lease_expires_at = NULL,
    last_error_code = sqlc.arg(error_code), updated_at = sqlc.arg(at_time)
WHERE id = sqlc.arg(id) AND workspace_id = sqlc.arg(workspace_id) AND status = 'processing'
RETURNING *;

-- name: TerminateActiveWorkflowJobs :execrows
-- Ends the execution's active job when the execution becomes terminal outside
-- the job-result path. Clearing claim_token and lease keeps a stale worker
-- fenced out; dead_letter with the reason is the terminal job vocabulary.
UPDATE public.jobs
SET status = 'dead_letter', claim_token = NULL, lease_expires_at = NULL,
    last_error_code = sqlc.arg(reason), dead_letter_reason = sqlc.arg(reason), updated_at = sqlc.arg(at_time)
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id)
  AND status IN ('pending','processing');

-- name: GetWorkflowJob :one
SELECT * FROM public.jobs WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id);

-- name: GetActiveWorkflowJobForExecution :one
SELECT * FROM public.jobs
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id)
  AND status IN ('pending','processing')
FOR UPDATE;

-- name: FindReconcileExecutionStep :one
SELECT s.* FROM public.execution_steps s
WHERE s.workspace_id = sqlc.arg(workspace_id) AND s.execution_id = sqlc.arg(execution_id)
  AND (s.state = 'RUNNING' OR (s.state = 'PENDING' AND NOT EXISTS (
    SELECT 1 FROM unnest(s.depends_on) dependency
    WHERE NOT EXISTS (
      SELECT 1 FROM public.execution_steps completed
      WHERE completed.workspace_id = s.workspace_id AND completed.execution_id = s.execution_id
        AND completed.step_key = dependency AND completed.state = 'SUCCEEDED'))))
ORDER BY CASE WHEN s.state = 'RUNNING' THEN 0 ELSE 1 END, s.position
LIMIT 1;

-- name: ListWorkflowDeadLetters :many
SELECT * FROM public.jobs
WHERE workspace_id = sqlc.arg(workspace_id) AND status = 'dead_letter'
ORDER BY updated_at DESC, id DESC
LIMIT sqlc.arg(row_limit);
