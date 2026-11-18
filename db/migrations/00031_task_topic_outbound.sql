-- +goose Up
-- Phase 10 R1: a Telegram message that shows a ClickUp comment maps to that
-- comment, so a reply to the message can answer the comment in ClickUp.
-- Identifiers only: no message text, comment text or names are stored.
CREATE TABLE public.telegram_message_links (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    integration_id uuid NOT NULL,
    chat_id bigint NOT NULL CHECK (chat_id <> 0),
    message_id bigint NOT NULL CHECK (message_id > 0),
    subject_kind text NOT NULL CHECK (subject_kind IN ('clickup_comment')),
    task_id text NOT NULL CHECK (btrim(task_id) <> '' AND octet_length(task_id) <= 64),
    comment_id text NOT NULL CHECK (btrim(comment_id) <> '' AND octet_length(comment_id) <= 64),
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, integration_id) REFERENCES public.integrations(workspace_id, id),
    UNIQUE (workspace_id, integration_id, chat_id, message_id),
    UNIQUE (workspace_id, id)
);
CREATE INDEX telegram_message_links_created_idx ON public.telegram_message_links (created_at);
COMMENT ON TABLE public.telegram_message_links IS 'Workspace-scoped map of a Telegram message to the ClickUp comment it shows; ids only, pruned after the retention setting.';
ALTER TABLE public.telegram_message_links ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE public.telegram_message_links;
