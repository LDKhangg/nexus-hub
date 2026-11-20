-- +goose Up
-- Retention may remove old execution events, but normal application writes
-- remain append-only. The sweeper enables this transaction-local gate only
-- after locking an eligible terminal execution batch.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.execution_events_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND current_setting('nexushub.retention_sweep', true) = 'on' THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'execution_events rows are immutable' USING ERRCODE = 'check_violation';
END;
$$;
-- +goose StatementEnd

CREATE INDEX message_logs_retention_idx ON public.message_logs (created_at, id);
CREATE INDEX tool_runs_retention_idx ON public.tool_runs (created_at, id) WHERE status <> 'started';
CREATE INDEX policy_decisions_retention_idx ON public.policy_decisions (created_at, id);
CREATE INDEX notification_outbox_retention_idx ON public.notification_outbox (updated_at, id) WHERE status IN ('sent','failed');
CREATE INDEX outbound_actions_retention_idx ON public.outbound_actions (updated_at, id) WHERE status IN ('sent','failed');

-- +goose Down
DROP INDEX public.outbound_actions_retention_idx;
DROP INDEX public.notification_outbox_retention_idx;
DROP INDEX public.policy_decisions_retention_idx;
DROP INDEX public.tool_runs_retention_idx;
DROP INDEX public.message_logs_retention_idx;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.execution_events_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'execution_events rows are immutable' USING ERRCODE = 'check_violation';
END;
$$;
-- +goose StatementEnd
