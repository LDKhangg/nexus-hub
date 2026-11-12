-- name: UpsertPendingAgentJob :one
INSERT INTO public.agent_jobs(workspace_id, source_integration_id, thread_id, run_at)
VALUES (sqlc.arg(workspace_id), sqlc.arg(source_integration_id), sqlc.arg(thread_id),
        now() + LEAST(make_interval(secs => sqlc.arg(window_secs)::float8), make_interval(secs => sqlc.arg(max_wait_secs)::float8)))
ON CONFLICT (thread_id) WHERE status = 'pending'
DO UPDATE SET run_at = LEAST(now() + make_interval(secs => sqlc.arg(window_secs)::float8),
                             public.agent_jobs.first_enqueued_at + make_interval(secs => sqlc.arg(max_wait_secs)::float8)),
              updated_at = now()
RETURNING id;

-- name: SaveAgentMessage :one
-- Inserts a source message once; a duplicate callback returns no row.
INSERT INTO public.message_logs(thread_id, external_message_id, sender_id, sender_name, sender_role, content, occurred_at, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (thread_id, external_message_id) DO NOTHING
RETURNING id;
