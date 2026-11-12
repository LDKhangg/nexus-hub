-- +goose Up
-- Phase 2 agent core: durable debounced agent jobs, outbound actions,
-- tool-run audit, encrypted credentials and global settings.
CREATE TABLE public.agent_jobs (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL,
    source_integration_id uuid NOT NULL,
    thread_id uuid NOT NULL,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','processing','done','failed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    run_at timestamptz NOT NULL,
    first_enqueued_at timestamptz NOT NULL DEFAULT now(),
    locked_until timestamptz,
    claim_token uuid,
    through_message_id bigint,
    last_error_code text,
    input_tokens integer NOT NULL DEFAULT 0,
    output_tokens integer NOT NULL DEFAULT 0,
    cache_read_tokens integer NOT NULL DEFAULT 0,
    cache_write_tokens integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, thread_id) REFERENCES public.hub_threads(workspace_id, id),
    FOREIGN KEY (workspace_id, source_integration_id) REFERENCES public.integrations(workspace_id, id)
);
CREATE UNIQUE INDEX agent_jobs_one_pending_idx ON public.agent_jobs(thread_id) WHERE status = 'pending';
CREATE INDEX agent_jobs_due_idx ON public.agent_jobs(status, run_at);
ALTER TABLE public.agent_jobs ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.outbound_actions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL,
    integration_id uuid NOT NULL,
    kind text NOT NULL CHECK (kind IN ('telegram_message')),
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    dedupe_key text NOT NULL CHECK (btrim(dedupe_key) <> ''),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','processing','sent','failed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    locked_until timestamptz,
    claim_token uuid,
    last_error_code text,
    provider_message_id bigint,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    sent_at timestamptz,
    FOREIGN KEY (workspace_id, integration_id) REFERENCES public.integrations(workspace_id, id),
    UNIQUE (kind, dedupe_key)
);
CREATE INDEX outbound_actions_due_idx ON public.outbound_actions(status, next_attempt_at, locked_until);
ALTER TABLE public.outbound_actions ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.tool_runs (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    agent_job_id bigint NOT NULL REFERENCES public.agent_jobs(id),
    step integer NOT NULL,
    tool text NOT NULL,
    tier text NOT NULL CHECK (tier IN ('read','write','dangerous')),
    repo text,
    account text,
    idempotency_key text NOT NULL,
    input_redacted jsonb,
    status text NOT NULL CHECK (status IN ('started','ok','error','denied','timeout','unknown_outcome','dry_run')),
    error_code text,
    exit_code integer,
    duration_ms bigint,
    output_bytes integer,
    result_summary text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX tool_runs_idempotency_idx ON public.tool_runs(idempotency_key)
    WHERE tier <> 'read' AND status IN ('started','ok');
CREATE INDEX tool_runs_job_idx ON public.tool_runs(agent_job_id);
ALTER TABLE public.tool_runs ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.credentials (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    name text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9_.-]{0,62}$'),
    kind text NOT NULL CHECK (kind IN ('telegram_bot','clickup_token','clickup_webhook','github_pat','github_app_key','anthropic_key')),
    ciphertext bytea NOT NULL,
    nonce bytea NOT NULL CHECK (octet_length(nonce) = 12),
    key_version integer NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name)
);
COMMENT ON TABLE public.credentials IS 'AES-256-GCM ciphertext only; AAD binds workspace, name and kind. The master key lives outside the database.';
ALTER TABLE public.credentials ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.app_settings (
    key text PRIMARY KEY,
    value jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE public.app_settings ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE public.app_settings;
DROP TABLE public.credentials;
DROP TABLE public.tool_runs;
DROP TABLE public.outbound_actions;
DROP TABLE public.agent_jobs;
