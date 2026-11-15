-- +goose Up
-- Conversation records contain routing metadata and bounded operator-authored
-- context only. Raw owner and assistant messages are never stored here.
CREATE TABLE public.conversations (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    integration_id uuid NOT NULL,
    chat_id bigint NOT NULL,
    user_id uuid,
    summary text NOT NULL DEFAULT '' CHECK (octet_length(summary) <= 2000),
    summary_updated_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, integration_id) REFERENCES public.integrations(workspace_id, id),
    UNIQUE (workspace_id, integration_id, chat_id),
    UNIQUE (workspace_id, id)
);
COMMENT ON TABLE public.conversations IS 'Workspace-scoped conversation routing and bounded sanitized summary; raw chat text is never stored.';
ALTER TABLE public.conversations ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.conversation_messages (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL,
    conversation_id bigint NOT NULL,
    direction text NOT NULL CHECK (direction IN ('in','out')),
    kind text NOT NULL CHECK (btrim(kind) <> '' AND octet_length(kind) <= 64),
    telegram_message_id bigint NOT NULL,
    reply_to_message_id bigint,
    execution_id bigint,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, conversation_id) REFERENCES public.conversations(workspace_id, id),
    FOREIGN KEY (workspace_id, execution_id) REFERENCES public.workflow_executions(workspace_id, id),
    UNIQUE (workspace_id, id)
);
COMMENT ON TABLE public.conversation_messages IS 'Message identifiers and routing metadata only; message bodies are never stored.';
CREATE INDEX conversation_messages_conversation_idx ON public.conversation_messages(workspace_id, conversation_id, created_at, id);
ALTER TABLE public.conversation_messages ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.conversation_workflows (
    workspace_id uuid NOT NULL,
    conversation_id bigint NOT NULL,
    execution_id bigint NOT NULL,
    source text NOT NULL CHECK (btrim(source) <> '' AND octet_length(source) <= 64),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, conversation_id, execution_id),
    FOREIGN KEY (workspace_id, conversation_id) REFERENCES public.conversations(workspace_id, id),
    FOREIGN KEY (workspace_id, execution_id) REFERENCES public.workflow_executions(workspace_id, id)
);
COMMENT ON TABLE public.conversation_workflows IS 'Links conversations to workflow executions without storing chat content.';
CREATE INDEX conversation_workflows_execution_idx ON public.conversation_workflows(workspace_id, execution_id);
ALTER TABLE public.conversation_workflows ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.resource_bindings (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL,
    execution_id bigint NOT NULL,
    kind text NOT NULL CHECK (btrim(kind) <> '' AND octet_length(kind) <= 64),
    external_id text NOT NULL CHECK (btrim(external_id) <> '' AND octet_length(external_id) <= 512),
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, execution_id) REFERENCES public.workflow_executions(workspace_id, id),
    UNIQUE (workspace_id, kind, external_id, execution_id),
    UNIQUE (workspace_id, id)
);
COMMENT ON TABLE public.resource_bindings IS 'Trusted external resource identifiers bound to workflow executions; resource content is never stored.';
CREATE INDEX resource_bindings_lookup_idx ON public.resource_bindings(workspace_id, kind, external_id);
ALTER TABLE public.resource_bindings ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.workflow_instructions (
    workspace_id uuid NOT NULL,
    execution_id bigint NOT NULL,
    version bigint NOT NULL CHECK (version > 0),
    text text NOT NULL CHECK (btrim(text) <> '' AND char_length(text) <= 2000),
    conversation_id bigint NOT NULL,
    telegram_update_id bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, execution_id, version),
    FOREIGN KEY (workspace_id, execution_id) REFERENCES public.workflow_executions(workspace_id, id),
    FOREIGN KEY (workspace_id, conversation_id) REFERENCES public.conversations(workspace_id, id),
    UNIQUE (workspace_id, telegram_update_id)
);
COMMENT ON TABLE public.workflow_instructions IS 'Sanitized, explicitly supplied workflow instructions; prompts and surrounding messages are never stored.';
ALTER TABLE public.workflow_instructions ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE public.workflow_instructions;
DROP TABLE public.resource_bindings;
DROP TABLE public.conversation_workflows;
DROP TABLE public.conversation_messages;
DROP TABLE public.conversations;
