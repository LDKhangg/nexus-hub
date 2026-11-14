-- +goose Up
-- The per-workspace runtime-mode fence of the workflow cutover.
--
-- A workspace without a row (or with mode 'legacy') is scheduled by the
-- legacy behavior run and agent job claimers; the durable workflow starters
-- create no execution for it. A workspace in mode 'workflow' was cut over:
-- the generic workflow job queue is its only agent/behavior scheduler and
-- the legacy claimers take nothing of it. There is no way back to 'legacy'
-- in this phase.
--
-- Every claim or start checks the mode in its own transaction after taking
-- a KEY SHARE lock on the workspace row; the cutover holds FOR UPDATE on
-- that row while it imports and sets the mode, so no claim or start can
-- interleave with it.
CREATE TABLE public.workflow_runtime_modes (
    workspace_id uuid PRIMARY KEY REFERENCES public.workspaces(id),
    mode text NOT NULL CHECK (mode IN ('legacy','workflow')),
    cutover_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((mode = 'workflow') = (cutover_at IS NOT NULL))
);
COMMENT ON TABLE public.workflow_runtime_modes IS 'Runtime-mode fence per workspace: legacy claimers or the durable workflow queue schedule its agent and behavior work, never both.';
ALTER TABLE public.workflow_runtime_modes ENABLE ROW LEVEL SECURITY;

-- Lookups of the execution of a legacy run or agent job (cutover, starters,
-- legacy claim exclusion and projection repair).
CREATE INDEX workflow_executions_legacy_run_idx ON public.workflow_executions(workspace_id, legacy_run_id)
    WHERE legacy_run_id IS NOT NULL;
CREATE INDEX workflow_executions_legacy_job_idx ON public.workflow_executions(workspace_id, legacy_job_id)
    WHERE legacy_job_id IS NOT NULL;

-- +goose Down
DROP INDEX public.workflow_executions_legacy_job_idx;
DROP INDEX public.workflow_executions_legacy_run_idx;
DROP TABLE public.workflow_runtime_modes;
