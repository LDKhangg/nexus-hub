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

-- name: SelectAgentClaimCandidate :one
-- A due pending job, or a processing job whose lease (or retry wait) ended;
-- never while another job of the same thread holds a live lease. The row
-- lock alone does not serialize a thread: Claim also takes the thread's
-- advisory lock and re-checks with CheckAgentThreadLease.
-- Jobs of a cut-over workspace and workflow-owned jobs are never
-- candidates; the claim re-checks the workspace's runtime mode under its
-- KEY SHARE lock (this filter only skips them cheaply).
SELECT j.id, j.thread_id, j.workspace_id FROM public.agent_jobs j
WHERE ((j.status = 'pending' AND j.run_at <= sqlc.arg(at_time)::timestamptz)
    OR (j.status = 'processing' AND j.locked_until <= sqlc.arg(at_time)::timestamptz))
  AND j.workflow_execution_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM public.workflow_runtime_modes m
                  WHERE m.workspace_id = j.workspace_id AND m.mode = 'workflow')
  AND NOT EXISTS (SELECT 1 FROM public.agent_jobs p
                  WHERE p.thread_id = j.thread_id AND p.id <> j.id AND p.status = 'processing'
                    AND p.locked_until > sqlc.arg(at_time)::timestamptz)
ORDER BY (j.status = 'processing') DESC, j.run_at, j.id
FOR UPDATE OF j SKIP LOCKED LIMIT 1;

-- name: TryLockAgentThread :one
-- Serializes claims of one thread within the transaction.
SELECT pg_try_advisory_xact_lock(hashtextextended('agent_thread:' || sqlc.arg(thread_id)::uuid::text, 0))::bool AS locked;

-- name: CheckAgentThreadLease :one
-- Whether another job of the thread holds a live processing lease, seen in
-- a fresh snapshot after the thread lock was taken.
SELECT EXISTS (SELECT 1 FROM public.agent_jobs p
               WHERE p.thread_id = sqlc.arg(thread_id)::uuid AND p.id <> sqlc.arg(id)
                 AND p.status = 'processing' AND p.locked_until > sqlc.arg(at_time)::timestamptz)::bool AS busy;

-- name: ClaimAgentJobByID :one
UPDATE public.agent_jobs AS jobs SET status = 'processing', attempts = jobs.attempts + 1,
  claim_token = sqlc.arg(token)::uuid,
  locked_until = sqlc.arg(at_time)::timestamptz + make_interval(secs => sqlc.arg(lease_secs)::float8),
  updated_at = now()
WHERE jobs.id = sqlc.arg(id)
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

-- name: UntriagedAgentMessages :many
-- The oldest untriaged external_user messages, so a batch is never pushed
-- out of the recent window by later messages.
SELECT id, sender_role, sender_name, content, metadata FROM public.message_logs
WHERE thread_id = sqlc.arg(thread_id)::uuid AND sender_role = 'external_user' AND id > sqlc.arg(after_id)::bigint
ORDER BY id ASC LIMIT 100;

-- name: EnqueueAgentFollowUp :execrows
-- A due pending job for the thread when external_user messages past the
-- committed batch remain; an existing pending job is kept as is.
INSERT INTO public.agent_jobs(workspace_id, source_integration_id, thread_id, run_at)
SELECT sqlc.arg(workspace_id)::uuid, sqlc.arg(source_integration_id)::uuid, sqlc.arg(thread_id)::uuid, now()
WHERE EXISTS (SELECT 1 FROM public.message_logs m
              WHERE m.thread_id = sqlc.arg(thread_id)::uuid AND m.sender_role = 'external_user'
                AND m.id > sqlc.arg(through)::bigint)
ON CONFLICT (thread_id) WHERE status = 'pending' DO NOTHING;

-- name: InsertOutboundAction :exec
INSERT INTO public.outbound_actions(workspace_id, integration_id, kind, payload, dedupe_key)
VALUES (sqlc.arg(workspace_id), sqlc.arg(integration_id), 'telegram_message', sqlc.arg(payload), sqlc.arg(dedupe_key))
ON CONFLICT (kind, dedupe_key) DO NOTHING;

-- name: GetThreadSummary :one
SELECT rolling_summary, summary_through_message_id FROM public.hub_threads WHERE workspace_id = $1 AND id = $2;

