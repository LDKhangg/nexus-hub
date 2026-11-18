-- +goose Up
-- Orchestrated tasks (Phase 7b P4): the built-in workflow orchestrated_task
-- runs a plan of tasks as durable steps. Its owner decisions (budget
-- extension "Thêm 1 vòng" / "Huỷ", and plan approvals "Làm" / "Huỷ") are
-- approvals of kind orchestrator_gate bound to the waiting step. The route
-- row remembers where the execution's messages go and the explicit repo,
-- if the owner named one; ids only, never the request text.
ALTER TABLE public.approvals DROP CONSTRAINT approvals_kind_check;
ALTER TABLE public.approvals ADD CONSTRAINT approvals_kind_check
    CHECK (kind IN ('item_answer','post_draft','confirm_action','feedback','feedback_reason','contact_alias','assistant_action','workflow_target','worker_job','orchestrator_gate'));
ALTER TABLE public.behaviors DROP CONSTRAINT behaviors_internal_kind_check;
ALTER TABLE public.behaviors ADD CONSTRAINT behaviors_internal_kind_check
    CHECK (internal_kind IS NULL OR internal_kind IN ('agent_comment_pipeline','local_task','orchestrated_task'));

CREATE TABLE public.orchestrator_routes (
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    execution_id bigint NOT NULL,
    integration_id uuid NOT NULL,
    chat_id bigint NOT NULL,
    thread_id bigint NOT NULL DEFAULT 0,
    repo text NOT NULL DEFAULT '' CHECK (repo = '' OR octet_length(repo) BETWEEN 3 AND 200),
    task_ref text NOT NULL DEFAULT '' CHECK (octet_length(task_ref) <= 64),
    clickup_task_id text NOT NULL DEFAULT '' CHECK (octet_length(clickup_task_id) <= 64),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, execution_id),
    FOREIGN KEY (workspace_id, execution_id) REFERENCES public.workflow_executions(workspace_id, id)
);
COMMENT ON TABLE public.orchestrator_routes IS 'Where the messages of an orchestrated task go, and its explicit repo; ids only.';
ALTER TABLE public.orchestrator_routes ENABLE ROW LEVEL SECURITY;

-- +goose Down
-- Rollback deletes every orchestrated_task execution, its owner gates and
-- its internal behavior definition (their history is lost): rows of the
-- new approval kind and internal kind cannot satisfy the old CHECKs, and
-- the gates are referenced by workflow waits and signals.
DROP TABLE public.orchestrator_routes;
CREATE TEMP TABLE down_00029_executions ON COMMIT DROP AS
WITH RECURSIVE e AS (
    SELECT x.workspace_id, x.id FROM public.workflow_executions x
    JOIN public.behavior_versions v ON v.workspace_id = x.workspace_id AND v.id = x.behavior_version_id
    JOIN public.behaviors b ON b.workspace_id = v.workspace_id AND b.id = v.behavior_id
    WHERE b.internal_kind = 'orchestrated_task'
    UNION
    SELECT c.workspace_id, c.id FROM public.workflow_executions c
    JOIN e ON c.workspace_id = e.workspace_id AND c.parent_execution_id = e.id
)
SELECT workspace_id, id FROM e;
DELETE FROM public.workflow_signals s
    WHERE EXISTS (SELECT 1 FROM public.approvals a WHERE a.workspace_id = s.workspace_id AND a.id = s.reference_id AND a.kind = 'orchestrator_gate')
       OR EXISTS (SELECT 1 FROM down_00029_executions d WHERE d.workspace_id = s.workspace_id AND d.id = s.execution_id);
DELETE FROM public.workflow_waits w
    WHERE EXISTS (SELECT 1 FROM public.approvals a WHERE a.workspace_id = w.workspace_id AND a.id = w.approval_id AND a.kind = 'orchestrator_gate')
       OR EXISTS (SELECT 1 FROM down_00029_executions d WHERE d.workspace_id = w.workspace_id AND d.id = w.execution_id);
