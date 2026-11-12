-- Behavior definitions and versions.

-- name: UpsertBehaviorDraft :one
INSERT INTO public.behaviors(workspace_id, name, draft, draft_vars, draft_source)
VALUES (sqlc.arg(workspace_id), sqlc.arg(name), sqlc.arg(draft), sqlc.arg(draft_vars), sqlc.arg(draft_source))
ON CONFLICT (workspace_id, name) DO UPDATE
SET draft = EXCLUDED.draft, draft_vars = EXCLUDED.draft_vars, draft_source = EXCLUDED.draft_source, updated_at = now()
RETURNING *;

-- name: GetBehaviorByName :one
SELECT * FROM public.behaviors WHERE workspace_id = sqlc.arg(workspace_id) AND name = sqlc.arg(name);

-- name: GetBehaviorByID :one
SELECT * FROM public.behaviors WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id);

-- name: LockBehaviorByName :one
-- Serializes publish and status changes of one behavior.
SELECT * FROM public.behaviors WHERE workspace_id = sqlc.arg(workspace_id) AND name = sqlc.arg(name) FOR UPDATE;

-- name: ListBehaviors :many
SELECT b.id, b.workspace_id, b.name, b.status, b.created_at, b.updated_at,
  COALESCE((SELECT MAX(v.version) FROM public.behavior_versions v WHERE v.behavior_id = b.id), 0)::int AS latest_version
FROM public.behaviors b
WHERE b.workspace_id = sqlc.arg(workspace_id)
ORDER BY b.name;

-- name: NextBehaviorVersion :one
SELECT (COALESCE(MAX(version), 0) + 1)::int AS next_version FROM public.behavior_versions WHERE behavior_id = sqlc.arg(behavior_id);

-- name: InsertBehaviorVersion :one
INSERT INTO public.behavior_versions(workspace_id, behavior_id, version, document, vars, source)
VALUES (sqlc.arg(workspace_id), sqlc.arg(behavior_id), sqlc.arg(version), sqlc.arg(document), sqlc.arg(vars), sqlc.arg(source))
RETURNING *;

-- name: SetBehaviorStatus :execrows
UPDATE public.behaviors SET status = sqlc.arg(status)::text, updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id);

-- name: GetBehaviorVersion :one
SELECT * FROM public.behavior_versions
WHERE workspace_id = sqlc.arg(workspace_id) AND behavior_id = sqlc.arg(behavior_id) AND version = sqlc.arg(version);

-- name: GetLatestBehaviorVersion :one
SELECT * FROM public.behavior_versions
WHERE workspace_id = sqlc.arg(workspace_id) AND behavior_id = sqlc.arg(behavior_id)
ORDER BY version DESC LIMIT 1;

-- name: GetBehaviorVersionByID :one
SELECT * FROM public.behavior_versions WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id);

-- name: ListBehaviorVersions :many
SELECT id, workspace_id, behavior_id, version, published_at FROM public.behavior_versions
WHERE workspace_id = sqlc.arg(workspace_id) AND behavior_id = sqlc.arg(behavior_id)
ORDER BY version;

-- name: ListEnabledBehaviorVersions :many
-- The latest published version of every enabled behavior.
SELECT DISTINCT ON (b.id) b.name, v.id, v.workspace_id, v.behavior_id, v.version, v.document, v.vars, v.published_at
FROM public.behaviors b
JOIN public.behavior_versions v ON v.workspace_id = b.workspace_id AND v.behavior_id = b.id
WHERE b.workspace_id = sqlc.arg(workspace_id) AND b.status = 'enabled'
ORDER BY b.id, v.version DESC;

-- Runs.

-- name: CreateBehaviorRun :one
-- A second run for the same (version, subject, revision) returns no row.
INSERT INTO public.behavior_runs(workspace_id, behavior_id, behavior_version_id, version, trigger_kind, trigger,
  subject, revision, correlation_id, run_at)
