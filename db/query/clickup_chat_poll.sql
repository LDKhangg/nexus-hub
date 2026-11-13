-- ClickUp Chat poll cursor and error state.

-- name: InitChatPoll :one
-- Creates the poll row with the cursor at cursor_ms, or starts it again when
-- the integration now points at another ClickUp workspace. Returns no row
-- when the stored row is kept.
INSERT INTO public.clickup_chat_polls AS p (workspace_id, integration_id, clickup_team_id, cursor_ms)
VALUES (sqlc.arg(workspace_id), sqlc.arg(integration_id), sqlc.arg(clickup_team_id)::text, sqlc.arg(cursor_ms)::bigint)
ON CONFLICT (workspace_id, integration_id) DO UPDATE SET
  clickup_team_id = EXCLUDED.clickup_team_id, cursor_ms = EXCLUDED.cursor_ms, initialized_at = now(),
  error_streak = 0, unauthorized_streak = 0, last_error_code = NULL, backoff_until = NULL,
  unauthorized_notified_at = NULL, updated_at = now()
WHERE p.clickup_team_id <> EXCLUDED.clickup_team_id
RETURNING *;

-- name: GetChatPoll :one
SELECT * FROM public.clickup_chat_polls
WHERE workspace_id = sqlc.arg(workspace_id) AND integration_id = sqlc.arg(integration_id);

-- name: AdvanceChatCursor :one
-- Moves the cursor forward only; a smaller value keeps the stored cursor.
-- The ClickUp workspace must still match, so a stale poll cannot move a
-- cursor that was started again for another workspace.
UPDATE public.clickup_chat_polls
SET cursor_ms = GREATEST(cursor_ms, sqlc.arg(cursor_ms)::bigint),
  updated_at = CASE WHEN sqlc.arg(cursor_ms)::bigint > cursor_ms THEN now() ELSE updated_at END
WHERE workspace_id = sqlc.arg(workspace_id) AND integration_id = sqlc.arg(integration_id)
  AND clickup_team_id = sqlc.arg(clickup_team_id)::text
RETURNING cursor_ms;

-- name: RecordChatPollError :one
-- Counts one failed poll and sets the backoff: base_secs doubled for each
-- earlier consecutive error, capped at max_secs. A code other than
-- chat_unauthorized ends the unauthorized streak and its notice.
UPDATE public.clickup_chat_polls
SET error_streak = error_streak + 1,
  unauthorized_streak = CASE WHEN sqlc.arg(code)::text = 'chat_unauthorized' THEN unauthorized_streak + 1 ELSE 0 END,
  unauthorized_notified_at = CASE WHEN sqlc.arg(code)::text = 'chat_unauthorized' THEN unauthorized_notified_at ELSE NULL END,
  last_error_code = sqlc.arg(code)::text,
  backoff_until = sqlc.arg(at_time)::timestamptz + make_interval(secs => LEAST(sqlc.arg(max_secs)::float8,
    sqlc.arg(base_secs)::float8 * power(2::float8, LEAST(error_streak, 30)::float8))),
  updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND integration_id = sqlc.arg(integration_id)
RETURNING *;

-- name: ResetChatPollErrors :exec
-- Clears the error state after a successful poll.
UPDATE public.clickup_chat_polls
SET error_streak = 0, unauthorized_streak = 0, last_error_code = NULL, backoff_until = NULL,
  unauthorized_notified_at = NULL, updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND integration_id = sqlc.arg(integration_id)
  AND (error_streak > 0 OR unauthorized_streak > 0 OR last_error_code IS NOT NULL OR backoff_until IS NOT NULL
    OR unauthorized_notified_at IS NOT NULL);

-- name: ClaimChatUnauthorizedNotice :execrows
-- Claims the one owner notice of the current unauthorized streak once the
-- streak reached min_streak; 0 rows when not reached or already claimed.
UPDATE public.clickup_chat_polls
SET unauthorized_notified_at = sqlc.arg(at_time)::timestamptz, updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND integration_id = sqlc.arg(integration_id)
  AND unauthorized_streak >= sqlc.arg(min_streak)::int AND unauthorized_notified_at IS NULL;
