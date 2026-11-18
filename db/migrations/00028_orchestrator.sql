-- +goose Up
-- AI orchestrator (Phase 7b). Plans, plan tasks and decisions hold enums,
-- ids, short titles, counters and usage only: never prompts, chat text,
-- model output or untrusted source text.
CREATE TABLE public.orchestrator_plans (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    execution_id bigint NOT NULL,
    version integer NOT NULL CHECK (version >= 1),
    schema_version integer NOT NULL CHECK (schema_version >= 1),
    intent text NOT NULL DEFAULT '' CHECK (intent ~ '^[a-z][a-z0-9_]{0,63}$' OR intent = ''),
    task_type text NOT NULL DEFAULT '' CHECK (task_type ~ '^[a-z][a-z0-9_]{0,63}$' OR task_type = ''),
    priority text NOT NULL DEFAULT 'normal' CHECK (priority IN ('urgent', 'high', 'normal', 'low')),
    complexity text NOT NULL DEFAULT 'm' CHECK (complexity IN ('xs', 's', 'm', 'l', 'xl')),
    confidence double precision NOT NULL DEFAULT 0 CHECK (confidence >= 0 AND confidence <= 1),
    budget jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(budget) = 'object' AND octet_length(budget::text) <= 1024),
    status text NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'SUPERSEDED', 'COMPLETED', 'FAILED', 'CANCELLED')),
    tokens_in bigint NOT NULL DEFAULT 0 CHECK (tokens_in >= 0),
    tokens_out bigint NOT NULL DEFAULT 0 CHECK (tokens_out >= 0),
    cost_usd double precision NOT NULL DEFAULT 0 CHECK (cost_usd >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, id),
    UNIQUE (execution_id, version),
    FOREIGN KEY (workspace_id, execution_id) REFERENCES public.workflow_executions(workspace_id, id)
);
CREATE INDEX orchestrator_plans_execution_idx ON public.orchestrator_plans (workspace_id, execution_id, version DESC);
COMMENT ON TABLE public.orchestrator_plans IS 'Versioned orchestrator plans; immutable after insert except status.';
ALTER TABLE public.orchestrator_plans ENABLE ROW LEVEL SECURITY;

-- Plans are immutable except for their status.
-- +goose StatementBegin
CREATE FUNCTION public.orchestrator_plans_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.workspace_id IS DISTINCT FROM OLD.workspace_id
        OR NEW.execution_id IS DISTINCT FROM OLD.execution_id
        OR NEW.version IS DISTINCT FROM OLD.version
        OR NEW.schema_version IS DISTINCT FROM OLD.schema_version
        OR NEW.intent IS DISTINCT FROM OLD.intent
        OR NEW.task_type IS DISTINCT FROM OLD.task_type
        OR NEW.priority IS DISTINCT FROM OLD.priority
        OR NEW.complexity IS DISTINCT FROM OLD.complexity
        OR NEW.confidence IS DISTINCT FROM OLD.confidence
        OR NEW.budget IS DISTINCT FROM OLD.budget
        OR NEW.tokens_in IS DISTINCT FROM OLD.tokens_in
        OR NEW.tokens_out IS DISTINCT FROM OLD.tokens_out
        OR NEW.cost_usd IS DISTINCT FROM OLD.cost_usd
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'orchestrator_plans is immutable except status';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER orchestrator_plans_immutable BEFORE UPDATE ON public.orchestrator_plans
    FOR EACH ROW EXECUTE FUNCTION public.orchestrator_plans_immutable();

CREATE TABLE public.orchestrator_plan_tasks (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    plan_id bigint NOT NULL,
    key text NOT NULL CHECK (key ~ '^[a-z][a-z0-9_-]{0,31}$'),
    title text NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200),
    capabilities text[] NOT NULL DEFAULT '{}' CHECK (cardinality(capabilities) <= 16),
    depends_on text[] NOT NULL DEFAULT '{}' CHECK (cardinality(depends_on) <= 32),
    priority text NOT NULL DEFAULT 'normal' CHECK (priority IN ('urgent', 'high', 'normal', 'low')),
    complexity text NOT NULL DEFAULT 'm' CHECK (complexity IN ('xs', 's', 'm', 'l', 'xl')),
    executor text NOT NULL DEFAULT '' CHECK (executor ~ '^[a-z][a-z0-9_.-]{0,63}$' OR executor = ''),
    state text NOT NULL DEFAULT 'PENDING' CHECK (state IN ('PENDING', 'READY', 'RUNNING', 'WAITING', 'SUCCEEDED', 'FAILED', 'BLOCKED', 'SKIPPED')),
    result_step_id text NOT NULL DEFAULT '' CHECK (octet_length(result_step_id) <= 128),
    result jsonb CHECK (result IS NULL OR (jsonb_typeof(result) = 'object' AND octet_length(result::text) <= 4096)),
    evidence_refs jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(evidence_refs) = 'array' AND octet_length(evidence_refs::text) <= 4096),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (plan_id, key),
    FOREIGN KEY (workspace_id, plan_id) REFERENCES public.orchestrator_plans(workspace_id, id)
);
COMMENT ON TABLE public.orchestrator_plan_tasks IS 'Tasks of an orchestrator plan: sanitized titles, fixed states, bounded evidence references.';
ALTER TABLE public.orchestrator_plan_tasks ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.orchestrator_decisions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    execution_id bigint NOT NULL,
    plan_version integer NOT NULL CHECK (plan_version >= 1),
    task_key text CHECK (task_key IS NULL OR task_key ~ '^[a-z][a-z0-9_-]{0,31}$'),
    decision text NOT NULL CHECK (decision IN ('EXECUTE', 'DELEGATE', 'REQUEST_CLARIFICATION', 'REQUEST_APPROVAL', 'REPLAN', 'COMPLETE', 'FAIL')),
    reason_code text NOT NULL DEFAULT '' CHECK (reason_code = '' OR reason_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    converted_from text CHECK (converted_from IS NULL OR converted_from IN ('EXECUTE', 'DELEGATE', 'REQUEST_CLARIFICATION', 'REQUEST_APPROVAL', 'REPLAN', 'COMPLETE', 'FAIL')),
    evidence_refs jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(evidence_refs) = 'array' AND octet_length(evidence_refs::text) <= 4096),
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, execution_id) REFERENCES public.workflow_executions(workspace_id, id)
);
CREATE INDEX orchestrator_decisions_execution_idx ON public.orchestrator_decisions (workspace_id, execution_id, id);
COMMENT ON TABLE public.orchestrator_decisions IS 'Audit of every orchestrator decision, including Go conversions; enums, codes and evidence references only.';
ALTER TABLE public.orchestrator_decisions ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE public.orchestrator_decisions;
DROP TABLE public.orchestrator_plan_tasks;
DROP TRIGGER orchestrator_plans_immutable ON public.orchestrator_plans;
DROP FUNCTION public.orchestrator_plans_immutable();
DROP TABLE public.orchestrator_plans;