VALUES (sqlc.arg(workspace_id), sqlc.arg(behavior_id), sqlc.arg(behavior_version_id), sqlc.arg(version),
  sqlc.arg(trigger_kind), sqlc.arg(trigger), sqlc.arg(subject), sqlc.arg(revision), sqlc.arg(correlation_id), sqlc.arg(run_at))
ON CONFLICT (behavior_version_id, subject, revision) DO NOTHING
RETURNING *;

-- name: GetBehaviorRunByDedupe :one
SELECT * FROM public.behavior_runs
WHERE workspace_id = sqlc.arg(workspace_id) AND behavior_version_id = sqlc.arg(behavior_version_id)
  AND subject = sqlc.arg(subject) AND revision = sqlc.arg(revision);

-- name: FailExhaustedBehaviorRuns :execrows
-- Runs whose lease ended after the last allowed attempt.
UPDATE public.behavior_runs SET status = 'failed', error_code = 'attempts_exhausted', claim_token = NULL,
  locked_until = NULL, finished_at = now(), updated_at = now()
WHERE status = 'running' AND locked_until <= sqlc.arg(at_time)::timestamptz AND attempts >= sqlc.arg(max_attempts)::int;

-- name: SelectBehaviorRunClaimCandidate :one
-- A due pending run, or a running run whose lease ended.
SELECT id FROM public.behavior_runs
WHERE ((status = 'pending' AND run_at <= sqlc.arg(at_time)::timestamptz)
    OR (status = 'running' AND locked_until <= sqlc.arg(at_time)::timestamptz))
  AND attempts < sqlc.arg(max_attempts)::int
ORDER BY (status = 'running') DESC, run_at, id
FOR UPDATE SKIP LOCKED LIMIT 1;

-- name: ClaimBehaviorRunByID :one
UPDATE public.behavior_runs AS r SET status = 'running', attempts = r.attempts + 1,
  claim_token = sqlc.arg(token)::uuid,
  locked_until = sqlc.arg(at_time)::timestamptz + make_interval(secs => sqlc.arg(lease_secs)::float8),
  started_at = COALESCE(r.started_at, now()), updated_at = now()
WHERE r.id = sqlc.arg(id)
RETURNING r.*;

-- name: AddBehaviorRunUsage :execrows
UPDATE public.behavior_runs SET input_tokens = input_tokens + sqlc.arg(input)::int,
  output_tokens = output_tokens + sqlc.arg(output)::int,
  cache_read_tokens = cache_read_tokens + sqlc.arg(cache_read)::int,
  cache_write_tokens = cache_write_tokens + sqlc.arg(cache_write)::int,
  steps = steps + sqlc.arg(steps)::int, updated_at = now()
WHERE id = sqlc.arg(id) AND claim_token = sqlc.arg(token)::uuid AND status = 'running';

-- name: FinishBehaviorRun :execrows
UPDATE public.behavior_runs SET status = sqlc.arg(status)::text, error_code = sqlc.narg(code),
  claim_token = NULL, locked_until = NULL, finished_at = now(), updated_at = now()
WHERE id = sqlc.arg(id) AND claim_token = sqlc.arg(token)::uuid AND status = 'running';

-- name: RetryBehaviorRun :execrows
UPDATE public.behavior_runs SET status = 'pending', run_at = sqlc.arg(next_at)::timestamptz, claim_token = NULL,
  locked_until = NULL, error_code = sqlc.arg(code)::text, updated_at = now()
WHERE id = sqlc.arg(id) AND claim_token = sqlc.arg(token)::uuid AND status = 'running';

-- name: SetBehaviorRunContext :execrows
-- Stores the context snapshot of the claimed run (replaced on a retry).
UPDATE public.behavior_runs SET context_snapshot = sqlc.arg(snapshot), updated_at = now()
WHERE id = sqlc.arg(id) AND claim_token = sqlc.arg(token)::uuid AND status = 'running';

