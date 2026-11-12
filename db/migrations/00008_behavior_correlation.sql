-- +goose Up
-- Correlation IDs on the audit rows of a behavior run: every tool run of a
-- behavior run and every engine Telegram message carry the run's
-- correlation ID, copied from behavior_runs when the row is inserted, so
-- logs, the tool audit and sent messages can be joined by one ID.
ALTER TABLE public.tool_runs ADD COLUMN correlation_id uuid;
UPDATE public.tool_runs t SET correlation_id = r.correlation_id
FROM public.behavior_runs r WHERE t.behavior_run_id = r.id;
ALTER TABLE public.tool_runs ADD CONSTRAINT tool_runs_behavior_correlation
    CHECK (behavior_run_id IS NULL OR correlation_id IS NOT NULL);
COMMENT ON COLUMN public.tool_runs.correlation_id IS 'The behavior run''s correlation ID; NULL for agent job rows.';

ALTER TABLE public.behavior_messages ADD COLUMN correlation_id uuid;
UPDATE public.behavior_messages m SET correlation_id = r.correlation_id
FROM public.behavior_runs r WHERE m.run_id = r.id;
ALTER TABLE public.behavior_messages ALTER COLUMN correlation_id SET NOT NULL;

CREATE INDEX behavior_runs_correlation_idx ON public.behavior_runs(workspace_id, correlation_id);

-- +goose Down
DROP INDEX public.behavior_runs_correlation_idx;
ALTER TABLE public.behavior_messages DROP COLUMN correlation_id;
ALTER TABLE public.tool_runs DROP CONSTRAINT tool_runs_behavior_correlation;
ALTER TABLE public.tool_runs DROP COLUMN correlation_id;
