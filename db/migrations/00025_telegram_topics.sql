-- +goose Up
-- Topic records hold routing identifiers only: a subject key such as
-- clickup_task:<id>, the Telegram thread id and the state. Topic names, task
-- names and message text are never stored.
CREATE TABLE public.telegram_topics (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    integration_id uuid NOT NULL,
    chat_id bigint NOT NULL,
    -- NULL while the topic is reserved but the Telegram topic is not created yet.
    thread_id bigint CHECK (thread_id IS NULL OR thread_id > 0),
    subject_kind text NOT NULL CHECK (subject_kind IN ('clickup_task', 'workflow', 'fixed')),
    subject_key text NOT NULL CHECK (btrim(subject_key) <> '' AND octet_length(subject_key) <= 128),
    state text NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'closed')),
    reserved_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    closed_at timestamptz,
    CHECK ((state = 'closed') = (closed_at IS NOT NULL)),
    FOREIGN KEY (workspace_id, integration_id) REFERENCES public.integrations(workspace_id, id),
    UNIQUE (workspace_id, integration_id, chat_id, subject_kind, subject_key),
    UNIQUE (workspace_id, integration_id, chat_id, thread_id),
    UNIQUE (workspace_id, id)
);
COMMENT ON TABLE public.telegram_topics IS 'Workspace-scoped binding of a Telegram forum topic to one subject; topic names, task names and message text are never stored.';
COMMENT ON COLUMN public.telegram_topics.thread_id IS 'Telegram message_thread_id; NULL while the topic is only reserved.';
ALTER TABLE public.telegram_topics ENABLE ROW LEVEL SECURITY;

ALTER TABLE public.conversations
    ADD COLUMN thread_id bigint NOT NULL DEFAULT 0 CHECK (thread_id >= 0);
COMMENT ON COLUMN public.conversations.thread_id IS 'Telegram message_thread_id; 0 is the chat itself (General).';
ALTER TABLE public.conversations
    DROP CONSTRAINT conversations_workspace_id_integration_id_chat_id_key;
ALTER TABLE public.conversations
    ADD CONSTRAINT conversations_workspace_integration_chat_thread_key
    UNIQUE (workspace_id, integration_id, chat_id, thread_id);

ALTER TABLE public.telegram_updates
    ADD COLUMN message_thread_id bigint;
COMMENT ON COLUMN public.telegram_updates.message_thread_id IS 'Telegram topic identifier only; no message body is stored.';

-- +goose Down
ALTER TABLE public.telegram_updates
    DROP COLUMN message_thread_id;
-- Keep owner data: fold topic conversations into the chat's thread-0 one.
INSERT INTO public.conversations (workspace_id, integration_id, chat_id, user_id, summary, summary_updated_at)
SELECT DISTINCT ON (workspace_id, integration_id, chat_id) workspace_id, integration_id, chat_id, user_id, '', NULL
FROM public.conversations t
WHERE t.thread_id <> 0
ORDER BY workspace_id, integration_id, chat_id, id
ON CONFLICT (workspace_id, integration_id, chat_id, thread_id) DO NOTHING;
CREATE TEMP TABLE conversation_fold ON COMMIT DROP AS
SELECT t.id AS old_id, g.id AS new_id, t.workspace_id
FROM public.conversations t
JOIN public.conversations g ON g.workspace_id = t.workspace_id AND g.integration_id = t.integration_id
    AND g.chat_id = t.chat_id AND g.thread_id = 0
WHERE t.thread_id <> 0;
UPDATE public.workflow_instructions w SET conversation_id = f.new_id
FROM conversation_fold f WHERE w.workspace_id = f.workspace_id AND w.conversation_id = f.old_id;
UPDATE public.conversation_messages m SET conversation_id = f.new_id
FROM conversation_fold f WHERE m.workspace_id = f.workspace_id AND m.conversation_id = f.old_id;
INSERT INTO public.conversation_workflows (workspace_id, conversation_id, execution_id, source, created_at)
SELECT w.workspace_id, f.new_id, w.execution_id, w.source, w.created_at
FROM public.conversation_workflows w
JOIN conversation_fold f ON w.workspace_id = f.workspace_id AND w.conversation_id = f.old_id
ON CONFLICT (workspace_id, conversation_id, execution_id) DO NOTHING;
DELETE FROM public.conversation_workflows w
USING conversation_fold f WHERE w.workspace_id = f.workspace_id AND w.conversation_id = f.old_id;
DELETE FROM public.conversations c
USING conversation_fold f WHERE c.workspace_id = f.workspace_id AND c.id = f.old_id;
ALTER TABLE public.conversations
    DROP CONSTRAINT conversations_workspace_integration_chat_thread_key;
ALTER TABLE public.conversations
    DROP COLUMN thread_id;
ALTER TABLE public.conversations
    ADD CONSTRAINT conversations_workspace_id_integration_id_chat_id_key
    UNIQUE (workspace_id, integration_id, chat_id);
DROP TABLE public.telegram_topics;
