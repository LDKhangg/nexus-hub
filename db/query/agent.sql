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

-- name: ClaimAgentJob :one
-- A due pending job, or a processing job whose lease (or retry wait) ended;
-- never while another job of the same thread holds a live lease.
WITH candidate AS (
 SELECT j.id FROM public.agent_jobs j
 WHERE ((j.status = 'pending' AND j.run_at <= sqlc.arg(at_time)::timestamptz)
     OR (j.status = 'processing' AND j.locked_until <= sqlc.arg(at_time)::timestamptz))
   AND NOT EXISTS (SELECT 1 FROM public.agent_jobs p
                   WHERE p.thread_id = j.thread_id AND p.id <> j.id AND p.status = 'processing'
                     AND p.locked_until > sqlc.arg(at_time)::timestamptz)
 ORDER BY (j.status = 'processing') DESC, j.run_at, j.id
 FOR UPDATE SKIP LOCKED LIMIT 1
)
UPDATE public.agent_jobs AS jobs SET status = 'processing', attempts = jobs.attempts + 1,
  claim_token = sqlc.arg(token)::uuid,
  locked_until = sqlc.arg(at_time)::timestamptz + make_interval(secs => sqlc.arg(lease_secs)::float8),
  updated_at = now()
FROM candidate WHERE jobs.id = candidate.id
RETURNING jobs.id, jobs.workspace_id, jobs.source_integration_id, jobs.thread_id, jobs.attempts,
  jobs.input_tokens, jobs.output_tokens, jobs.cache_read_tokens, jobs.cache_write_tokens;

-- name: LockAgentClaim :one
SELECT id FROM public.agent_jobs
WHERE id = sqlc.arg(id) AND claim_token = sqlc.arg(token)::uuid AND status = 'processing'
FOR UPDATE;

-- name: AddAgentUsage :execrows
UPDATE public.agent_jobs SET input_tokens = input_tokens + sqlc.arg(input)::int,
  output_tokens = output_tokens + sqlc.arg(output)::int,
  cache_read_tokens = cache_read_tokens + sqlc.arg(cache_read)::int,
  cache_write_tokens = cache_write_tokens + sqlc.arg(cache_write)::int, updated_at = now()
WHERE id = sqlc.arg(id) AND claim_token = sqlc.arg(token)::uuid AND status = 'processing';

-- name: FinishAgentJob :execrows
UPDATE public.agent_jobs SET status = sqlc.arg(status)::text, through_message_id = sqlc.narg(through),
  last_error_code = sqlc.narg(code), claim_token = NULL, locked_until = NULL, updated_at = now()
WHERE id = sqlc.arg(id) AND claim_token = sqlc.arg(token)::uuid AND status = 'processing';

-- name: RetryAgentJob :execrows
-- The job stays processing with its lease set to the retry time, so the
-- thread stays serialized and the one-pending-job index is untouched.
UPDATE public.agent_jobs SET locked_until = sqlc.arg(next_at)::timestamptz, claim_token = NULL,
  last_error_code = sqlc.arg(code)::text, updated_at = now()
WHERE id = sqlc.arg(id) AND claim_token = sqlc.arg(token)::uuid AND status = 'processing';

-- name: LastTriagedMessage :one
SELECT COALESCE(MAX(through_message_id), 0)::bigint FROM public.agent_jobs
WHERE thread_id = $1 AND status IN ('done', 'failed');

-- name: GetAgentThread :one
SELECT title, rolling_summary, external_id FROM public.hub_threads WHERE workspace_id = $1 AND id = $2;

-- name: RecentAgentMessages :many
SELECT m.id, m.sender_role, m.sender_name, m.content, m.metadata FROM (
  SELECT id, sender_role, sender_name, content, metadata FROM public.message_logs
  WHERE thread_id = $1 ORDER BY id DESC LIMIT 60) m
ORDER BY m.id;

-- name: InsertOutboundAction :exec
INSERT INTO public.outbound_actions(workspace_id, integration_id, kind, payload, dedupe_key)
VALUES (sqlc.arg(workspace_id), sqlc.arg(integration_id), 'telegram_message', sqlc.arg(payload), sqlc.arg(dedupe_key))
ON CONFLICT (kind, dedupe_key) DO NOTHING;
