-- +goose Up
-- One durable, fenced queue for workflow execution. Rows contain only
-- lifecycle metadata and trusted identifiers; executor payloads stay in their
-- owning domain tables.
CREATE TABLE public.jobs (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL,
    execution_id bigint NOT NULL,
    step_key text NOT NULL CHECK (btrim(step_key) <> '' AND octet_length(step_key) <= 200),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','processing','completed','dead_letter')),
    due_at timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0 AND attempts <= 5),
    max_attempts integer NOT NULL CHECK (max_attempts >= 1 AND max_attempts <= 5),
    claim_token uuid,
    lease_expires_at timestamptz,
    last_error_code text CHECK (last_error_code IS NULL OR last_error_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    dead_letter_reason text CHECK (dead_letter_reason IS NULL OR dead_letter_reason ~ '^[a-z][a-z0-9_]{0,63}$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, execution_id) REFERENCES public.workflow_executions(workspace_id, id),
    FOREIGN KEY (workspace_id, execution_id, step_key) REFERENCES public.execution_steps(workspace_id, execution_id, step_key),
    UNIQUE (workspace_id, id),
    CHECK ((status = 'processing') = (claim_token IS NOT NULL AND lease_expires_at IS NOT NULL)),
    CHECK ((status = 'dead_letter') = (dead_letter_reason IS NOT NULL))
);
CREATE UNIQUE INDEX jobs_one_active_execution_idx ON public.jobs(execution_id)
    WHERE status IN ('pending','processing');
CREATE INDEX jobs_due_idx ON public.jobs(status, due_at, lease_expires_at, id)
    WHERE status IN ('pending','processing');
CREATE INDEX jobs_execution_idx ON public.jobs(workspace_id, execution_id, created_at, id);
ALTER TABLE public.jobs ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE public.jobs;
