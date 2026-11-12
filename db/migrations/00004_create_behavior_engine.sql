-- +goose Up
-- Phase 3 behavior engine: versioned behavior definitions, pinned runs and
-- their items, approvals, Telegram inbound updates, the user profile and
-- decision memory. Every table is workspace scoped; child rows reference
-- their parent through (workspace_id, id) so a row can never cross
-- workspaces. Message, prompt and context text never lands here except the
-- item fields the trace must show and the owner's own answers.

CREATE TABLE public.behaviors (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    name text NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9_-]{0,62}$'),
    status text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','enabled','disabled')),
    draft jsonb NOT NULL CHECK (jsonb_typeof(draft) = 'object'),
    draft_vars jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(draft_vars) = 'object'),
    draft_source text NOT NULL DEFAULT '' CHECK (octet_length(draft_source) <= 262144),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name),
    UNIQUE (workspace_id, id)
);
COMMENT ON COLUMN public.behaviors.draft_source IS 'The imported YAML/JSON text of the draft, kept so export returns the same text.';
ALTER TABLE public.behaviors ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.behavior_versions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL,
    behavior_id bigint NOT NULL,
    version integer NOT NULL CHECK (version >= 1),
    document jsonb NOT NULL CHECK (jsonb_typeof(document) = 'object'),
    vars jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(vars) = 'object'),
    source text NOT NULL DEFAULT '' CHECK (octet_length(source) <= 262144),
    published_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, behavior_id) REFERENCES public.behaviors(workspace_id, id),
    UNIQUE (behavior_id, version),
    UNIQUE (workspace_id, id)
);
COMMENT ON TABLE public.behavior_versions IS 'Published behavior versions are immutable; a trigger rejects UPDATE.';
ALTER TABLE public.behavior_versions ENABLE ROW LEVEL SECURITY;

-- +goose StatementBegin
CREATE FUNCTION public.behavior_versions_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'behavior_versions rows are immutable' USING ERRCODE = 'check_violation';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER behavior_versions_no_update BEFORE UPDATE ON public.behavior_versions
    FOR EACH ROW EXECUTE FUNCTION public.behavior_versions_immutable();

