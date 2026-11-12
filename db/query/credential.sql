-- name: PutCredential :exec
INSERT INTO public.credentials(id, workspace_id, name, kind, ciphertext, nonce)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (workspace_id, name)
DO UPDATE SET kind = EXCLUDED.kind, ciphertext = EXCLUDED.ciphertext, nonce = EXCLUDED.nonce, updated_at = now();

-- name: GetCredential :one
SELECT kind, nonce, ciphertext, key_version FROM public.credentials WHERE workspace_id = $1 AND name = $2;

-- name: ListCredentials :many
SELECT name, kind, updated_at FROM public.credentials WHERE workspace_id = $1 ORDER BY name;

-- name: DeleteCredential :execrows
DELETE FROM public.credentials WHERE workspace_id = $1 AND name = $2;