-- name: LockClaimedBehaviorRun :one
-- Locks a run only while the caller still holds its claim.
SELECT workspace_id FROM public.behavior_runs
WHERE id = sqlc.arg(id) AND claim_token = sqlc.arg(token)::uuid AND status = 'running'
FOR UPDATE;

-- name: CancelPendingBehaviorRun :execrows
UPDATE public.behavior_runs SET status = 'canceled', finished_at = now(), updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND status = 'pending';

-- name: SetBehaviorRunSnapshot :execrows
-- Keeps the first snapshot: later writes of the same run never replace it.
UPDATE public.behavior_runs SET snapshot = sqlc.arg(snapshot), updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND snapshot IS NULL;

-- name: MarkBehaviorRunUndone :execrows
UPDATE public.behavior_runs SET undone_at = now(), updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND undone_at IS NULL AND snapshot IS NOT NULL;

-- name: GetBehaviorRun :one
SELECT sqlc.embed(r), b.name AS behavior_name
FROM public.behavior_runs r JOIN public.behaviors b ON b.workspace_id = r.workspace_id AND b.id = r.behavior_id
WHERE r.workspace_id = sqlc.arg(workspace_id) AND r.id = sqlc.arg(id);

-- name: LatestBehaviorRunBySubject :one
SELECT sqlc.embed(r), b.name AS behavior_name
FROM public.behavior_runs r JOIN public.behaviors b ON b.workspace_id = r.workspace_id AND b.id = r.behavior_id
WHERE r.workspace_id = sqlc.arg(workspace_id) AND r.subject = sqlc.arg(subject)
ORDER BY r.created_at DESC, r.id DESC LIMIT 1;

-- name: LatestBehaviorRunByCorrelation :one
SELECT sqlc.embed(r), b.name AS behavior_name
FROM public.behavior_runs r JOIN public.behaviors b ON b.workspace_id = r.workspace_id AND b.id = r.behavior_id
WHERE r.workspace_id = sqlc.arg(workspace_id) AND r.correlation_id = sqlc.arg(correlation_id)
ORDER BY r.created_at DESC, r.id DESC LIMIT 1;

-- name: ListRecentBehaviorRuns :many
-- Recent runs, newest first, optionally of one behavior, with item counts
-- by class.
SELECT r.id, b.name AS behavior_name, r.version, r.trigger_kind, r.subject, r.status,
       COALESCE(r.error_code, '')::text AS error_code, r.correlation_id, r.created_at,
       COUNT(i.id) FILTER (WHERE i.class = 'auto')::int AS auto_items,
       COUNT(i.id) FILTER (WHERE i.class = 'notify')::int AS notify_items,
       COUNT(i.id) FILTER (WHERE i.class = 'ask')::int AS ask_items
FROM public.behavior_runs r
JOIN public.behaviors b ON b.workspace_id = r.workspace_id AND b.id = r.behavior_id
LEFT JOIN public.run_items i ON i.workspace_id = r.workspace_id AND i.run_id = r.id
WHERE r.workspace_id = sqlc.arg(workspace_id)
  AND (sqlc.arg(behavior_name)::text = '' OR b.name = sqlc.arg(behavior_name)::text)
GROUP BY r.id, b.name
ORDER BY r.created_at DESC, r.id DESC
LIMIT sqlc.arg(row_limit)::int;

-- Run items.

-- name: InsertRunItem :one
-- Idempotent per (run, key): a retried run returns no row for a stored item.
INSERT INTO public.run_items(workspace_id, run_id, item_key, label, text, proposal, citations, topic, confidence,
  informational, flags, class, rule, status)
VALUES (sqlc.arg(workspace_id), sqlc.arg(run_id), sqlc.arg(item_key), sqlc.arg(label), sqlc.arg(text), sqlc.arg(proposal),
  sqlc.arg(citations), sqlc.arg(topic), sqlc.arg(confidence), sqlc.arg(informational), sqlc.arg(flags)::text[],
  sqlc.arg(class), sqlc.arg(rule), sqlc.arg(status))
