-- +goose Up
-- Durable approval and user-input waits. A wait binds one blocking approval
-- to the execution step that created it, in the same transaction as the
-- approval. A signal records that the bound approval was decided (or
-- expired), in the same transaction as the decision. Rows hold identifiers,
-- decision codes and markers only; the decision itself (and any free-text
-- answer) stays on the approval row.
ALTER TABLE public.approvals ADD CONSTRAINT approvals_workspace_id_id_key UNIQUE (workspace_id, id);

CREATE TABLE public.workflow_waits (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL,
    execution_id bigint NOT NULL,
    step_key text NOT NULL CHECK (btrim(step_key) <> '' AND octet_length(step_key) <= 200),
    approval_id bigint NOT NULL,
    wait_kind text NOT NULL CHECK (wait_kind IN ('approval','input')),
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, execution_id) REFERENCES public.workflow_executions(workspace_id, id),
    FOREIGN KEY (workspace_id, execution_id, step_key) REFERENCES public.execution_steps(workspace_id, execution_id, step_key),
    FOREIGN KEY (workspace_id, approval_id) REFERENCES public.approvals(workspace_id, id),
    UNIQUE (workspace_id, approval_id),
    UNIQUE (workspace_id, id)
);
CREATE INDEX workflow_waits_step_idx ON public.workflow_waits(workspace_id, execution_id, step_key, id);
COMMENT ON TABLE public.workflow_waits IS 'A blocking approval or user-input wait of one execution step; only bound approvals change execution state.';
ALTER TABLE public.workflow_waits ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.workflow_signals (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL,
    signal_id text NOT NULL CHECK (btrim(signal_id) <> '' AND octet_length(signal_id) <= 200),
    execution_id bigint NOT NULL,
    step_key text NOT NULL CHECK (btrim(step_key) <> '' AND octet_length(step_key) <= 200),
    wait_id bigint NOT NULL,
    reference_kind text NOT NULL CHECK (reference_kind IN ('approval')),
    reference_id bigint NOT NULL,
    decision text NOT NULL CHECK (decision IN ('approved','rejected','answered','expired')),
    consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, execution_id, step_key) REFERENCES public.execution_steps(workspace_id, execution_id, step_key),
    FOREIGN KEY (workspace_id, wait_id) REFERENCES public.workflow_waits(workspace_id, id),
    FOREIGN KEY (workspace_id, reference_id) REFERENCES public.approvals(workspace_id, id),
    UNIQUE (workspace_id, signal_id),
    UNIQUE (workspace_id, wait_id)
);
CREATE INDEX workflow_signals_pending_idx ON public.workflow_signals(workspace_id, execution_id, step_key)
    WHERE consumed_at IS NULL;
COMMENT ON TABLE public.workflow_signals IS 'One authorized decision per wait; consumed_at is set when the fenced continuation applied it.';
ALTER TABLE public.workflow_signals ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE public.workflow_signals;
DROP TABLE public.workflow_waits;
ALTER TABLE public.approvals DROP CONSTRAINT approvals_workspace_id_id_key;
