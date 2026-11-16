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
SET last_seen_at = now(), state = CASE WHEN state = 'busy' THEN 'busy' ELSE 'online' END
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
