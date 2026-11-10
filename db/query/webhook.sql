-- name: LookupWebhookIntegration :one
SELECT id, workspace_id, platform, role, is_enabled
FROM public.integrations
WHERE id = $1;
