-- ClickUp drafts (Phase 10 W1). The body exists only while a draft may still
-- be posted; every terminal transition clears it in the same statement.

-- name: InsertClickUpDraft :one
INSERT INTO public.clickup_drafts (workspace_id, integration_id, chat_id, thread_id, task_id, parent_comment_id,
    mention_user_ids, new_status, body, body_sha256, source, execution_id, plan_task_key, expires_at)
VALUES (sqlc.arg(workspace_id), sqlc.arg(integration_id), sqlc.arg(chat_id), sqlc.arg(thread_id), sqlc.arg(task_id),
    sqlc.arg(parent_comment_id), sqlc.arg(mention_user_ids)::text[], sqlc.arg(new_status), sqlc.arg(body)::text, sqlc.arg(body_sha256),
    sqlc.arg(source), sqlc.narg(execution_id), sqlc.arg(plan_task_key), sqlc.arg(expires_at))
RETURNING *;

-- name: GetClickUpDraft :one
SELECT * FROM public.clickup_drafts
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id);

-- name: SetClickUpDraftPreview :execrows
-- Binds the preview message that carries the draft's current buttons.
UPDATE public.clickup_drafts SET preview_message_id = sqlc.arg(message_id)::bigint, updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND state IN ('pending', 'failed');

-- name: ReviseClickUpDraft :one
-- Replaces the body of a pending, unexpired draft whose body is still the
-- one the owner edited; the old preview stops being valid.
UPDATE public.clickup_drafts
SET body = sqlc.arg(body)::text, body_sha256 = sqlc.arg(body_sha256), preview_message_id = 0, updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND state = 'pending'
  AND body_sha256 = sqlc.arg(old_sha256) AND expires_at > sqlc.arg(at_time)::timestamptz
RETURNING *;

-- name: ClaimClickUpDraftPost :one
-- Atomic: only one confirm of the exact previewed body wins.
UPDATE public.clickup_drafts SET state = 'posting', reason_code = '', updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND state IN ('pending', 'failed')
  AND body_sha256 = sqlc.arg(body_sha256) AND preview_message_id = sqlc.arg(preview_message_id)::bigint
  AND expires_at > sqlc.arg(at_time)::timestamptz
RETURNING *;

-- name: FinishClickUpDraftPosted :one
-- reason_code is set when the comment was posted but the status change of
-- the same confirm failed.
UPDATE public.clickup_drafts
SET state = 'posted', body = NULL, posted_comment_id = sqlc.arg(posted_comment_id), reason_code = sqlc.arg(reason_code),
    finished_at = now(), updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND state = 'posting'
RETURNING *;

-- name: FailClickUpDraft :one
-- A failed post keeps the body for "Thử lại" until the draft expires.
UPDATE public.clickup_drafts SET state = 'failed', reason_code = sqlc.arg(reason_code), updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND state = 'posting'
RETURNING *;

-- name: CancelClickUpDraft :one
UPDATE public.clickup_drafts SET state = 'cancelled', body = NULL, finished_at = now(), updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND state IN ('pending', 'failed')
RETURNING *;

-- name: ExpireClickUpDrafts :many
-- Expires up to limit overdue open drafts in every workspace and clears
-- their bodies.
UPDATE public.clickup_drafts d SET state = 'expired', body = NULL, finished_at = now(), updated_at = now()
WHERE d.id IN (SELECT c.id FROM public.clickup_drafts c
               WHERE c.state IN ('pending', 'failed') AND c.expires_at <= sqlc.arg(at_time)::timestamptz
               ORDER BY c.expires_at LIMIT sqlc.arg(max_rows)::int FOR UPDATE SKIP LOCKED)
RETURNING d.*;

-- name: ReclaimStaleClickUpDraftPosts :many
-- Posts interrupted before they finished (a crash mid-confirm): the caller
-- finishes them with the same idempotency key, so nothing posts twice.
UPDATE public.clickup_drafts d SET updated_at = now()
WHERE d.id IN (SELECT c.id FROM public.clickup_drafts c
               WHERE c.state = 'posting' AND c.updated_at < sqlc.arg(before)::timestamptz
               ORDER BY c.updated_at LIMIT sqlc.arg(max_rows)::int FOR UPDATE SKIP LOCKED)
RETURNING d.*;

-- name: GetOrchestratorClickUpDraft :one
-- The newest draft an orchestrated execution made for one plan task (Phase
-- 10 REQ-10), so a retried step finds the draft it already made.
SELECT * FROM public.clickup_drafts
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id)::bigint
  AND source = 'orchestrator' AND plan_task_key = sqlc.arg(plan_task_key)
ORDER BY id DESC
LIMIT 1;
