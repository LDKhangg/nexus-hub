-- +goose Up
-- Batched notify/FYI messages (profile quiet hours and digest mode):
-- * behavior_digest_entries queues a run's digest or FYI message for a
--   later batch instead of sending it at once. The run's message key is
--   claimed in behavior_messages in the same transaction, so the message is
--   either sent per run or batched, never both. Entries hold identifiers
--   only; the batch text is built from the run's items when it is sent.
-- * behavior_digest_batches is one batched Telegram message to one chat.
--   A batch is claimed before the send; a failed send releases its entries,
--   a batch that was claimed but never recorded is an unknown outcome and
--   is not resent.
CREATE TABLE public.behavior_digest_batches (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    integration_id uuid NOT NULL,
    chat_id bigint NOT NULL,
    status text NOT NULL DEFAULT 'sending' CHECK (status IN ('sending','sent')),
    claim_token uuid,
    telegram_message_id bigint CHECK (telegram_message_id IS NULL OR telegram_message_id > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    sent_at timestamptz,
    CHECK ((status = 'sent') = (telegram_message_id IS NOT NULL))
);
COMMENT ON TABLE public.behavior_digest_batches IS 'One batched engine Telegram message; identifiers only, never message text.';
ALTER TABLE public.behavior_digest_batches ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.behavior_digest_entries (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL,
    run_id bigint NOT NULL,
    msg_key text NOT NULL CHECK (msg_key IN ('digest','fyi')),
    integration_id uuid NOT NULL,
    chat_id bigint NOT NULL,
    due_at timestamptz NOT NULL,
    batch_id bigint REFERENCES public.behavior_digest_batches(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, run_id) REFERENCES public.behavior_runs(workspace_id, id),
    UNIQUE (run_id, msg_key)
);
CREATE INDEX behavior_digest_entries_due_idx ON public.behavior_digest_entries(due_at, id) WHERE batch_id IS NULL;
COMMENT ON TABLE public.behavior_digest_entries IS 'A run''s notify/FYI message waiting for its batch; identifiers only.';
ALTER TABLE public.behavior_digest_entries ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE public.behavior_digest_entries;
DROP TABLE public.behavior_digest_batches;