ON CONFLICT (run_id, item_key) DO NOTHING
RETURNING *;

-- name: GetRunItem :one
SELECT * FROM public.run_items WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id);

-- name: GetRunItemByKey :one
SELECT * FROM public.run_items
WHERE workspace_id = sqlc.arg(workspace_id) AND run_id = sqlc.arg(run_id) AND item_key = sqlc.arg(item_key);

-- name: ListRunItems :many
SELECT * FROM public.run_items WHERE workspace_id = sqlc.arg(workspace_id) AND run_id = sqlc.arg(run_id) ORDER BY id;

-- name: TransitionRunItem :one
-- Moves an item only from one of the expected statuses; no row means the
-- item was not in any of them. A NULL result keeps the stored one.
UPDATE public.run_items SET status = sqlc.arg(to_status)::text,
  action_result = COALESCE(sqlc.narg(action_result), action_result), updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND status = ANY(sqlc.arg(from_statuses)::text[])
RETURNING *;

-- name: RateRunItem :execrows
UPDATE public.run_items SET rating = sqlc.arg(rating)::text, rating_reason = sqlc.narg(reason), rated_at = now(), updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id);

-- Approvals.

-- name: CreateApproval :one
INSERT INTO public.approvals(workspace_id, kind, run_id, item_id, action, resource, payload, options, nonce, expires_at)
VALUES (sqlc.arg(workspace_id), sqlc.arg(kind), sqlc.narg(run_id), sqlc.narg(item_id), sqlc.arg(action), sqlc.arg(resource),
  sqlc.arg(payload), sqlc.arg(options), sqlc.arg(nonce), sqlc.arg(expires_at))
RETURNING *;

-- name: GetApprovalByNonce :one
SELECT * FROM public.approvals WHERE workspace_id = sqlc.arg(workspace_id) AND nonce = sqlc.arg(nonce);

-- name: GetApproval :one
SELECT * FROM public.approvals WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id);

-- name: DecideApproval :one
-- Atomic: only an open, unexpired approval is decided; otherwise no row.
UPDATE public.approvals SET decision = sqlc.arg(decision)::text, answer = sqlc.narg(answer),
  decided_by = sqlc.arg(decided_by)::text, decided_at = now(), updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND nonce = sqlc.arg(nonce)
  AND decision IS NULL AND expires_at > sqlc.arg(at_time)::timestamptz
RETURNING *;

-- name: ExpireApprovals :many
UPDATE public.approvals SET decision = 'expired', decided_by = 'system', decided_at = now(), updated_at = now()
WHERE id IN (SELECT a.id FROM public.approvals a
             WHERE a.decision IS NULL AND a.expires_at <= sqlc.arg(at_time)::timestamptz
             ORDER BY a.expires_at, a.id
             FOR UPDATE SKIP LOCKED LIMIT sqlc.arg(max_rows)::int)
RETURNING *;

-- name: SetApprovalMessage :execrows
UPDATE public.approvals SET telegram_message_id = sqlc.arg(message_id)::bigint, updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id);

-- name: SetApprovalReplyMessage :execrows
-- Free-text mode: only an open approval takes a reply message.
UPDATE public.approvals SET reply_message_id = sqlc.arg(message_id)::bigint, updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND decision IS NULL;

-- name: StageApprovalAnswer :one
-- Stages a free-text reply on the newest open, unexpired approval in
-- free-text mode for that message; no row when none.
UPDATE public.approvals SET pending_answer = sqlc.arg(answer)::text, updated_at = now()
WHERE id = (SELECT c.id FROM public.approvals c
            WHERE c.workspace_id = sqlc.arg(workspace_id) AND c.reply_message_id = sqlc.arg(message_id)::bigint
              AND c.decision IS NULL AND c.expires_at > sqlc.arg(at_time)::timestamptz
            ORDER BY c.id DESC LIMIT 1 FOR UPDATE)
