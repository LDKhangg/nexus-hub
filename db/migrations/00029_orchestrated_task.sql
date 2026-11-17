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
DROP TABLE public.orchestrator_routes;
DELETE FROM public.approvals WHERE kind = 'orchestrator_gate';
ALTER TABLE public.approvals DROP CONSTRAINT approvals_kind_check;
ALTER TABLE public.approvals ADD CONSTRAINT approvals_kind_check
    CHECK (kind IN ('item_answer','post_draft','confirm_action','feedback','feedback_reason','contact_alias','assistant_action','workflow_target','worker_job'));
ALTER TABLE public.behaviors DROP CONSTRAINT behaviors_internal_kind_check;
ALTER TABLE public.behaviors ADD CONSTRAINT behaviors_internal_kind_check
    CHECK (internal_kind IS NULL OR internal_kind IN ('agent_comment_pipeline','local_task'));
