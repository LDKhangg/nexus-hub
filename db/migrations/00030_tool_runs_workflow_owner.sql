-- +goose Up
-- Durable audit for orchestrator investigate tasks (Phase 7b P4c): a tool
-- run can belong to a workflow execution, next to an agent job or a behavior
-- run. Exactly one owner per row.
ALTER TABLE public.tool_runs ADD COLUMN workflow_execution_id bigint;
ALTER TABLE public.tool_runs ADD CONSTRAINT tool_runs_workflow_execution_fkey
    FOREIGN KEY (workspace_id, workflow_execution_id) REFERENCES public.workflow_executions(workspace_id, id);
ALTER TABLE public.tool_runs DROP CONSTRAINT tool_runs_one_owner;
ALTER TABLE public.tool_runs ADD CONSTRAINT tool_runs_one_owner
    CHECK (num_nonnulls(agent_job_id, behavior_run_id, workflow_execution_id) = 1);
CREATE INDEX tool_runs_workflow_execution_idx ON public.tool_runs(workflow_execution_id)
    WHERE workflow_execution_id IS NOT NULL;

-- +goose Down
-- Rows owned only by a workflow execution cannot satisfy the old CHECK, so
-- they are deleted first (their audit trail is lost on rollback).
DROP INDEX public.tool_runs_workflow_execution_idx;
DELETE FROM public.tool_runs WHERE workflow_execution_id IS NOT NULL;
ALTER TABLE public.tool_runs DROP CONSTRAINT tool_runs_one_owner;
ALTER TABLE public.tool_runs ADD CONSTRAINT tool_runs_one_owner
    CHECK (num_nonnulls(agent_job_id, behavior_run_id) = 1);
ALTER TABLE public.tool_runs DROP CONSTRAINT tool_runs_workflow_execution_fkey;
ALTER TABLE public.tool_runs DROP COLUMN workflow_execution_id;
