-- Workspace-scoped Telegram topic bindings. Identifiers and state only.

-- name: ReserveTelegramTopic :one
-- Returns a row only for the caller that created the reservation.
INSERT INTO public.telegram_topics (workspace_id, integration_id, chat_id, subject_kind, subject_key)
VALUES (sqlc.arg(workspace_id), sqlc.arg(integration_id), sqlc.arg(chat_id), sqlc.arg(subject_kind), sqlc.arg(subject_key))
ON CONFLICT (workspace_id, integration_id, chat_id, subject_kind, subject_key) DO NOTHING
RETURNING *;

-- name: ReclaimTelegramTopicReservation :one
-- Takes over a reservation that never got a thread id within the lease.
UPDATE public.telegram_topics
SET reserved_at = now(), updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND integration_id = sqlc.arg(integration_id)
  AND chat_id = sqlc.arg(chat_id) AND subject_kind = sqlc.arg(subject_kind) AND subject_key = sqlc.arg(subject_key)
  AND thread_id IS NULL
  AND reserved_at <= now() - make_interval(secs => sqlc.arg(lease_secs)::float8)
RETURNING *;

-- name: SetTelegramTopicThread :one
-- Succeeds once, for a reserved topic without a thread id, and only for the
-- holder whose reservation (reserved_at) is still the current one.
UPDATE public.telegram_topics
SET thread_id = sqlc.arg(thread_id), updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND thread_id IS NULL
  AND reserved_at = sqlc.arg(reserved_at)
RETURNING *;

-- name: ReleaseTelegramTopicReservation :execrows
DELETE FROM public.telegram_topics
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND thread_id IS NULL;

-- name: GetTelegramTopicBySubject :one
SELECT * FROM public.telegram_topics
WHERE workspace_id = sqlc.arg(workspace_id) AND integration_id = sqlc.arg(integration_id)
  AND chat_id = sqlc.arg(chat_id) AND subject_kind = sqlc.arg(subject_kind) AND subject_key = sqlc.arg(subject_key);

-- name: GetTelegramTopicByThread :one
SELECT * FROM public.telegram_topics
WHERE workspace_id = sqlc.arg(workspace_id) AND integration_id = sqlc.arg(integration_id)
  AND chat_id = sqlc.arg(chat_id) AND thread_id = sqlc.arg(thread_id);

-- name: SetTelegramTopicState :one
UPDATE public.telegram_topics
SET state = sqlc.arg(state)::text,
    closed_at = CASE WHEN sqlc.arg(state)::text = 'closed' THEN now() ELSE NULL END,
    updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id)
RETURNING *;

-- name: ListTelegramTopics :many
SELECT * FROM public.telegram_topics
WHERE workspace_id = sqlc.arg(workspace_id)
ORDER BY id;

-- name: DeleteTelegramTopic :execrows
-- Forgets a binding whose Telegram thread no longer exists.
DELETE FROM public.telegram_topics
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id);
