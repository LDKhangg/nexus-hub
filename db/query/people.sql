-- name: UpsertContactAccount :one
WITH locked_workspace AS (
  SELECT id FROM public.workspaces
  WHERE id = sqlc.arg(workspace_id)::uuid
  FOR UPDATE
), existing AS (
  SELECT a.contact_id
  FROM public.contact_accounts a
  CROSS JOIN locked_workspace
  WHERE a.workspace_id = sqlc.arg(workspace_id)::uuid
    AND a.platform = sqlc.arg(platform)
    AND a.external_id = sqlc.arg(external_id)
), new_contact AS (
  INSERT INTO public.contacts(id, workspace_id)
  SELECT sqlc.arg(contact_id)::uuid, id FROM locked_workspace
  WHERE NOT EXISTS (SELECT 1 FROM existing)
  RETURNING id
)
INSERT INTO public.contact_accounts(
  id, workspace_id, contact_id, platform, external_id, platform_name, username,
  source, is_self, first_seen, last_seen
)
SELECT sqlc.arg(account_id)::uuid, sqlc.arg(workspace_id)::uuid,
  COALESCE((SELECT contact_id FROM existing), (SELECT id FROM new_contact)),
  sqlc.arg(platform), sqlc.arg(external_id), sqlc.narg(platform_name)::text,
  sqlc.narg(username)::text, sqlc.arg(source), sqlc.arg(is_self)::boolean,
  sqlc.arg(seen_at)::timestamptz, sqlc.arg(seen_at)::timestamptz
ON CONFLICT (workspace_id, platform, external_id) DO UPDATE SET
  platform_name = EXCLUDED.platform_name,
  username = EXCLUDED.username,
  is_self = public.contact_accounts.is_self OR EXCLUDED.is_self,
  last_seen = GREATEST(public.contact_accounts.last_seen, EXCLUDED.last_seen)
RETURNING *;

-- name: GetContactByAccount :one
SELECT c.*, a.id AS account_id, a.platform, a.external_id, a.platform_name,
  a.username, a.source, a.is_self, a.first_seen, a.last_seen
FROM public.contacts c
JOIN public.contact_accounts a
  ON a.workspace_id = c.workspace_id AND a.contact_id = c.id
WHERE a.workspace_id = sqlc.arg(workspace_id)::uuid
  AND a.platform = sqlc.arg(platform)
  AND a.external_id = sqlc.arg(external_id);

-- name: ListContacts :many
SELECT c.*, count(a.id)::bigint AS account_count
FROM public.contacts c
LEFT JOIN public.contact_accounts a
  ON a.workspace_id = c.workspace_id AND a.contact_id = c.id
WHERE c.workspace_id = sqlc.arg(workspace_id)::uuid
  AND (sqlc.narg(platform)::text IS NULL OR a.platform = sqlc.narg(platform)::text)
  AND (sqlc.narg(search)::text IS NULL
    OR c.display_name ILIKE '%' || sqlc.narg(search)::text || '%'
    OR a.platform_name ILIKE '%' || sqlc.narg(search)::text || '%'
    OR a.username ILIKE '%' || sqlc.narg(search)::text || '%')
GROUP BY c.id
ORDER BY COALESCE(c.display_name, min(a.platform_name), min(a.username), c.id::text), c.id
LIMIT sqlc.arg(max_rows)::int OFFSET sqlc.arg(row_offset)::int;

-- name: GetContact :one
SELECT * FROM public.contacts
WHERE workspace_id = sqlc.arg(workspace_id)::uuid AND id = sqlc.arg(contact_id)::uuid;

-- name: ListContactAccounts :many
SELECT * FROM public.contact_accounts
WHERE workspace_id = sqlc.arg(workspace_id)::uuid AND contact_id = sqlc.arg(contact_id)::uuid
ORDER BY platform, external_id;

