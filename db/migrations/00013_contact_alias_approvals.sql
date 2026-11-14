-- +goose Up
ALTER TABLE public.approvals DROP CONSTRAINT approvals_kind_check;
ALTER TABLE public.approvals ADD CONSTRAINT approvals_kind_check
    CHECK (kind IN ('item_answer','post_draft','confirm_action','feedback','feedback_reason','contact_alias'));

-- +goose Down
DELETE FROM public.approvals WHERE kind = 'contact_alias';
ALTER TABLE public.approvals DROP CONSTRAINT approvals_kind_check;
ALTER TABLE public.approvals ADD CONSTRAINT approvals_kind_check
    CHECK (kind IN ('item_answer','post_draft','confirm_action','feedback','feedback_reason'));
