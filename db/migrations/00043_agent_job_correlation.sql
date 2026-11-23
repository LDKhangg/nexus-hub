-- +goose Up
ALTER TABLE public.agent_jobs
    ADD COLUMN correlation_id text
    CONSTRAINT agent_jobs_correlation_id_format
    CHECK (correlation_id IS NULL OR correlation_id ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$');

COMMENT ON COLUMN public.agent_jobs.correlation_id IS 'Inbound request correlation ID propagated into the agent workflow execution.';

-- +goose Down
ALTER TABLE public.agent_jobs DROP COLUMN correlation_id;
