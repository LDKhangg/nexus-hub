-- Telegram message to ClickUp comment links. Identifiers only.

-- name: InsertTelegramCommentLink :execrows
-- Records the comment a delivered Telegram message shows; a second record
-- of the same message keeps the first.
INSERT INTO public.telegram_message_links (workspace_id, integration_id, chat_id, message_id, subject_kind, task_id, comment_id)
VALUES (sqlc.arg(workspace_id), sqlc.arg(integration_id), sqlc.arg(chat_id), sqlc.arg(message_id), 'clickup_comment', sqlc.arg(task_id), sqlc.arg(comment_id))
ON CONFLICT (workspace_id, integration_id, chat_id, message_id) DO NOTHING;

-- name: GetTelegramCommentLink :one
SELECT task_id, comment_id FROM public.telegram_message_links
WHERE workspace_id = sqlc.arg(workspace_id) AND integration_id = sqlc.arg(integration_id)
  AND chat_id = sqlc.arg(chat_id) AND message_id = sqlc.arg(message_id)
  AND subject_kind = 'clickup_comment';

-- name: PruneTelegramMessageLinks :execrows
DELETE FROM public.telegram_message_links
WHERE created_at < sqlc.arg(before);

-- name: GetClickUpTaskTopicThread :one
-- The thread of a created ClickUp task topic (open or closed) of one chat.
SELECT thread_id::bigint AS thread_id FROM public.telegram_topics
WHERE workspace_id = sqlc.arg(workspace_id) AND integration_id = sqlc.arg(integration_id)
  AND chat_id = sqlc.arg(chat_id) AND subject_kind = 'clickup_task' AND subject_key = sqlc.arg(task_id)
  AND thread_id IS NOT NULL;