CREATE TABLE public.behavior_runs (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL,
    behavior_id bigint NOT NULL,
    behavior_version_id bigint NOT NULL,
    version integer NOT NULL CHECK (version >= 1),
    trigger_kind text NOT NULL CHECK (btrim(trigger_kind) <> ''),
    trigger jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(trigger) = 'object'),
    subject text NOT NULL CHECK (btrim(subject) <> '' AND octet_length(subject) <= 512),
    revision text NOT NULL CHECK (btrim(revision) <> '' AND octet_length(revision) <= 512),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','running','done','failed','canceled')),
    correlation_id uuid NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    run_at timestamptz NOT NULL DEFAULT now(),
    locked_until timestamptz,
    claim_token uuid,
    input_tokens integer NOT NULL DEFAULT 0,
    output_tokens integer NOT NULL DEFAULT 0,
    cache_read_tokens integer NOT NULL DEFAULT 0,
    cache_write_tokens integer NOT NULL DEFAULT 0,
    steps integer NOT NULL DEFAULT 0 CHECK (steps >= 0),
    error_code text,
    snapshot jsonb CHECK (snapshot IS NULL OR jsonb_typeof(snapshot) = 'object'),
    undone_at timestamptz,
    started_at timestamptz,
    finished_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, behavior_id) REFERENCES public.behaviors(workspace_id, id),
    FOREIGN KEY (workspace_id, behavior_version_id) REFERENCES public.behavior_versions(workspace_id, id),
    UNIQUE (behavior_version_id, subject, revision),
    UNIQUE (workspace_id, id)
);
CREATE INDEX behavior_runs_due_idx ON public.behavior_runs(status, run_at, locked_until);
CREATE INDEX behavior_runs_subject_idx ON public.behavior_runs(workspace_id, subject, created_at DESC);
COMMENT ON COLUMN public.behavior_runs.trigger IS 'Trusted event identifiers only (IDs, status names); never message or context text.';
COMMENT ON COLUMN public.behavior_runs.snapshot IS 'State saved before the first write of the run, used by undo.';
ALTER TABLE public.behavior_runs ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.run_items (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL,
    run_id bigint NOT NULL,
    item_key text NOT NULL CHECK (btrim(item_key) <> '' AND octet_length(item_key) <= 200),
    label text NOT NULL DEFAULT '',
    text text NOT NULL,
    proposal text NOT NULL DEFAULT '',
    citations jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(citations) = 'array'),
    topic text NOT NULL DEFAULT '',
    confidence double precision NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    informational boolean NOT NULL DEFAULT false,
    flags text[] NOT NULL DEFAULT '{}',
    class text NOT NULL CHECK (class IN ('auto','notify','ask')),
    rule text NOT NULL CHECK (btrim(rule) <> ''),
    status text NOT NULL DEFAULT 'drafted' CHECK (status IN ('drafted','done','asked','answered','expired','skipped','conflict','failed')),
    action_result jsonb CHECK (action_result IS NULL OR jsonb_typeof(action_result) = 'object'),
    rating text CHECK (rating IN ('up','down')),
    rating_reason text CHECK (rating_reason IS NULL OR char_length(rating_reason) <= 1000),
    rated_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, run_id) REFERENCES public.behavior_runs(workspace_id, id),
    UNIQUE (run_id, item_key),
    UNIQUE (workspace_id, id)
);
COMMENT ON COLUMN public.run_items.rule IS 'The engine rule that decided the class.';
ALTER TABLE public.run_items ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.approvals (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    kind text NOT NULL CHECK (kind IN ('item_answer','post_draft','confirm_action')),
    run_id bigint,
    item_id bigint,
    action text NOT NULL DEFAULT '',
    resource text NOT NULL DEFAULT '',
    payload jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(payload) = 'object'),
    options jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(options) = 'array'),
    nonce text NOT NULL UNIQUE CHECK (nonce ~ '^[A-Za-z0-9_-]{16,64}$'),
    expires_at timestamptz NOT NULL,
    decision text CHECK (decision IN ('approved','rejected','answered','expired')),
    answer text CHECK (answer IS NULL OR char_length(answer) <= 4000),
    decided_by text,
    decided_at timestamptz,
    telegram_message_id bigint,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, run_id) REFERENCES public.behavior_runs(workspace_id, id),
    FOREIGN KEY (workspace_id, item_id) REFERENCES public.run_items(workspace_id, id),
    CHECK ((decision IS NULL) = (decided_at IS NULL)),
    CHECK (item_id IS NULL OR run_id IS NOT NULL)
);
CREATE UNIQUE INDEX approvals_one_open_per_item_idx ON public.approvals(item_id, kind)
    WHERE decision IS NULL AND item_id IS NOT NULL;