-- name: SetContactAlias :one
UPDATE public.contacts
SET display_name = sqlc.narg(display_name)::text, updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id)::uuid AND id = sqlc.arg(contact_id)::uuid
RETURNING *;

-- name: MergeContacts :one
WITH valid AS (
  SELECT source.id AS source_id, target.id AS target_id
  FROM public.contacts source
  JOIN public.contacts target ON target.workspace_id = source.workspace_id
  WHERE source.workspace_id = sqlc.arg(workspace_id)::uuid
    AND source.id = sqlc.arg(source_id)::uuid
    AND target.id = sqlc.arg(target_id)::uuid
    AND source.id <> target.id
), moved AS (
  UPDATE public.contact_accounts a SET contact_id = valid.target_id
  FROM valid WHERE a.workspace_id = sqlc.arg(workspace_id)::uuid
    AND a.contact_id = valid.source_id
), deleted AS (
  DELETE FROM public.contacts c USING valid
  WHERE c.workspace_id = sqlc.arg(workspace_id)::uuid AND c.id = valid.source_id
)
SELECT c.* FROM public.contacts c JOIN valid ON c.id = valid.target_id;

-- name: SplitContactAccount :one
WITH locked_workspace AS (
  SELECT id FROM public.workspaces WHERE id = sqlc.arg(workspace_id)::uuid FOR UPDATE
), new_contact AS (
  INSERT INTO public.contacts(id, workspace_id)
  SELECT sqlc.arg(new_contact_id)::uuid, id FROM locked_workspace
  WHERE EXISTS (
    SELECT 1 FROM public.contact_accounts
    WHERE workspace_id = sqlc.arg(workspace_id)::uuid AND id = sqlc.arg(account_id)::uuid
  )
  RETURNING id
)
UPDATE public.contact_accounts a SET contact_id = new_contact.id
FROM new_contact
WHERE a.workspace_id = sqlc.arg(workspace_id)::uuid AND a.id = sqlc.arg(account_id)::uuid
RETURNING a.*;

-- name: ForgetContact :one
DELETE FROM public.contacts
WHERE workspace_id = sqlc.arg(workspace_id)::uuid AND id = sqlc.arg(contact_id)::uuid
RETURNING id;

-- name: GetOwnerUser :one
SELECT u.* FROM public.users u
WHERE EXISTS (SELECT 1 FROM public.workspace_members m WHERE m.user_id = u.id AND m.role = 'owner')
ORDER BY u.created_at, u.id LIMIT 1;

-- name: ListUserMemberships :many
SELECT * FROM public.workspace_members
WHERE user_id = sqlc.arg(user_id)::uuid
ORDER BY workspace_id;

-- name: LockPeopleWorkspace :one
-- A separate statement takes the per-workspace people lock, so statements that
-- follow in the same READ COMMITTED transaction see accounts committed by the
-- previous lock holder.
SELECT id FROM public.workspaces
WHERE id = sqlc.arg(workspace_id)::uuid
FOR UPDATE;

-- name: EnsureWorkspaceOwner :execrows
-- Gives a workspace without any owner the stable bootstrap owner (the same
-- user 00012 creates). Idempotent; never changes an existing membership.
WITH owner_user AS (
  INSERT INTO public.users(id, display_name, status)
  VALUES ('00000000-0000-0000-0000-000000000001', 'Owner', 'active')
  ON CONFLICT (id) DO NOTHING
  RETURNING id
)
INSERT INTO public.workspace_members(user_id, workspace_id, role)
SELECT '00000000-0000-0000-0000-000000000001'::uuid, w.id, 'owner'
FROM public.workspaces w
WHERE w.id = sqlc.arg(workspace_id)::uuid
  AND NOT EXISTS (
    SELECT 1 FROM public.workspace_members m
    WHERE m.workspace_id = w.id AND m.role = 'owner'
  )
ON CONFLICT (user_id, workspace_id) DO NOTHING;
