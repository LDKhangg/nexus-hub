-- name: CreateThread :one
INSERT INTO public.hub_threads(id,workspace_id,integration_id,external_scope,thread_type,external_id,title,metadata)
VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING *;

-- name: GetThread :one
SELECT * FROM public.hub_threads WHERE workspace_id=$1 AND id=$2;

-- name: ListThreads :many
SELECT * FROM public.hub_threads WHERE workspace_id=$1
ORDER BY updated_at DESC, id DESC LIMIT $2 OFFSET $3;

-- name: UpdateThread :one
UPDATE public.hub_threads SET title=$3, metadata=$4, updated_at=now()
WHERE workspace_id=$1 AND id=$2 RETURNING *;

-- name: DeleteThread :one
DELETE FROM public.hub_threads WHERE workspace_id=$1 AND id=$2 RETURNING id;