CREATE INDEX approvals_open_expiry_idx ON public.approvals(expires_at) WHERE decision IS NULL;
CREATE INDEX approvals_run_idx ON public.approvals(run_id);
COMMENT ON TABLE public.approvals IS 'A decision binds run, item, action and resource; it is taken once, only while open and unexpired.';
ALTER TABLE public.approvals ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.telegram_updates (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    update_id bigint NOT NULL,
    chat_id bigint NOT NULL,
    kind text NOT NULL CHECK (kind IN ('command','callback','text','other')),
    command text NOT NULL DEFAULT '' CHECK (char_length(command) <= 64),
    args text NOT NULL DEFAULT '' CHECK (char_length(args) <= 512),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    locked_until timestamptz,
    claim_token uuid,
    received_at timestamptz NOT NULL DEFAULT now(),
    processed_at timestamptz,
    error_code text,
    UNIQUE (workspace_id, update_id)
);
CREATE INDEX telegram_updates_pending_idx ON public.telegram_updates(id) WHERE processed_at IS NULL;
COMMENT ON TABLE public.telegram_updates IS 'Inbound update dedupe and work queue; holds a short command and its arguments, never full message text.';
ALTER TABLE public.telegram_updates ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.user_profiles (
    workspace_id uuid PRIMARY KEY REFERENCES public.workspaces(id),
    language text NOT NULL DEFAULT 'vi' CHECK (language ~ '^[a-z]{2}(-[A-Z]{2})?$'),
    tone text NOT NULL DEFAULT '' CHECK (char_length(tone) <= 200),
    quiet_start_minute integer CHECK (quiet_start_minute BETWEEN 0 AND 1439),
    quiet_end_minute integer CHECK (quiet_end_minute BETWEEN 0 AND 1439),
    timezone text NOT NULL DEFAULT 'Asia/Ho_Chi_Minh' CHECK (btrim(timezone) <> '' AND char_length(timezone) <= 64),
    digest_mode text NOT NULL DEFAULT 'per_run' CHECK (digest_mode IN ('per_run','hourly','daily')),
    default_model text NOT NULL DEFAULT '' CHECK (default_model = '' OR default_model ~ '^[a-z0-9][a-z0-9.-]{0,99}$'),
    budgets jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(budgets) = 'object'),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((quiet_start_minute IS NULL) = (quiet_end_minute IS NULL))
);
COMMENT ON COLUMN public.user_profiles.default_model IS 'Empty means the settings default model.';
ALTER TABLE public.user_profiles ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.decision_memory (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    topic text NOT NULL DEFAULT '' CHECK (char_length(topic) <= 200),
    question text NOT NULL CHECK (btrim(question) <> '' AND char_length(question) <= 4000),
    answer text NOT NULL CHECK (btrim(answer) <> '' AND char_length(answer) <= 4000),
    scope jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(scope) = 'object'),
    source text NOT NULL CHECK (source IN ('ask_answer','auto_posted','auto_undone')),
    run_id bigint,
    item_id bigint,
    created_at timestamptz NOT NULL DEFAULT now(),
    forgotten_at timestamptz,
    FOREIGN KEY (workspace_id, run_id) REFERENCES public.behavior_runs(workspace_id, id),
    FOREIGN KEY (workspace_id, item_id) REFERENCES public.run_items(workspace_id, id)
);
CREATE INDEX decision_memory_search_idx ON public.decision_memory
    USING gin (to_tsvector('simple'::regconfig, topic || ' ' || question || ' ' || answer))
    WHERE forgotten_at IS NULL;
CREATE INDEX decision_memory_workspace_idx ON public.decision_memory(workspace_id, created_at DESC);
ALTER TABLE public.decision_memory ENABLE ROW LEVEL SECURITY;

-- The Telegram webhook secret is an encrypted credential of its own kind.
ALTER TABLE public.credentials DROP CONSTRAINT credentials_kind_check;
ALTER TABLE public.credentials ADD CONSTRAINT credentials_kind_check
    CHECK (kind IN ('telegram_bot','clickup_token','clickup_webhook','github_pat','github_app_key','anthropic_key','telegram_webhook'));

-- +goose Down
DELETE FROM public.credentials WHERE kind = 'telegram_webhook';
ALTER TABLE public.credentials DROP CONSTRAINT credentials_kind_check;
ALTER TABLE public.credentials ADD CONSTRAINT credentials_kind_check
    CHECK (kind IN ('telegram_bot','clickup_token','clickup_webhook','github_pat','github_app_key','anthropic_key'));
DROP TABLE public.decision_memory;
DROP TABLE public.user_profiles;
DROP TABLE public.telegram_updates;
DROP TABLE public.approvals;
DROP TABLE public.run_items;
DROP TABLE public.behavior_runs;
DROP TRIGGER behavior_versions_no_update ON public.behavior_versions;
DROP FUNCTION public.behavior_versions_immutable();
DROP TABLE public.behavior_versions;
DROP TABLE public.behaviors;
