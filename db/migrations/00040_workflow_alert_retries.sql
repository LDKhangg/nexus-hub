-- +goose Up
-- Phase 12 review B: bounded alert retries and a database rate cap.
-- The claim row of an alert is kept for its whole life: the watcher
-- inserts it as pending (or suppressed when the hourly cap is reached),
-- marks it sent after a delivered send, and counts a failed send as one
-- attempt with a backoff; after the last attempt the row is failed and is
-- never sent again. The hourly cap counts rows sent in the last hour, and
-- the merged-alerts summary is built from suppressed rows not yet
-- summarised, so a restart keeps both. Ids, states, times and codes only:
-- never message text, prompts or payloads.
ALTER TABLE public.workflow_alerts
    ADD COLUMN status text NOT NULL DEFAULT 'sent' CHECK (status IN ('sent','pending','failed','suppressed')),
    ADD COLUMN attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0 AND attempts <= 100),
    ADD COLUMN next_attempt_at timestamptz,
    ADD COLUMN last_error_code text CHECK (last_error_code IS NULL OR last_error_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    ADD COLUMN since_at timestamptz,
    ADD COLUMN error_code text CHECK (error_code IS NULL OR error_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    ADD COLUMN created_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN summarised_at timestamptz;
-- Rows of 00039 were claimed (sent or merged); keep them as sent, claimed
-- at their old time, and never summarise them again.
UPDATE public.workflow_alerts SET created_at = sent_at, attempts = 1;
ALTER TABLE public.workflow_alerts ALTER COLUMN status SET DEFAULT 'pending';
ALTER TABLE public.workflow_alerts ALTER COLUMN sent_at DROP NOT NULL;
ALTER TABLE public.workflow_alerts ALTER COLUMN sent_at DROP DEFAULT;
ALTER TABLE public.workflow_alerts ADD CONSTRAINT workflow_alerts_sent_at_status_check
    CHECK ((status = 'sent') = (sent_at IS NOT NULL));
ALTER TABLE public.workflow_alerts ADD CONSTRAINT workflow_alerts_next_attempt_status_check
    CHECK ((status = 'pending') = (next_attempt_at IS NOT NULL));
CREATE INDEX workflow_alerts_retry_idx ON public.workflow_alerts (next_attempt_at, created_at)
    WHERE status = 'pending';
CREATE INDEX workflow_alerts_unsummarised_idx ON public.workflow_alerts (workspace_id, created_at)
    WHERE status = 'suppressed' AND summarised_at IS NULL;
CREATE INDEX workflow_alerts_created_idx ON public.workflow_alerts (created_at);
-- The failed-execution alert scan and nh status read FAILED executions by
-- finish time.
CREATE INDEX workflow_executions_failed_finished_idx ON public.workflow_executions (finished_at)
    WHERE state = 'FAILED';
COMMENT ON COLUMN public.workflow_alerts.status IS 'sent, pending (send not yet delivered), failed (attempts exhausted) or suppressed (hourly cap).';
COMMENT ON COLUMN public.workflow_alerts.sent_at IS 'When the alert was delivered; NULL unless status is sent.';
COMMENT ON COLUMN public.workflow_alerts.attempts IS 'Send attempts made, including a timed-out one.';
COMMENT ON COLUMN public.workflow_alerts.next_attempt_at IS 'When a pending alert is tried (again); NULL unless pending.';
COMMENT ON COLUMN public.workflow_alerts.last_error_code IS 'Safe code of the last failed send attempt.';
COMMENT ON COLUMN public.workflow_alerts.since_at IS 'When the alert state began (for the age in the alert line).';
COMMENT ON COLUMN public.workflow_alerts.error_code IS 'Safe error code of the execution shown in the alert line.';
COMMENT ON COLUMN public.workflow_alerts.created_at IS 'When the alert was claimed.';
COMMENT ON COLUMN public.workflow_alerts.summarised_at IS 'When a suppressed alert was counted in a merged-alerts summary.';

-- +goose Down
-- Back to the 00039 shape: a pending claim is deleted so the old watcher
-- retries it; every other row stays a claim with a time.
DROP INDEX public.workflow_executions_failed_finished_idx;
DROP INDEX public.workflow_alerts_created_idx;
DROP INDEX public.workflow_alerts_unsummarised_idx;
DROP INDEX public.workflow_alerts_retry_idx;
DELETE FROM public.workflow_alerts WHERE status = 'pending';
ALTER TABLE public.workflow_alerts DROP CONSTRAINT workflow_alerts_next_attempt_status_check;
ALTER TABLE public.workflow_alerts DROP CONSTRAINT workflow_alerts_sent_at_status_check;
UPDATE public.workflow_alerts SET sent_at = created_at WHERE sent_at IS NULL;
ALTER TABLE public.workflow_alerts ALTER COLUMN sent_at SET DEFAULT now();
ALTER TABLE public.workflow_alerts ALTER COLUMN sent_at SET NOT NULL;
ALTER TABLE public.workflow_alerts
    DROP COLUMN summarised_at,
    DROP COLUMN created_at,
    DROP COLUMN error_code,
    DROP COLUMN since_at,
    DROP COLUMN last_error_code,
    DROP COLUMN next_attempt_at,
    DROP COLUMN attempts,
    DROP COLUMN status;
COMMENT ON COLUMN public.workflow_alerts.sent_at IS 'When the alert was claimed; a row of an alert merged into the hourly summary (rate cap) is kept too.';