-- name: SummaryCandidates :many
-- Messages past the summary cursor, excluding the thread's newest three.
SELECT m.id, m.sender_role, m.sender_name, m.content FROM public.message_logs m
WHERE m.thread_id = sqlc.arg(thread)::uuid AND m.id > sqlc.arg(cursor)::bigint
  AND m.id < COALESCE((SELECT MIN(n.id) FROM (SELECT r.id FROM public.message_logs r
                       WHERE r.thread_id = sqlc.arg(thread)::uuid ORDER BY r.id DESC LIMIT 3) n), 0)
ORDER BY m.id LIMIT 40;

-- name: SaveThreadSummary :execrows
UPDATE public.hub_threads SET rolling_summary = sqlc.arg(summary), summary_through_message_id = sqlc.arg(through)::bigint, updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id)
  AND COALESCE(summary_through_message_id, 0) = sqlc.arg(expected)::bigint;

-- Durable comment pipeline (workflow-owned agent batches). Every query is
-- workspace scoped. A workflow-owned batch is processing with no legacy
-- claim token and a lease that never ends, so the legacy claimer neither
-- takes it nor claims another job of its thread.

-- name: SelectDueAgentBatch :one
-- The oldest due pending batch of the workspace whose thread has no
-- processing job at all (legacy or workflow-owned), or, first, a job a
-- legacy claimer left processing (imported by the cutover) whose lease or
-- retry wait has ended: an unexpired legacy lease stays unavailable.
SELECT j.id, j.thread_id, j.source_integration_id, j.status FROM public.agent_jobs j
WHERE j.workspace_id = sqlc.arg(workspace_id)::uuid AND j.workflow_execution_id IS NULL
  AND ((j.status = 'pending' AND j.run_at <= sqlc.arg(at_time)::timestamptz)
    OR (j.status = 'processing' AND j.locked_until <= sqlc.arg(at_time)::timestamptz))
  AND NOT EXISTS (SELECT 1 FROM public.agent_jobs p
                  WHERE p.workspace_id = j.workspace_id AND p.thread_id = j.thread_id
                    AND p.id <> j.id AND p.status = 'processing')
ORDER BY (j.status = 'processing') DESC, j.run_at, j.id
FOR UPDATE SKIP LOCKED LIMIT 1;

-- name: AgentThreadBusy :one
-- Whether another job of the thread is processing, seen after the thread's
-- advisory lock was taken.
SELECT EXISTS (SELECT 1 FROM public.agent_jobs p
               WHERE p.workspace_id = sqlc.arg(workspace_id)::uuid AND p.thread_id = sqlc.arg(thread_id)::uuid
                 AND p.id <> sqlc.arg(id) AND p.status = 'processing')::bool AS busy;

-- name: AdoptAgentBatch :one
-- Hands a due pending batch, or a legacy-processing job whose lease ended,
-- to its workflow execution. The legacy claim token is cleared, so a late
-- legacy claimant's writes fail.
UPDATE public.agent_jobs SET status = 'processing', workflow_execution_id = sqlc.arg(execution_id),
  claim_token = NULL, locked_until = 'infinity'::timestamptz, workflow_attempt_deadline = NULL, updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND workflow_execution_id IS NULL
  AND (status = 'pending' OR (status = 'processing' AND locked_until <= sqlc.arg(at_time)::timestamptz))
RETURNING *;

-- name: GetAgentBatch :one
SELECT * FROM public.agent_jobs WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id);

-- name: LockDurableAgentBatch :one
-- The batch of a workflow execution (or of its retry child, which keeps
-- the legacy job reference), locked for a fenced write.
SELECT j.* FROM public.agent_jobs j
JOIN public.workflow_executions e ON e.workspace_id = j.workspace_id AND e.legacy_job_id = j.id
WHERE j.workspace_id = sqlc.arg(workspace_id) AND j.id = sqlc.arg(id) AND e.id = sqlc.arg(execution_id)
  AND e.source_kind = 'agent_job' AND j.workflow_execution_id IS NOT NULL
FOR UPDATE OF j;

-- name: BeginDurableAgentAttempt :one
UPDATE public.agent_jobs SET attempts = attempts + 1, workflow_attempt_deadline = sqlc.arg(deadline)::timestamptz, updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND status = 'processing'
  AND workflow_execution_id IS NOT NULL
RETURNING *;

