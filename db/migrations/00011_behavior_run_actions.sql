-- +goose Up
-- Run-level actions (actions.run): each run keeps the outcome of every
-- run-level action it executed (type, target identifiers, outcome codes,
-- review or message IDs), so a retried run never repeats an action already
-- recorded and trace and undo can show and reverse them. Identifiers and
-- codes only; never message, pull request or prompt text.
ALTER TABLE public.behavior_runs ADD COLUMN run_actions jsonb
    CHECK (run_actions IS NULL OR (jsonb_typeof(run_actions) = 'object' AND octet_length(run_actions::text) <= 65536));
COMMENT ON COLUMN public.behavior_runs.run_actions IS 'Outcomes of the run-level actions: types, identifiers and codes only, never message text.';

-- +goose Down
ALTER TABLE public.behavior_runs DROP COLUMN run_actions;
