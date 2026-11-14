-- +goose Up
ALTER TABLE public.telegram_updates
    ADD COLUMN telegram_message_id bigint,
    ADD COLUMN reply_to_message_id bigint;

COMMENT ON COLUMN public.telegram_updates.telegram_message_id IS 'Telegram message identifier only; no message body is stored.';
COMMENT ON COLUMN public.telegram_updates.reply_to_message_id IS 'Telegram replied-to message identifier only; no message body is stored.';

-- +goose Down
ALTER TABLE public.telegram_updates
    DROP COLUMN reply_to_message_id,
    DROP COLUMN telegram_message_id;
