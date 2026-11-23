-- Phase 12 B1: operations reads for /metrics and nh status, and the LLM
-- spend ledger. Counts, states, ages and codes only; never content.

-- name: OpsExecutionStateCounts :many
-- Executions by state, hub-wide or for one workspace.
SELECT state, count(*)::bigint AS n
FROM public.workflow_executions
WHERE sqlc.narg(workspace_id)::uuid IS NULL OR workspace_id = sqlc.narg(workspace_id)::uuid
GROUP BY state;

-- name: OpsQueueStats :one
-- Queue depth (jobs waiting for a claim) and the age in seconds of the
-- oldest one that is due (0 when none is due).
SELECT count(*)::bigint AS pending,
       COALESCE(EXTRACT(EPOCH FROM (now() - min(due_at) FILTER (WHERE due_at <= now()))), 0)::float8 AS oldest_due_age_s
FROM public.jobs
WHERE status = 'pending'
  AND (sqlc.narg(workspace_id)::uuid IS NULL OR workspace_id = sqlc.narg(workspace_id)::uuid);

-- name: OpsWorkerStats :one
-- Enrolled (not revoked) worker devices, how many are online or busy,
-- and the age in seconds of the newest heartbeat (-1 when none).
SELECT count(*)::bigint AS devices,
       (count(*) FILTER (WHERE state IN ('online', 'busy')))::bigint AS online,
       COALESCE(EXTRACT(EPOCH FROM (now() - max(last_seen_at))), -1)::float8 AS newest_heartbeat_age_s
FROM public.worker_devices
WHERE revoked_at IS NULL
  AND (sqlc.narg(workspace_id)::uuid IS NULL OR workspace_id = sqlc.narg(workspace_id)::uuid);

-- name: UpsertLLMUsageDaily :exec
-- Adds one call's tokens and estimated cost to the day's ledger row.
INSERT INTO public.llm_usage_daily (workspace_id, day, provider, model, input_tokens, output_tokens, cost_usd, updated_at)
VALUES (sqlc.arg(workspace_id), sqlc.arg(day), sqlc.arg(provider), sqlc.arg(model), sqlc.arg(input_tokens), sqlc.arg(output_tokens), sqlc.arg(cost_usd), now())
ON CONFLICT (workspace_id, day, provider, model) DO UPDATE
SET input_tokens = llm_usage_daily.input_tokens + EXCLUDED.input_tokens,
    output_tokens = llm_usage_daily.output_tokens + EXCLUDED.output_tokens,
    cost_usd = llm_usage_daily.cost_usd + EXCLUDED.cost_usd,
    updated_at = now();

-- name: SumLLMCostForDay :one
-- A workspace's estimated model spend of one UTC day.
SELECT COALESCE(sum(cost_usd), 0)::float8 AS cost_usd
FROM public.llm_usage_daily
WHERE workspace_id = sqlc.arg(workspace_id) AND day = sqlc.arg(day);

-- name: ListLLMUsageForDay :many
-- Tokens and estimated cost of one UTC day by provider and model,
-- hub-wide or for one workspace.
SELECT provider, model,
       sum(input_tokens)::bigint AS input_tokens,
       sum(output_tokens)::bigint AS output_tokens,
       sum(cost_usd)::float8 AS cost_usd
FROM public.llm_usage_daily
WHERE day = sqlc.arg(day)
  AND (sqlc.narg(workspace_id)::uuid IS NULL OR workspace_id = sqlc.narg(workspace_id)::uuid)
GROUP BY provider, model
ORDER BY provider, model;

-- name: OpsFailedExecutionReasons :many
-- Executions that FAILED since a time, by reason code ('' when none),
-- hub-wide or for one workspace.
SELECT COALESCE(reason_code, '')::text AS reason_code, count(*)::bigint AS n
FROM public.workflow_executions
WHERE state = 'FAILED' AND finished_at >= sqlc.arg(since)::timestamptz
  AND (sqlc.narg(workspace_id)::uuid IS NULL OR workspace_id = sqlc.narg(workspace_id)::uuid)
GROUP BY 1
ORDER BY 1;

-- name: OpsDeadLetterReasons :many
-- Dead-letter workflow jobs by reason code, hub-wide or for one workspace.
SELECT COALESCE(dead_letter_reason, '')::text AS reason_code, count(*)::bigint AS n
FROM public.jobs
WHERE status = 'dead_letter'
  AND (sqlc.narg(workspace_id)::uuid IS NULL OR workspace_id = sqlc.narg(workspace_id)::uuid)
GROUP BY 1
ORDER BY 1;
