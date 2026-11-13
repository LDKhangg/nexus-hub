-- +goose Up
-- assistant_action: a write the Telegram assistant proposed ("Làm"/"Huỷ").
-- The row holds only the intent and its minimal arguments (ids, the alias
-- text, the style), never the owner's message.
ALTER TABLE public.approvals DROP CONSTRAINT approvals_kind_check;
ALTER TABLE public.approvals ADD CONSTRAINT approvals_kind_check
    CHECK (kind IN ('item_answer','post_draft','confirm_action','feedback','feedback_reason','contact_alias','assistant_action'));

-- +goose Down
DELETE FROM public.approvals WHERE kind = 'assistant_action';
ALTER TABLE public.approvals DROP CONSTRAINT approvals_kind_check;
ALTER TABLE public.approvals ADD CONSTRAINT approvals_kind_check
    CHECK (kind IN ('item_answer','post_draft','confirm_action','feedback','feedback_reason','contact_alias'));
