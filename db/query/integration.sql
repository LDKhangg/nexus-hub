-- name: CreateIntegration :one
INSERT INTO public.integrations(
  id,
  workspace_id,
  platform,
  role, config, is_enabled
) VALUES($1,$2,$3,$4,$5,$6)

RETURNING id, workspace_id, platform, role, config, is_enabled, created_at, updated_at;

-- name: GetIntegration :one
SELECT id, workspace_id, platform, role, config, is_enabled, created_at, updated_at
FROM public.integrations WHERE workspace_id=$1 AND id=$2;

-- name: ListIntegrations :many
SELECT id, workspace_id, platform, role, config, is_enabled, created_at, updated_at
FROM public.integrations WHERE workspace_id=$1
ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3;

-- name: UpdateIntegration :one
UPDATE public.integrations SET role=$3, config=$4, is_enabled=$5, updated_at=now()
WHERE workspace_id=$1 AND id=$2
RETURNING id, workspace_id, platform, role, config, is_enabled, created_at, updated_at;

-- name: DeleteIntegration :one
DELETE FROM public.integrations WHERE workspace_id=$1 AND id=$2 RETURNING id;
