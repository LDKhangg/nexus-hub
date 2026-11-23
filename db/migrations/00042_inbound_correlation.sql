-- +goose Up
ALTER TABLE public.telegram_updates
    ADD COLUMN correlation_id text
    CONSTRAINT telegram_updates_correlation_id_format
    CHECK (correlation_id IS NULL OR correlation_id ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$');

COMMENT ON COLUMN public.telegram_updates.correlation_id IS 'Request correlation ID propagated into asynchronous behavior and workflow execution.';

-- +goose Down
ALTER TABLE public.telegram_updates DROP COLUMN correlation_id;
