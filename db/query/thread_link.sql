-- name: CreateThreadLink :one
INSERT INTO public.thread_links (workspace_id, source_thread_id, target_thread_id, metadata)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetThreadLink :one
SELECT * FROM public.thread_links
WHERE workspace_id = $1 AND source_thread_id = $2 AND target_thread_id = $3;

-- name: ListThreadLinks :many
SELECT * FROM public.thread_links
WHERE workspace_id = $1 AND (source_thread_id = sqlc.arg('thread_id') OR target_thread_id = sqlc.arg('thread_id'))
ORDER BY source_thread_id ASC, target_thread_id ASC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: UpdateThreadLink :one
UPDATE public.thread_links SET metadata = $4
WHERE workspace_id = $1 AND source_thread_id = $2 AND target_thread_id = $3
RETURNING *;

-- name: DeleteThreadLink :one
DELETE FROM public.thread_links
WHERE workspace_id = $1 AND source_thread_id = $2 AND target_thread_id = $3
RETURNING source_thread_id;
