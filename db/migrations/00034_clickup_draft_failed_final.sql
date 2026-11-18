-- +goose Up
-- Phase 10 review A (R-A-1): a post that failed with no safe retry (dry
-- run, unknown outcome, denied) ends the draft in `failed_final`: the body
-- is cleared and finished_at is set, like the other terminal states. A
-- `failed` draft still keeps its body for "Thử lại" until it expires.
ALTER TABLE public.clickup_drafts DROP CONSTRAINT clickup_drafts_state_check;
ALTER TABLE public.clickup_drafts ADD CONSTRAINT clickup_drafts_state_check
    CHECK (state IN ('pending', 'posting', 'posted', 'cancelled', 'expired', 'failed', 'failed_final'));
ALTER TABLE public.clickup_drafts DROP CONSTRAINT clickup_drafts_check;
ALTER TABLE public.clickup_drafts ADD CONSTRAINT clickup_drafts_check
    CHECK ((state IN ('posted', 'cancelled', 'expired', 'failed_final')) = (body IS NULL));
ALTER TABLE public.clickup_drafts DROP CONSTRAINT clickup_drafts_check1;
ALTER TABLE public.clickup_drafts ADD CONSTRAINT clickup_drafts_check1
    CHECK ((state IN ('posted', 'cancelled', 'expired', 'failed_final')) = (finished_at IS NOT NULL));

-- +goose Down
-- A finally failed draft has no body and can never be posted again, so it
-- becomes `cancelled`; its reason code (for example unknown_outcome) stays.
UPDATE public.clickup_drafts SET state = 'cancelled' WHERE state = 'failed_final';
ALTER TABLE public.clickup_drafts DROP CONSTRAINT clickup_drafts_check1;
ALTER TABLE public.clickup_drafts ADD CONSTRAINT clickup_drafts_check1
    CHECK ((state IN ('posted', 'cancelled', 'expired')) = (finished_at IS NOT NULL));
ALTER TABLE public.clickup_drafts DROP CONSTRAINT clickup_drafts_check;
ALTER TABLE public.clickup_drafts ADD CONSTRAINT clickup_drafts_check
    CHECK ((state IN ('posted', 'cancelled', 'expired')) = (body IS NULL));
ALTER TABLE public.clickup_drafts DROP CONSTRAINT clickup_drafts_state_check;
ALTER TABLE public.clickup_drafts ADD CONSTRAINT clickup_drafts_state_check
    CHECK (state IN ('pending', 'posting', 'posted', 'cancelled', 'expired', 'failed'));
