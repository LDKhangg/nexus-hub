-- +goose Up
-- A job queued to resume a step woken by a recorded decision continues the
-- step's current attempt: its claim does not add an attempt, so approval
-- rounds never consume the retry budget. The flag is cleared by the claim,
-- so a resumed claim that fails or loses its lease is retried as a real
-- next attempt.
ALTER TABLE public.jobs ADD COLUMN continues_attempt boolean NOT NULL DEFAULT false;
COMMENT ON COLUMN public.jobs.continues_attempt IS 'True while the pending job resumes a woken step within its current attempt; its claim does not count an attempt.';

-- A wait is closed when its execution ends. A later decision of a closed
-- wait is still recorded on the approval (and its signal), but no step is
-- ever woken to apply it.
ALTER TABLE public.workflow_waits ADD COLUMN closed_at timestamptz;
COMMENT ON COLUMN public.workflow_waits.closed_at IS 'When the execution ended; a decision after it is recorded but never applied.';

-- +goose Down
ALTER TABLE public.workflow_waits DROP COLUMN closed_at;
ALTER TABLE public.jobs DROP COLUMN continues_attempt;
