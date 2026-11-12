-- name: LockToolRunKey :exec
-- Serializes Begin for one idempotency key within the transaction.
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(idempotency_key)::text, 0));

-- name: FindPriorToolRun :one
-- The latest non-read run of a key that must not be re-executed.
SELECT status, COALESCE(result_summary, '')::text AS result_summary
FROM public.tool_runs
WHERE idempotency_key = sqlc.arg(idempotency_key) AND tier <> 'read'
  AND status IN ('started','ok','timeout','unknown_outcome')
ORDER BY (status = 'ok') DESC, id DESC
LIMIT 1;

-- name: InsertToolRun :one
-- A behavior run's row takes the run's correlation ID.
INSERT INTO public.tool_runs(workspace_id, agent_job_id, behavior_run_id, step, tool, tier, repo, account, idempotency_key, input_redacted, status, error_code, correlation_id)
VALUES (sqlc.arg(workspace_id), sqlc.narg(agent_job_id), sqlc.narg(behavior_run_id), sqlc.arg(step), sqlc.arg(tool), sqlc.arg(tier),
        sqlc.narg(repo), sqlc.narg(account), sqlc.arg(idempotency_key), sqlc.narg(input_redacted), sqlc.arg(status), sqlc.narg(error_code),
        (SELECT r.correlation_id FROM public.behavior_runs r
         WHERE r.id = sqlc.narg(behavior_run_id) AND r.workspace_id = sqlc.arg(workspace_id)))
RETURNING id;

-- name: FinishToolRun :execrows
UPDATE public.tool_runs
SET status = sqlc.arg(status), error_code = sqlc.narg(error_code), duration_ms = sqlc.arg(duration_ms),
    output_bytes = sqlc.arg(output_bytes), result_summary = sqlc.narg(result_summary), updated_at = now()
WHERE id = sqlc.arg(id) AND status = 'started';

-- name: ListToolRunsForJob :many
SELECT id, step, tool, tier, status, COALESCE(error_code, '')::text AS error_code, COALESCE(result_summary, '')::text AS result_summary, duration_ms
FROM public.tool_runs WHERE agent_job_id = sqlc.arg(agent_job_id)::bigint ORDER BY id;

-- name: CountJobBudgetRuns :one
-- Non-read runs of a job that consumed write budget: everything except
-- denials and definite failures. Dangerous runs are counted in both.
SELECT COUNT(*)::int AS writes,
       COUNT(*) FILTER (WHERE tier = 'dangerous')::int AS dangerous
FROM public.tool_runs
WHERE agent_job_id = sqlc.arg(agent_job_id)::bigint AND tier <> 'read'
  AND status IN ('started','ok','timeout','unknown_outcome','dry_run');

-- name: CountBehaviorRunBudgetRuns :one
-- The same count as CountJobBudgetRuns for the tool runs of a behavior run.
SELECT COUNT(*)::int AS writes,
       COUNT(*) FILTER (WHERE tier = 'dangerous')::int AS dangerous
FROM public.tool_runs
WHERE behavior_run_id = sqlc.arg(behavior_run_id)::bigint AND tier <> 'read'
  AND status IN ('started','ok','timeout','unknown_outcome','dry_run');

-- name: ListToolRunsForBehaviorRun :many
-- The audit rows of one behavior run, without inputs or result text.
SELECT t.id, t.step, t.tool, t.tier, t.status, COALESCE(t.error_code, '')::text AS error_code,
       COALESCE(t.duration_ms, 0)::bigint AS duration_ms, t.created_at
FROM public.tool_runs t
WHERE t.workspace_id = sqlc.arg(workspace_id) AND t.behavior_run_id = sqlc.arg(behavior_run_id)::bigint
ORDER BY t.id;
