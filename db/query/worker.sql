-- Local worker bridge: devices, enrollment codes and audit. Hashes only.

-- name: CreateWorkerEnrollCode :exec
INSERT INTO public.worker_enroll_codes (workspace_id, code_hash, expires_at, created_by)
VALUES (sqlc.arg(workspace_id), sqlc.arg(code_hash), sqlc.arg(expires_at), sqlc.arg(created_by));

-- name: RedeemWorkerEnrollCode :one
-- Single use: only an unused, unexpired code returns its workspace.
UPDATE public.worker_enroll_codes
SET used_at = now()
WHERE code_hash = sqlc.arg(code_hash) AND used_at IS NULL AND expires_at > now()
RETURNING workspace_id;

-- name: CreateWorkerDevice :one
INSERT INTO public.worker_devices (workspace_id, name, token_hash, capabilities)
VALUES (sqlc.arg(workspace_id), sqlc.arg(name), sqlc.arg(token_hash), sqlc.arg(capabilities))
RETURNING *;

-- name: AuthenticateWorkerDevice :one
-- Matches an active device by token hash and records contact.
UPDATE public.worker_devices
SET last_seen_at = now(),
    state = CASE WHEN EXISTS (
        SELECT 1 FROM public.worker_jobs j
        WHERE j.device_id = worker_devices.id AND j.state IN ('LEASED', 'RUNNING', 'CANCELLING')
    ) THEN 'busy' ELSE 'online' END
WHERE token_hash = sqlc.arg(token_hash) AND revoked_at IS NULL
RETURNING *;

-- name: ListWorkerDevices :many
SELECT * FROM public.worker_devices
WHERE workspace_id = sqlc.arg(workspace_id)
ORDER BY (revoked_at IS NOT NULL), name, created_at;

-- name: RevokeWorkerDevice :one
UPDATE public.worker_devices
SET revoked_at = now(), state = 'offline'
WHERE workspace_id = sqlc.arg(workspace_id) AND name = sqlc.arg(name) AND revoked_at IS NULL
RETURNING *;

-- name: InsertWorkerEvent :exec
INSERT INTO public.worker_events (workspace_id, device_id, job_id, kind, reason_code)
VALUES (sqlc.arg(workspace_id), sqlc.arg(device_id), sqlc.narg(job_id), sqlc.arg(kind), sqlc.arg(reason_code));

-- Jobs (lease protocol, Phase 7a W-2). Payloads hold ids and bounded context
-- only; results are the structured contract.

-- name: CreateWorkerJob :one
INSERT INTO public.worker_jobs (workspace_id, execution_id, step_id, required_capabilities, payload)
VALUES (sqlc.arg(workspace_id), sqlc.narg(execution_id), sqlc.arg(step_id), sqlc.arg(required_capabilities), sqlc.arg(payload))
RETURNING *;

-- name: GetWorkerJob :one
SELECT * FROM public.worker_jobs
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id);

-- name: LockWorkerJob :one
SELECT * FROM public.worker_jobs
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id)
FOR UPDATE;

-- name: ListWorkerJobs :many
SELECT * FROM public.worker_jobs
WHERE workspace_id = sqlc.arg(workspace_id)
ORDER BY created_at DESC, id
LIMIT sqlc.arg(row_limit);

