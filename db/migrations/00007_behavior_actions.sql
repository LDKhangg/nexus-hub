-- +goose Up
-- Behavior action library:
-- * behavior_messages records each Telegram message the engine sends for a
--   run under a stable key, claimed before the send, so a retried dispatch
--   never sends it again (a claim without a message ID is an unknown
--   outcome and is not resent);
-- * approvals gain two kinds: 'feedback' (the handle of the 👍/👎 buttons on
--   a message that decides nothing) and 'feedback_reason' (the owner's
--   optional reason for a 👎, answered as a free-text reply).
CREATE TABLE public.behavior_messages (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL,
    run_id bigint NOT NULL,
    msg_key text NOT NULL CHECK (msg_key ~ '^[a-z0-9_:-]{1,100}$'),
    telegram_message_id bigint CHECK (telegram_message_id IS NULL OR telegram_message_id > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    sent_at timestamptz,
    FOREIGN KEY (workspace_id, run_id) REFERENCES public.behavior_runs(workspace_id, id),
    UNIQUE (run_id, msg_key)
);
COMMENT ON TABLE public.behavior_messages IS 'One row per engine Telegram message of a run; holds identifiers only, never message text.';
ALTER TABLE public.behavior_messages ENABLE ROW LEVEL SECURITY;

ALTER TABLE public.approvals DROP CONSTRAINT approvals_kind_check;
ALTER TABLE public.approvals ADD CONSTRAINT approvals_kind_check
    CHECK (kind IN ('item_answer','post_draft','confirm_action','feedback','feedback_reason'));

-- +goose Down
DELETE FROM public.approvals WHERE kind IN ('feedback','feedback_reason');
ALTER TABLE public.approvals DROP CONSTRAINT approvals_kind_check;
ALTER TABLE public.approvals ADD CONSTRAINT approvals_kind_check
    CHECK (kind IN ('item_answer','post_draft','confirm_action'));
DROP TABLE public.behavior_messages;
