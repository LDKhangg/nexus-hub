-- +goose Up
-- Telegram inbound plumbing: an update remembers the integration (bot) it
-- came through so replies go out through the same bot, and a short note
-- code for content the handler refused to keep (never the content itself).
-- An approval can be put in free-text mode by naming the Telegram message
-- whose replies count as its answer; the owner's reply is staged on the
-- approval (not in telegram_updates) until the worker hands it on.
ALTER TABLE public.telegram_updates
    ADD COLUMN integration_id uuid,
    ADD COLUMN note text NOT NULL DEFAULT '' CHECK (note ~ '^[a-z_]{0,32}$'),
    ADD CONSTRAINT telegram_updates_integration_fk
        FOREIGN KEY (workspace_id, integration_id) REFERENCES public.integrations(workspace_id, id);
COMMENT ON COLUMN public.telegram_updates.note IS 'A short code such as args_too_long; never message text.';

ALTER TABLE public.approvals
    ADD COLUMN reply_message_id bigint CHECK (reply_message_id IS NULL OR reply_message_id > 0),
    ADD COLUMN pending_answer text CHECK (pending_answer IS NULL OR char_length(pending_answer) <= 4000);
CREATE INDEX approvals_open_reply_idx ON public.approvals(workspace_id, reply_message_id)
    WHERE decision IS NULL AND reply_message_id IS NOT NULL;
COMMENT ON COLUMN public.approvals.reply_message_id IS 'Free-text mode: a Telegram reply to this message is the answer.';
COMMENT ON COLUMN public.approvals.pending_answer IS 'The owner''s free-text reply, staged until the worker hands it on.';

-- +goose Down
DROP INDEX public.approvals_open_reply_idx;
ALTER TABLE public.approvals DROP COLUMN pending_answer, DROP COLUMN reply_message_id;
ALTER TABLE public.telegram_updates DROP CONSTRAINT telegram_updates_integration_fk,
    DROP COLUMN note, DROP COLUMN integration_id;
