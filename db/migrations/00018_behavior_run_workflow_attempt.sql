-- +goose Up
-- A behavior run executed as durable workflow steps keeps the deadline of
-- its current attempt here, so splitting one attempt into several steps
-- never extends its wall-clock allowance. Legacy claims never read it.
ALTER TABLE public.behavior_runs ADD COLUMN workflow_attempt_deadline timestamptz;
COMMENT ON COLUMN public.behavior_runs.workflow_attempt_deadline IS 'Deadline of the current attempt of a run executed by durable workflow steps; NULL otherwise.';

-- +goose Down
ALTER TABLE public.behavior_runs DROP COLUMN workflow_attempt_deadline;
