-- name: CreateMessage :one
INSERT INTO public.message_logs (
    thread_id, external_message_id, sender_id, sender_name, sender_role, content, occurred_at, metadata
) VALUES ($1, $2, $3, $4, $5, $6, COALESCE(sqlc.narg('occurred_at')::timestamptz, now()), $7)
RETURNING *;

-- name: GetMessage :one
SELECT m.* FROM public.message_logs AS m
WHERE m.thread_id = $1 AND m.id = $2
  AND EXISTS (SELECT 1 FROM public.hub_threads AS t WHERE t.id = m.thread_id AND t.workspace_id = $3);

-- name: ListMessages :many
SELECT m.* FROM public.message_logs AS m
WHERE m.thread_id = $1
  AND EXISTS (SELECT 1 FROM public.hub_threads AS t WHERE t.id = m.thread_id AND t.workspace_id = $2)
ORDER BY m.id ASC LIMIT $3 OFFSET $4;

-- name: UpdateMessage :one
UPDATE public.message_logs AS m SET content = $3, metadata = $4
WHERE m.thread_id = $1 AND m.id = $2
  AND EXISTS (SELECT 1 FROM public.hub_threads AS t WHERE t.id = m.thread_id AND t.workspace_id = $5)
RETURNING m.*;

-- name: DeleteMessage :one
DELETE FROM public.message_logs AS m
WHERE m.thread_id = $1 AND m.id = $2
  AND EXISTS (SELECT 1 FROM public.hub_threads AS t WHERE t.id = m.thread_id AND t.workspace_id = $3)
RETURNING m.id;

-- name: LockMessageThread :one
SELECT * FROM public.hub_threads WHERE workspace_id = $1 AND id = $2 FOR UPDATE;

-- name: InvalidateThreadSummary :exec
UPDATE public.hub_threads SET rolling_summary = '', summary_through_message_id = NULL
WHERE id = $1 AND summary_through_message_id >= $2;

-- name: RefreshMessageThread :exec
UPDATE public.hub_threads SET updated_at = now() WHERE id = $1;
