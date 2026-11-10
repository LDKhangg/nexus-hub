-- name: CreateWorkspace :one
INSERT INTO public.workspaces (
  id,
  display_name
) VALUES($1,$2)

RETURNING id, display_name, created_at, updated_at;

-- name: ListWorkspaces :many
SELECT  id, display_name, created_at, updated_at
FROM public.workspaces
ORDER BY updated_at DESC, id DESC
LIMIT $1 OFFSET $2;

-- name: GetWorkspaceByID :one
SELECT id,display_name, created_at, updated_at
FROM public.workspaces ws
WHERE ws.id = $1;

-- name: UpdateWorkspace :one
UPDATE public.workspaces SET display_name = $2, updated_at = now()
WHERE id = $1 RETURNING id, display_name, created_at, updated_at;

-- name: DeleteWorkspace :one
DELETE FROM public.workspaces WHERE id = $1 RETURNING id;
