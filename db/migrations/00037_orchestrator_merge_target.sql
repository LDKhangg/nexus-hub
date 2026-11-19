-- +goose Up
-- Phase 9 P3: a github.merge plan task names the pull request it merges:
-- repository, number and the exact head commit SHA from earlier task
-- evidence. Ids and a SHA only; the owner approves exactly this head.
ALTER TABLE public.orchestrator_plan_tasks
    ADD COLUMN merge_target jsonb CHECK (merge_target IS NULL OR (
        jsonb_typeof(merge_target) = 'object' AND octet_length(merge_target::text) <= 512
        AND merge_target ? 'repo' AND merge_target ? 'number' AND merge_target ? 'head_sha'
        AND (merge_target ->> 'head_sha') ~ '^[0-9a-f]{40}$'));
COMMENT ON COLUMN public.orchestrator_plan_tasks.merge_target IS 'github.merge task: {repo, number, head_sha}; NULL for other tasks.';

-- D-A-1: an open gate (decision NULL) made the old CHECK NULL, so it passed
-- and consumed_at could be set on an undecided gate. Only a decided,
-- approved policy gate may be consumed.
ALTER TABLE public.approvals DROP CONSTRAINT approvals_consumed_check;
ALTER TABLE public.approvals ADD CONSTRAINT approvals_consumed_check
    CHECK (consumed_at IS NULL OR (kind = 'policy_gate' AND decision IS NOT DISTINCT FROM 'approved'));

-- +goose Down
ALTER TABLE public.approvals DROP CONSTRAINT approvals_consumed_check;
ALTER TABLE public.approvals ADD CONSTRAINT approvals_consumed_check
    CHECK (consumed_at IS NULL OR (kind = 'policy_gate' AND decision = 'approved'));
ALTER TABLE public.orchestrator_plan_tasks DROP COLUMN merge_target;