RETURNING nonce;

-- name: ClearApprovalPendingAnswer :execrows
UPDATE public.approvals SET pending_answer = NULL, updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND pending_answer IS NOT NULL;

-- name: ListApprovalsByRun :many
SELECT * FROM public.approvals WHERE workspace_id = sqlc.arg(workspace_id) AND run_id = sqlc.arg(run_id) ORDER BY id;

-- Telegram inbound.

-- name: InsertTelegramUpdate :one
-- A repeated update_id returns no row.
INSERT INTO public.telegram_updates(workspace_id, update_id, chat_id, kind, command, args, integration_id, note)
VALUES (sqlc.arg(workspace_id), sqlc.arg(update_id), sqlc.arg(chat_id), sqlc.arg(kind), sqlc.arg(command), sqlc.arg(args),
        sqlc.narg(integration_id), sqlc.arg(note))
ON CONFLICT (workspace_id, update_id) DO NOTHING
RETURNING id;

-- name: ClaimTelegramUpdate :one
-- The oldest unprocessed update with no live lease.
UPDATE public.telegram_updates AS u SET attempts = u.attempts + 1, claim_token = sqlc.arg(token)::uuid,
  locked_until = sqlc.arg(at_time)::timestamptz + make_interval(secs => sqlc.arg(lease_secs)::float8)
WHERE u.id = (SELECT c.id FROM public.telegram_updates c
              WHERE c.processed_at IS NULL AND (c.locked_until IS NULL OR c.locked_until <= sqlc.arg(at_time)::timestamptz)
              ORDER BY c.id FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING u.*;

-- name: FinishTelegramUpdate :execrows
UPDATE public.telegram_updates SET processed_at = now(), error_code = sqlc.narg(code), claim_token = NULL, locked_until = NULL
WHERE id = sqlc.arg(id) AND claim_token = sqlc.arg(token)::uuid AND processed_at IS NULL;

-- User profile.

-- name: GetUserProfile :one
SELECT * FROM public.user_profiles WHERE workspace_id = sqlc.arg(workspace_id);

-- name: UpsertUserProfile :one
INSERT INTO public.user_profiles(workspace_id, language, tone, quiet_start_minute, quiet_end_minute, timezone,
  digest_mode, default_model, budgets)
VALUES (sqlc.arg(workspace_id), sqlc.arg(language), sqlc.arg(tone), sqlc.narg(quiet_start_minute), sqlc.narg(quiet_end_minute),
  sqlc.arg(timezone), sqlc.arg(digest_mode), sqlc.arg(default_model), sqlc.arg(budgets))
ON CONFLICT (workspace_id) DO UPDATE SET language = EXCLUDED.language, tone = EXCLUDED.tone,
  quiet_start_minute = EXCLUDED.quiet_start_minute, quiet_end_minute = EXCLUDED.quiet_end_minute,
  timezone = EXCLUDED.timezone, digest_mode = EXCLUDED.digest_mode, default_model = EXCLUDED.default_model,
  budgets = EXCLUDED.budgets, updated_at = now()
RETURNING *;

-- Decision memory.

-- name: InsertDecision :one
INSERT INTO public.decision_memory(workspace_id, topic, question, answer, scope, source, run_id, item_id)
VALUES (sqlc.arg(workspace_id), sqlc.arg(topic), sqlc.arg(question), sqlc.arg(answer), sqlc.arg(scope), sqlc.arg(source),
  sqlc.narg(run_id), sqlc.narg(item_id))
RETURNING *;

-- name: GetDecision :one
SELECT * FROM public.decision_memory WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id);

-- name: SearchDecisions :many
-- Any query word matches (the words are OR-ed); best rank first. An empty
-- behavior filter matches every scope.
SELECT d.*, ts_rank(to_tsvector('simple'::regconfig, d.topic || ' ' || d.question || ' ' || d.answer),
  replace(plainto_tsquery('simple'::regconfig, sqlc.arg(query)::text)::text, '&', '|')::tsquery)::float8 AS rank
