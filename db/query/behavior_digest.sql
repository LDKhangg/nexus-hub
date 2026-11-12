-- Batched notify/FYI messages.

-- name: QueueDigestEntry :one
-- Queues a run's message once per (run, key); a repeat returns no row.
INSERT INTO public.behavior_digest_entries(workspace_id, run_id, msg_key, integration_id, chat_id, due_at)
SELECT r.workspace_id, r.id, sqlc.arg(msg_key)::text, sqlc.arg(integration_id)::uuid, sqlc.arg(chat_id)::bigint,
  sqlc.arg(due_at)::timestamptz
FROM public.behavior_runs r
WHERE r.workspace_id = sqlc.arg(workspace_id) AND r.id = sqlc.arg(run_id)
ON CONFLICT (run_id, msg_key) DO NOTHING
RETURNING id;

-- name: GetDigestEntry :one
SELECT * FROM public.behavior_digest_entries
WHERE workspace_id = sqlc.arg(workspace_id) AND run_id = sqlc.arg(run_id) AND msg_key = sqlc.arg(msg_key);

-- name: SelectDueDigestChat :one
-- The chat of the oldest due, unbatched entry.
SELECT workspace_id, integration_id, chat_id FROM public.behavior_digest_entries
WHERE batch_id IS NULL AND due_at <= sqlc.arg(at_time)::timestamptz
ORDER BY due_at, id
LIMIT 1
FOR UPDATE SKIP LOCKED;

-- name: InsertDigestBatch :one
INSERT INTO public.behavior_digest_batches(workspace_id, integration_id, chat_id, claim_token)
VALUES (sqlc.arg(workspace_id), sqlc.arg(integration_id), sqlc.arg(chat_id), sqlc.arg(token)::uuid)
RETURNING id;

-- name: AssignDigestEntries :many
-- Puts up to max_rows due, unbatched entries of one chat into a batch.
UPDATE public.behavior_digest_entries e SET batch_id = sqlc.arg(batch_id)::bigint
WHERE e.id IN (
  SELECT x.id FROM public.behavior_digest_entries x
  WHERE x.workspace_id = sqlc.arg(workspace_id) AND x.integration_id = sqlc.arg(integration_id)
    AND x.chat_id = sqlc.arg(chat_id) AND x.batch_id IS NULL AND x.due_at <= sqlc.arg(at_time)::timestamptz
  ORDER BY x.run_id, x.msg_key
  LIMIT sqlc.arg(max_rows)::int
  FOR UPDATE SKIP LOCKED)
RETURNING e.run_id, e.msg_key;

-- name: LockDigestBatch :one
SELECT id FROM public.behavior_digest_batches
WHERE id = sqlc.arg(id) AND claim_token = sqlc.arg(token)::uuid AND status = 'sending'
FOR UPDATE;

-- name: FinishDigestBatch :execrows
UPDATE public.behavior_digest_batches SET status = 'sent', telegram_message_id = sqlc.arg(message_id)::bigint,
  claim_token = NULL, sent_at = now()
WHERE id = sqlc.arg(id) AND claim_token = sqlc.arg(token)::uuid AND status = 'sending';

-- name: SetDigestBatchMessages :execrows
-- Records the batch's message on each entry's run message key.
UPDATE public.behavior_messages m SET telegram_message_id = sqlc.arg(message_id)::bigint, sent_at = now()
FROM public.behavior_digest_entries e
WHERE e.batch_id = sqlc.arg(batch_id)::bigint AND m.run_id = e.run_id AND m.msg_key = e.msg_key
  AND m.telegram_message_id IS NULL;

-- name: ReleaseDigestEntries :execrows
-- Returns a batch's entries to the queue, due again at due_at when given.
UPDATE public.behavior_digest_entries SET batch_id = NULL, due_at = COALESCE(sqlc.narg(due_at)::timestamptz, due_at)
WHERE batch_id = sqlc.arg(batch_id)::bigint;

-- name: DeleteDigestBatch :execrows
DELETE FROM public.behavior_digest_batches WHERE id = sqlc.arg(id) AND status = 'sending';
