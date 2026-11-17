-- +goose Up
-- The pinned task link message of a ClickUp task topic is posted at most once
-- per topic. NULL: never posted; 0: claimed (posting, or an unclear outcome
-- that is never retried); > 0: the Telegram message id. No text is stored.
ALTER TABLE public.telegram_topics
    ADD COLUMN link_message_id bigint CHECK (link_message_id IS NULL OR link_message_id >= 0);
COMMENT ON COLUMN public.telegram_topics.link_message_id IS 'Telegram message id of the pinned task link; NULL never posted, 0 claimed. Ids only.';

-- /task remembers the repository of the last local task of a ClickUp task.
CREATE INDEX worker_task_routes_clickup_task_idx
    ON public.worker_task_routes (workspace_id, clickup_task_id, created_at DESC);

-- +goose Down
DROP INDEX public.worker_task_routes_clickup_task_idx;
ALTER TABLE public.telegram_topics DROP COLUMN link_message_id;