FROM public.decision_memory d
WHERE d.workspace_id = sqlc.arg(workspace_id) AND d.forgotten_at IS NULL
  AND to_tsvector('simple'::regconfig, d.topic || ' ' || d.question || ' ' || d.answer)
      @@ replace(plainto_tsquery('simple'::regconfig, sqlc.arg(query)::text)::text, '&', '|')::tsquery
  AND (sqlc.arg(behavior)::text = '' OR d.scope->>'behavior' = sqlc.arg(behavior)::text)
ORDER BY rank DESC, d.created_at DESC, d.id DESC
LIMIT sqlc.arg(max_rows)::int;

-- name: ListDecisions :many
SELECT * FROM public.decision_memory
WHERE workspace_id = sqlc.arg(workspace_id) AND (sqlc.arg(include_forgotten)::bool OR forgotten_at IS NULL)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(max_rows)::int OFFSET sqlc.arg(skip)::int;

-- name: ForgetDecision :execrows
UPDATE public.decision_memory SET forgotten_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND forgotten_at IS NULL;

-- Stats.

-- name: BehaviorVersionStats :many
SELECT v.version,
  count(DISTINCT r.id)::bigint AS runs,
  count(DISTINCT r.id) FILTER (WHERE r.undone_at IS NOT NULL)::bigint AS undos,
  count(i.id) FILTER (WHERE i.class = 'auto')::bigint AS auto_items,
  count(i.id) FILTER (WHERE i.class = 'notify')::bigint AS notify_items,
  count(i.id) FILTER (WHERE i.class = 'ask')::bigint AS ask_items,
  count(i.id) FILTER (WHERE i.status = 'conflict')::bigint AS conflicts,
  count(i.id) FILTER (WHERE i.rating = 'down')::bigint AS rated_down
FROM public.behavior_versions v
LEFT JOIN public.behavior_runs r ON r.workspace_id = v.workspace_id AND r.behavior_version_id = v.id
LEFT JOIN public.run_items i ON i.workspace_id = r.workspace_id AND i.run_id = r.id
WHERE v.workspace_id = sqlc.arg(workspace_id) AND v.behavior_id = sqlc.arg(behavior_id)
GROUP BY v.id, v.version
ORDER BY v.version;

-- Engine Telegram messages.

-- name: ClaimBehaviorMessage :one
-- Claims a message key of a run once; a repeat returns no row. The row
-- takes the run's correlation ID.
INSERT INTO public.behavior_messages(workspace_id, run_id, msg_key, correlation_id)
SELECT r.workspace_id, r.id, sqlc.arg(msg_key), r.correlation_id
FROM public.behavior_runs r
WHERE r.workspace_id = sqlc.arg(workspace_id) AND r.id = sqlc.arg(run_id)
ON CONFLICT (run_id, msg_key) DO NOTHING
RETURNING id;

-- name: GetBehaviorMessage :one
SELECT * FROM public.behavior_messages
WHERE workspace_id = sqlc.arg(workspace_id) AND run_id = sqlc.arg(run_id) AND msg_key = sqlc.arg(msg_key);

-- name: SetBehaviorMessageSent :execrows
UPDATE public.behavior_messages SET telegram_message_id = sqlc.arg(message_id)::bigint, sent_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND run_id = sqlc.arg(run_id) AND msg_key = sqlc.arg(msg_key)
  AND telegram_message_id IS NULL;

-- name: ReleaseBehaviorMessage :execrows
-- Drops a claim whose send failed before Telegram took it, so a retry may
-- send again. A recorded message is never released.
DELETE FROM public.behavior_messages
WHERE workspace_id = sqlc.arg(workspace_id) AND run_id = sqlc.arg(run_id) AND msg_key = sqlc.arg(msg_key)
  AND telegram_message_id IS NULL;