-- name: LeaseWorkerJob :one
-- Leases the oldest waiting job of the workspace whose required capabilities
-- are a subset of the device's, unless the device already holds an active
-- job. SKIP LOCKED keeps concurrent pollers off the same row; the unique
-- index worker_jobs_one_active_per_device is the last guard.
WITH candidate AS (
    SELECT c.id FROM public.worker_jobs c
    WHERE c.workspace_id = sqlc.arg(workspace_id)
      AND c.state = 'WAITING_FOR_WORKER'
      AND c.required_capabilities <@ sqlc.arg(capabilities)::jsonb
      AND NOT EXISTS (
          SELECT 1 FROM public.worker_jobs a
          WHERE a.device_id = sqlc.arg(device_id) AND a.state IN ('LEASED', 'RUNNING', 'CANCELLING')
      )
    ORDER BY c.created_at, c.id
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
UPDATE public.worker_jobs j
SET state = 'LEASED', device_id = sqlc.arg(device_id), lease_token_hash = sqlc.arg(lease_token_hash),
    lease_expires_at = sqlc.arg(lease_expires_at), attempt = j.attempt + 1, leased_at = sqlc.arg(now),
    interrupted_at = NULL, progress = NULL, progress_seq = 0, updated_at = sqlc.arg(now)
FROM candidate
WHERE j.id = candidate.id AND j.state = 'WAITING_FOR_WORKER'
RETURNING j.*;

-- name: ReissueWorkerLease :one
-- Rotates the lease token of the job the device holds in LEASED state, i.e.
-- leased but never heartbeated nor reported: the poll response that carried
-- the first token was lost. Same attempt; RUNNING or CANCELLING jobs are not
-- touched. Only a poll that started after the lease was issued may rotate
-- it (leased_at < poll_started_at), so a stale poll still open on a dead
-- connection never replaces a token already delivered. leased_at moves to
-- now: a reissue is a new issuance. The unique index
-- worker_jobs_one_active_per_device keeps it to one row.
UPDATE public.worker_jobs
SET lease_token_hash = sqlc.arg(lease_token_hash), lease_expires_at = sqlc.arg(lease_expires_at),
    leased_at = sqlc.arg(now), updated_at = sqlc.arg(now)
WHERE workspace_id = sqlc.arg(workspace_id) AND device_id = sqlc.arg(device_id) AND state = 'LEASED'
  AND leased_at < sqlc.arg(poll_started_at)
RETURNING *;

-- name: UpdateWorkerJobLease :one
-- Extends a held lease (heartbeat, progress) and resumes an interrupted job.
UPDATE public.worker_jobs
SET lease_expires_at = sqlc.arg(lease_expires_at), state = sqlc.arg(state),
    interrupted_at = NULL, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: UpdateWorkerJobProgress :one
UPDATE public.worker_jobs
SET progress = sqlc.arg(progress), progress_seq = sqlc.arg(progress_seq), lease_expires_at = sqlc.arg(lease_expires_at),
    state = sqlc.arg(state), interrupted_at = NULL, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: FinishWorkerJob :one
-- Records the result. The lease token hash stays so a retried upload with the
-- same idempotency key can be matched.
UPDATE public.worker_jobs
SET state = sqlc.arg(state), result = sqlc.narg(result), idempotency_key = sqlc.narg(idempotency_key),
    lease_expires_at = NULL, interrupted_at = NULL, finished_at = sqlc.arg(now), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: CancelWorkerJob :one
-- Waiting or interrupted jobs are cancelled at once; leased or running ones
-- become CANCELLING until the worker reports. A failed or needs-input job
-- (the owner's Cancel button next to Retry) is cancelled at once too, so its
-- workflow step ends (Phase 10 REQ-12).
UPDATE public.worker_jobs
SET state = CASE WHEN state IN ('WAITING_FOR_WORKER', 'INTERRUPTED', 'FAILED', 'NEEDS_INPUT') THEN 'CANCELLED' ELSE 'CANCELLING' END,
    finished_at = CASE WHEN state IN ('WAITING_FOR_WORKER', 'INTERRUPTED', 'FAILED', 'NEEDS_INPUT') THEN sqlc.arg(now) ELSE finished_at END,
    lease_expires_at = CASE WHEN state IN ('WAITING_FOR_WORKER', 'INTERRUPTED', 'FAILED', 'NEEDS_INPUT') THEN NULL ELSE lease_expires_at END,
    updated_at = sqlc.arg(now)
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id)
  AND state IN ('WAITING_FOR_WORKER', 'LEASED', 'RUNNING', 'CANCELLING', 'INTERRUPTED', 'FAILED', 'NEEDS_INPUT')
RETURNING *;

-- name: DeviceHasOtherActiveWorkerJob :one
SELECT EXISTS (
    SELECT 1 FROM public.worker_jobs
    WHERE device_id = sqlc.arg(device_id) AND id <> sqlc.arg(job_id) AND state IN ('LEASED', 'RUNNING', 'CANCELLING')
);

-- name: ExpireWorkerLeases :many
-- An expired lease interrupts the job (W7); a job already cancelling is
-- cancelled instead.
UPDATE public.worker_jobs
SET state = CASE WHEN state = 'CANCELLING' THEN 'CANCELLED' ELSE 'INTERRUPTED' END,
    interrupted_at = CASE WHEN state = 'CANCELLING' THEN NULL ELSE sqlc.arg(now)::timestamptz END,
    finished_at = CASE WHEN state = 'CANCELLING' THEN sqlc.arg(now)::timestamptz ELSE NULL END,
    lease_expires_at = NULL, updated_at = sqlc.arg(now)::timestamptz
WHERE state IN ('LEASED', 'RUNNING', 'CANCELLING') AND lease_expires_at < sqlc.arg(now)::timestamptz
RETURNING *;

-- name: RefreshWorkerDeviceState :exec
UPDATE public.worker_devices d
SET state = CASE
        WHEN EXISTS (
            SELECT 1 FROM public.worker_jobs j
            WHERE j.device_id = d.id AND j.state IN ('LEASED', 'RUNNING', 'CANCELLING')
        ) THEN 'busy'
        WHEN d.last_seen_at IS NOT NULL AND d.last_seen_at >= sqlc.arg(online_after)::timestamptz THEN 'online'
        ELSE 'offline' END
WHERE d.id = sqlc.arg(device_id) AND d.revoked_at IS NULL;

-- name: MarkWorkerDevicesOffline :many
UPDATE public.worker_devices
SET state = 'offline'
WHERE revoked_at IS NULL AND state <> 'offline'
  AND (last_seen_at IS NULL OR last_seen_at < sqlc.arg(cutoff))
RETURNING id, workspace_id;

-- name: ListWorkerEvents :many
SELECT * FROM public.worker_events
WHERE workspace_id = sqlc.arg(workspace_id)
ORDER BY id DESC
LIMIT sqlc.arg(row_limit);

-- name: UpsertWorkerTelemetry :exec
INSERT INTO public.worker_telemetry (device_id, workspace_id, snapshot, received_at)
VALUES (sqlc.arg(device_id), sqlc.arg(workspace_id), sqlc.arg(snapshot), sqlc.arg(received_at))
ON CONFLICT (device_id) DO UPDATE
SET snapshot = EXCLUDED.snapshot, received_at = EXCLUDED.received_at;

-- name: GetWorkerTelemetry :one
SELECT * FROM public.worker_telemetry
WHERE device_id = sqlc.arg(device_id);

-- name: ListWorkerTelemetry :many
SELECT * FROM public.worker_telemetry
WHERE workspace_id = sqlc.arg(workspace_id);

-- name: UpsertWorkerTelemetryHourly :exec
INSERT INTO public.worker_telemetry_hourly (
    device_id, hour, workspace_id, samples, cpu_load_sum, cpu_load_max, ram_used_pct_max,
    disk_free_min_bytes, claude_session_left_min, claude_weekly_left_min,
    jobs_total, failures_total, executor_tokens_total, busy_seconds_total)
VALUES (
    sqlc.arg(device_id), sqlc.arg(hour), sqlc.arg(workspace_id), 1, COALESCE(sqlc.narg(cpu_load)::double precision, 0),
    sqlc.narg(cpu_load)::double precision, sqlc.narg(ram_used_pct)::double precision, sqlc.narg(disk_free_bytes)::bigint,
    sqlc.narg(claude_session_left)::smallint, sqlc.narg(claude_weekly_left)::smallint,
    sqlc.narg(jobs_total)::bigint, sqlc.narg(failures_total)::bigint, sqlc.narg(executor_tokens_total)::bigint, sqlc.narg(busy_seconds_total)::bigint)
ON CONFLICT (device_id, hour) DO UPDATE SET
    samples = worker_telemetry_hourly.samples + 1,
    cpu_load_sum = worker_telemetry_hourly.cpu_load_sum + COALESCE(EXCLUDED.cpu_load_max, 0),
    cpu_load_max = GREATEST(worker_telemetry_hourly.cpu_load_max, EXCLUDED.cpu_load_max),
    ram_used_pct_max = GREATEST(worker_telemetry_hourly.ram_used_pct_max, EXCLUDED.ram_used_pct_max),
    disk_free_min_bytes = LEAST(worker_telemetry_hourly.disk_free_min_bytes, EXCLUDED.disk_free_min_bytes),
    claude_session_left_min = LEAST(worker_telemetry_hourly.claude_session_left_min, EXCLUDED.claude_session_left_min),
    claude_weekly_left_min = LEAST(worker_telemetry_hourly.claude_weekly_left_min, EXCLUDED.claude_weekly_left_min),
    jobs_total = GREATEST(worker_telemetry_hourly.jobs_total, EXCLUDED.jobs_total),
    failures_total = GREATEST(worker_telemetry_hourly.failures_total, EXCLUDED.failures_total),
    executor_tokens_total = GREATEST(worker_telemetry_hourly.executor_tokens_total, EXCLUDED.executor_tokens_total),
    busy_seconds_total = GREATEST(worker_telemetry_hourly.busy_seconds_total, EXCLUDED.busy_seconds_total);

-- name: PruneWorkerTelemetryHourly :execrows
DELETE FROM public.worker_telemetry_hourly
WHERE hour < sqlc.arg(before);
