-- +goose Up
-- Threads remain independent. Work-item grouping and vector search come later.
-- Application code must refresh updated_at when changing a row.
-- Go generates UUID v7 IDs and supplies them on INSERT; no UUID v4 fallback.
-- The uuid type stores any UUID version; the application enforces UUID v7.
CREATE TABLE public.workspaces (
    id uuid PRIMARY KEY,
    display_name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT workspaces_display_name_not_blank CHECK (btrim(display_name) <> '')
);

CREATE TABLE public.integrations (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces (id),
    platform text NOT NULL,
    role text NOT NULL DEFAULT 'source',
    encrypted_credentials text,
    config jsonb NOT NULL DEFAULT '{}'::jsonb,
    is_enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT integrations_workspace_id_id_key UNIQUE (workspace_id, id),
    CONSTRAINT integrations_platform_not_blank CHECK (btrim(platform) <> ''),
    CONSTRAINT integrations_role_check CHECK (role IN ('source', 'control', 'both')),
    CONSTRAINT integrations_config_object CHECK (jsonb_typeof(config) = 'object')
);

-- Store ciphertext only; encryption keys must live outside the database.
COMMENT ON COLUMN public.integrations.encrypted_credentials IS
    'Application-encrypted credentials; never store plaintext tokens or keys.';

CREATE TABLE public.hub_threads (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces (id),
    integration_id uuid NOT NULL,
    external_scope text NOT NULL DEFAULT '',
    thread_type text NOT NULL,
    external_id text NOT NULL,
    title text NOT NULL DEFAULT '',
    rolling_summary text NOT NULL DEFAULT '',
    summary_through_message_id bigint,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT hub_threads_workspace_id_id_key UNIQUE (workspace_id, id),
    CONSTRAINT hub_threads_external_identity_key
        UNIQUE (integration_id, external_scope, thread_type, external_id),
    CONSTRAINT hub_threads_integration_workspace_fkey
        FOREIGN KEY (workspace_id, integration_id)
        REFERENCES public.integrations (workspace_id, id),
    CONSTRAINT hub_threads_type_not_blank CHECK (btrim(thread_type) <> ''),
    CONSTRAINT hub_threads_external_id_not_blank CHECK (btrim(external_id) <> ''),
    CONSTRAINT hub_threads_metadata_object CHECK (jsonb_typeof(metadata) = 'object')
);

COMMENT ON COLUMN public.hub_threads.external_scope IS
    'Stable source namespace, e.g. repository ID or channel ID. Empty only when the external ID needs no additional namespace within this integration and type.';
COMMENT ON COLUMN public.hub_threads.thread_type IS
    'Adapter-defined kind, e.g. task, pull_request, issue, conversation. Not a platform-specific enum.';

CREATE TABLE public.message_logs (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    thread_id uuid NOT NULL REFERENCES public.hub_threads (id),
    external_message_id text,
    sender_id text,
    sender_name text NOT NULL DEFAULT '',
    sender_role text NOT NULL,
    content text NOT NULL,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    -- This index also supports recent-message and summary-tail queries.
    CONSTRAINT message_logs_thread_id_id_key UNIQUE (thread_id, id),
    CONSTRAINT message_logs_external_identity_key UNIQUE (thread_id, external_message_id),
    CONSTRAINT message_logs_external_id_not_blank
        CHECK (external_message_id IS NULL OR btrim(external_message_id) <> ''),
    CONSTRAINT message_logs_sender_role_check
        CHECK (sender_role IN ('external_user', 'human_owner', 'ai_router', 'ai_actor', 'system')),
    CONSTRAINT message_logs_metadata_object CHECK (jsonb_typeof(metadata) = 'object')
);

COMMENT ON COLUMN public.message_logs.external_message_id IS
    'Stable source message identity scoped to this thread, including a source subtype if needed. NULL for messages without a source ID; not a webhook delivery ID.';
COMMENT ON COLUMN public.message_logs.occurred_at IS
    'Source timestamp when available; created_at records ingestion time.';

-- A summary cursor must point to a message belonging to this exact thread.
ALTER TABLE public.hub_threads
    ADD CONSTRAINT hub_threads_summary_message_fkey
    FOREIGN KEY (id, summary_through_message_id)
    REFERENCES public.message_logs (thread_id, id);

COMMENT ON COLUMN public.hub_threads.summary_through_message_id IS
    'Last ingestion-order message covered by rolling_summary. Serialize per-thread writes and update summary plus cursor atomically; IDs alone do not establish concurrent commit order.';

-- Links are symmetric related-to relationships, not ownership or access grants.
-- Always store the smaller UUID first to prevent duplicate reverse links.
CREATE TABLE public.thread_links (
    workspace_id uuid NOT NULL REFERENCES public.workspaces (id),
    source_thread_id uuid NOT NULL,
    target_thread_id uuid NOT NULL,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, source_thread_id, target_thread_id),
    CONSTRAINT thread_links_source_workspace_fkey
        FOREIGN KEY (workspace_id, source_thread_id)
        REFERENCES public.hub_threads (workspace_id, id),
    CONSTRAINT thread_links_target_workspace_fkey
        FOREIGN KEY (workspace_id, target_thread_id)
        REFERENCES public.hub_threads (workspace_id, id),
    CONSTRAINT thread_links_canonical_order CHECK (source_thread_id < target_thread_id),
    CONSTRAINT thread_links_metadata_object CHECK (jsonb_typeof(metadata) = 'object')
);

CREATE INDEX thread_links_target_idx
    ON public.thread_links (workspace_id, target_thread_id, source_thread_id);

COMMENT ON TABLE public.thread_links IS
    'Explicit same-workspace related-to links. A link neither authorizes access nor automatically merges context. Metadata may record the confirmed URL or human decision.';

-- Backend-only baseline: no frontend policies are granted by this migration.
-- The backend needs a trusted owner/BYPASSRLS role and must enforce workspace
-- authorization itself. Add reviewed policies before exposing a client API.
ALTER TABLE public.workspaces ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.integrations ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.hub_threads ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.message_logs ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.thread_links ENABLE ROW LEVEL SECURITY;

-- +goose Down
-- Destructive rollback: all messages, summaries, links and integrations are lost.
DROP TABLE public.thread_links;
ALTER TABLE public.hub_threads DROP CONSTRAINT hub_threads_summary_message_fkey;
DROP TABLE public.message_logs;
DROP TABLE public.hub_threads;
DROP TABLE public.integrations;
DROP TABLE public.workspaces;
