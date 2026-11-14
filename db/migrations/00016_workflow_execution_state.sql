-- +goose Up
-- Durable workflow lifecycle. Generic jobs and signals are added by their
-- owning tasks; these tables contain only trusted identifiers and bounded
-- checkpoint references, never prompts, messages or provider payloads.
CREATE TABLE public.workflow_executions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    behavior_version_id bigint NOT NULL,
    trigger_key text NOT NULL CHECK (btrim(trigger_key) <> '' AND octet_length(trigger_key) <= 512),
    correlation_id uuid NOT NULL,
    state text NOT NULL CHECK (state IN ('RECEIVED','TRIAGING','PLANNING','QUEUED','RUNNING','EVALUATING','WAITING_FOR_USER','WAITING_FOR_APPROVAL','COMPLETED','FAILED','CANCELLED')),
    reason_code text CHECK (reason_code IS NULL OR reason_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    definition jsonb NOT NULL CHECK (jsonb_typeof(definition) = 'object'),
    source_kind text NOT NULL CHECK (btrim(source_kind) <> '' AND octet_length(source_kind) <= 64),
    source_id text NOT NULL CHECK (btrim(source_id) <> '' AND octet_length(source_id) <= 512),
    subject text NOT NULL DEFAULT '' CHECK (octet_length(subject) <= 512),
    revision text NOT NULL DEFAULT '' CHECK (octet_length(revision) <= 512),
    operation_key text NOT NULL CHECK (btrim(operation_key) <> '' AND octet_length(operation_key) <= 512),
    legacy_run_id bigint,
    legacy_job_id bigint,
    parent_execution_id bigint,
    deadline_at timestamptz,
    cancelled_at timestamptz,
    started_at timestamptz,
    finished_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, behavior_version_id) REFERENCES public.behavior_versions(workspace_id, id),
    FOREIGN KEY (workspace_id, parent_execution_id) REFERENCES public.workflow_executions(workspace_id, id),
    UNIQUE (workspace_id, trigger_key),
    UNIQUE (workspace_id, id),
    UNIQUE (parent_execution_id),
    CHECK (parent_execution_id IS NULL OR parent_execution_id <> id),
    CHECK ((state = 'CANCELLED') = (cancelled_at IS NOT NULL)),
    CHECK ((state IN ('COMPLETED','FAILED','CANCELLED')) = (finished_at IS NOT NULL))
);
CREATE INDEX workflow_executions_state_idx ON public.workflow_executions(workspace_id, state, created_at, id);
CREATE INDEX workflow_executions_correlation_idx ON public.workflow_executions(workspace_id, correlation_id, created_at, id);
ALTER TABLE public.workflow_executions ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.execution_steps (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL,
    execution_id bigint NOT NULL,
    step_key text NOT NULL CHECK (btrim(step_key) <> '' AND octet_length(step_key) <= 200),
    position integer NOT NULL CHECK (position >= 0 AND position < 64),
    kind text NOT NULL CHECK (btrim(kind) <> '' AND octet_length(kind) <= 64),
    depends_on text[] NOT NULL DEFAULT '{}',
    state text NOT NULL DEFAULT 'PENDING' CHECK (state IN ('PENDING','RUNNING','WAITING','SUCCEEDED','FAILED','CANCELLED')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    input_tokens bigint NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens bigint NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    cache_read_tokens bigint NOT NULL DEFAULT 0 CHECK (cache_read_tokens >= 0),
    cache_write_tokens bigint NOT NULL DEFAULT 0 CHECK (cache_write_tokens >= 0),
    checkpoint jsonb CHECK (checkpoint IS NULL OR (jsonb_typeof(checkpoint) = 'object' AND pg_column_size(checkpoint) <= 65536)),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, execution_id) REFERENCES public.workflow_executions(workspace_id, id),
    UNIQUE (execution_id, step_key),
    UNIQUE (execution_id, position),
    UNIQUE (workspace_id, execution_id, step_key)
);
CREATE INDEX execution_steps_state_idx ON public.execution_steps(workspace_id, execution_id, state, position);
ALTER TABLE public.execution_steps ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.execution_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL,
    execution_id bigint NOT NULL,
    sequence bigint NOT NULL CHECK (sequence > 0),
    kind text NOT NULL CHECK (kind IN ('started','transition','checkpoint','retry_child')),
    from_state text CHECK (from_state IS NULL OR from_state IN ('RECEIVED','TRIAGING','PLANNING','QUEUED','RUNNING','EVALUATING','WAITING_FOR_USER','WAITING_FOR_APPROVAL','COMPLETED','FAILED','CANCELLED')),
    to_state text CHECK (to_state IS NULL OR to_state IN ('RECEIVED','TRIAGING','PLANNING','QUEUED','RUNNING','EVALUATING','WAITING_FOR_USER','WAITING_FOR_APPROVAL','COMPLETED','FAILED','CANCELLED')),
    step_key text CHECK (step_key IS NULL OR (btrim(step_key) <> '' AND octet_length(step_key) <= 200)),
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    reason_code text CHECK (reason_code IS NULL OR reason_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    idempotency_key text CHECK (idempotency_key IS NULL OR (btrim(idempotency_key) <> '' AND octet_length(idempotency_key) <= 512)),
    input_tokens bigint NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens bigint NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    cache_read_tokens bigint NOT NULL DEFAULT 0 CHECK (cache_read_tokens >= 0),
    cache_write_tokens bigint NOT NULL DEFAULT 0 CHECK (cache_write_tokens >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, execution_id) REFERENCES public.workflow_executions(workspace_id, id),
    FOREIGN KEY (workspace_id, execution_id, step_key) REFERENCES public.execution_steps(workspace_id, execution_id, step_key),
    UNIQUE (execution_id, sequence)
);
CREATE UNIQUE INDEX execution_events_command_idx ON public.execution_events(execution_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
ALTER TABLE public.execution_events ENABLE ROW LEVEL SECURITY;

-- +goose StatementBegin
CREATE FUNCTION public.execution_events_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'execution_events rows are immutable' USING ERRCODE = 'check_violation';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER execution_events_no_update BEFORE UPDATE OR DELETE ON public.execution_events
    FOR EACH ROW EXECUTE FUNCTION public.execution_events_immutable();

-- +goose Down
DROP TRIGGER execution_events_no_update ON public.execution_events;
DROP FUNCTION public.execution_events_immutable();
DROP TABLE public.execution_events;
DROP TABLE public.execution_steps;
DROP TABLE public.workflow_executions;