DELETE FROM public.approvals WHERE kind = 'orchestrator_gate';
DELETE FROM public.orchestrator_decisions t USING down_00029_executions d WHERE t.workspace_id = d.workspace_id AND t.execution_id = d.id;
DELETE FROM public.orchestrator_plan_tasks t USING public.orchestrator_plans p, down_00029_executions d
    WHERE t.workspace_id = p.workspace_id AND t.plan_id = p.id AND p.workspace_id = d.workspace_id AND p.execution_id = d.id;
DELETE FROM public.orchestrator_plans t USING down_00029_executions d WHERE t.workspace_id = d.workspace_id AND t.execution_id = d.id;
DELETE FROM public.worker_events ev USING public.worker_jobs j, down_00029_executions d
    WHERE ev.workspace_id = j.workspace_id AND ev.job_id = j.id AND j.workspace_id = d.workspace_id AND j.execution_id = d.id;
DELETE FROM public.worker_jobs t USING down_00029_executions d WHERE t.workspace_id = d.workspace_id AND t.execution_id = d.id;
DELETE FROM public.worker_task_routes t USING down_00029_executions d WHERE t.workspace_id = d.workspace_id AND t.execution_id = d.id;
DELETE FROM public.workflow_instructions t USING down_00029_executions d WHERE t.workspace_id = d.workspace_id AND t.execution_id = d.id;
DELETE FROM public.resource_bindings t USING down_00029_executions d WHERE t.workspace_id = d.workspace_id AND t.execution_id = d.id;
DELETE FROM public.conversation_workflows t USING down_00029_executions d WHERE t.workspace_id = d.workspace_id AND t.execution_id = d.id;
UPDATE public.conversation_messages t SET execution_id = NULL FROM down_00029_executions d WHERE t.workspace_id = d.workspace_id AND t.execution_id = d.id;
DELETE FROM public.jobs t USING down_00029_executions d WHERE t.workspace_id = d.workspace_id AND t.execution_id = d.id;
ALTER TABLE public.execution_events DISABLE TRIGGER execution_events_no_update;
DELETE FROM public.execution_events t USING down_00029_executions d WHERE t.workspace_id = d.workspace_id AND t.execution_id = d.id;
ALTER TABLE public.execution_events ENABLE TRIGGER execution_events_no_update;
DELETE FROM public.execution_steps t USING down_00029_executions d WHERE t.workspace_id = d.workspace_id AND t.execution_id = d.id;
UPDATE public.workflow_executions t SET parent_execution_id = NULL FROM down_00029_executions d
    WHERE t.workspace_id = d.workspace_id AND t.id = d.id AND t.parent_execution_id IS NOT NULL;
DELETE FROM public.workflow_executions t USING down_00029_executions d WHERE t.workspace_id = d.workspace_id AND t.id = d.id;
-- Internal definitions have no behavior_runs (they never run through the
-- public behavior paths); the immutability trigger is lifted only for this
-- delete.
DELETE FROM public.behavior_versions v USING public.behaviors b
    WHERE b.workspace_id = v.workspace_id AND b.id = v.behavior_id AND b.internal_kind = 'orchestrated_task';
ALTER TABLE public.behaviors DISABLE TRIGGER behaviors_internal_no_update;
DELETE FROM public.behaviors WHERE internal_kind = 'orchestrated_task';
ALTER TABLE public.behaviors ENABLE TRIGGER behaviors_internal_no_update;
ALTER TABLE public.approvals DROP CONSTRAINT approvals_kind_check;
ALTER TABLE public.approvals ADD CONSTRAINT approvals_kind_check
    CHECK (kind IN ('item_answer','post_draft','confirm_action','feedback','feedback_reason','contact_alias','assistant_action','workflow_target','worker_job'));
ALTER TABLE public.behaviors DROP CONSTRAINT behaviors_internal_kind_check;
ALTER TABLE public.behaviors ADD CONSTRAINT behaviors_internal_kind_check
    CHECK (internal_kind IS NULL OR internal_kind IN ('agent_comment_pipeline','local_task'));
DROP TABLE down_00029_executions;
