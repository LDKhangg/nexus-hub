-- name: CreateWorkspace :one
INSERT INTO public.workspaces (
  id,
  display_name
) VALUES($1,$2)

RETURNING id, display_name, created_at, updated_at;
