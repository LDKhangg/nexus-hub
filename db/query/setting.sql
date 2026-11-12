-- name: GetSetting :one
SELECT value FROM public.app_settings WHERE key = $1;

-- name: SetSetting :exec
INSERT INTO public.app_settings(key, value) VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();

-- name: ListSettings :many
SELECT key, value, updated_at FROM public.app_settings ORDER BY key;