-- name: AddDurableAgentUsage :execrows
UPDATE public.agent_jobs SET input_tokens = input_tokens + sqlc.arg(input)::int,
  output_tokens = output_tokens + sqlc.arg(output)::int,
  cache_read_tokens = cache_read_tokens + sqlc.arg(cache_read)::int,
  cache_write_tokens = cache_write_tokens + sqlc.arg(cache_write)::int, updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND status = 'processing'
  AND workflow_execution_id IS NOT NULL;

-- name: RetryDurableAgentBatch :execrows
-- Records why the attempt is tried again and ends its deadline; the
-- workflow job owns the schedule.
UPDATE public.agent_jobs SET last_error_code = sqlc.arg(code)::text, workflow_attempt_deadline = NULL, updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND status = 'processing'
  AND workflow_execution_id IS NOT NULL;

-- name: FinishDurableAgentBatch :execrows
UPDATE public.agent_jobs SET status = sqlc.arg(status)::text, through_message_id = sqlc.narg(through),
  last_error_code = sqlc.narg(code), locked_until = NULL, workflow_attempt_deadline = NULL, updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND status = 'processing'
  AND workflow_execution_id IS NOT NULL;

-- name: InsertAgentBatchResult :one
INSERT INTO public.agent_batch_results(
  workspace_id, agent_job_id, execution_id, stage, outcome, through_message_id, last_triaged_id, max_message_id,
  category, urgency, confidence, summary, goal, router_note, tools_available, unseen_count,
  report_text, tools_line, total_input_tokens, total_output_tokens)
VALUES (sqlc.arg(workspace_id), sqlc.arg(agent_job_id), sqlc.arg(execution_id), sqlc.arg(stage), sqlc.arg(outcome),
  sqlc.arg(through_message_id), sqlc.arg(last_triaged_id), sqlc.arg(max_message_id),
  sqlc.arg(category), sqlc.arg(urgency), sqlc.arg(confidence), sqlc.arg(summary), sqlc.arg(goal), sqlc.arg(router_note),
  sqlc.arg(tools_available), sqlc.arg(unseen_count), sqlc.arg(report_text), sqlc.arg(tools_line),
  sqlc.arg(total_input_tokens), sqlc.arg(total_output_tokens))
ON CONFLICT (workspace_id, agent_job_id, stage) DO NOTHING
RETURNING id;

-- name: GetAgentBatchResultID :one
SELECT id FROM public.agent_batch_results
WHERE workspace_id = sqlc.arg(workspace_id) AND agent_job_id = sqlc.arg(agent_job_id) AND stage = sqlc.arg(stage);

-- name: ListAgentBatchResults :many
SELECT * FROM public.agent_batch_results
WHERE workspace_id = sqlc.arg(workspace_id) AND agent_job_id = sqlc.arg(agent_job_id)
ORDER BY id;

-- name: InsertInternalBehavior :exec
-- Creates the workspace's internal definition once; it is enabled so its
-- versions can start executions, and immutable afterwards.
INSERT INTO public.behaviors(workspace_id, name, status, draft, internal_kind)
VALUES (sqlc.arg(workspace_id), 'internal:' || sqlc.arg(internal_kind)::text, 'enabled', '{}'::jsonb, sqlc.arg(internal_kind)::text)
ON CONFLICT DO NOTHING;

-- name: LockInternalBehavior :one
-- Serializes new internal versions of one workspace.
SELECT * FROM public.behaviors
WHERE workspace_id = sqlc.arg(workspace_id) AND internal_kind = sqlc.arg(internal_kind)::text
FOR UPDATE;

-- name: LatestInternalBehaviorVersion :one
SELECT v.* FROM public.behavior_versions v
WHERE v.workspace_id = sqlc.arg(workspace_id) AND v.behavior_id = sqlc.arg(behavior_id)
ORDER BY v.version DESC LIMIT 1;

-- name: InsertInternalBehaviorVersion :one
INSERT INTO public.behavior_versions(workspace_id, behavior_id, version, document, vars, source)
VALUES (sqlc.arg(workspace_id), sqlc.arg(behavior_id), sqlc.arg(version), sqlc.arg(document), '{}'::jsonb, '')
RETURNING *;

-- name: GetInternalBehaviorVersion :one
SELECT v.* FROM public.behavior_versions v
JOIN public.behaviors b ON b.workspace_id = v.workspace_id AND b.id = v.behavior_id
WHERE v.workspace_id = sqlc.arg(workspace_id) AND v.id = sqlc.arg(id) AND b.internal_kind = sqlc.arg(internal_kind)::text;
