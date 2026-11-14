-- +goose Up
-- The built-in Phase 2 comment pipeline as durable workflow executions.
--
-- 1. An internal behavior definition per workspace holds the immutable
--    snapshots (versions) of the pipeline settings in force. Its name lives
--    in a reserved namespace no public behavior name can take, and a trigger
--    rejects any change or deletion of it: it cannot be edited, enabled,
--    disabled or triggered through the public behavior paths.
-- 2. An agent job (the logical comment batch of one thread) can be owned by
--    one workflow execution. A workflow-owned job holds no legacy claim
--    token; its current attempt deadline is kept so splitting an attempt
--    into steps never extends it.
-- 3. agent_batch_results holds the validated structured result of the
--    triage and investigation steps of a batch. Workflow checkpoints only
--    reference these rows; they never copy model output.
ALTER TABLE public.behaviors ADD COLUMN internal_kind text
    CHECK (internal_kind IS NULL OR internal_kind IN ('agent_comment_pipeline'));
ALTER TABLE public.behaviors DROP CONSTRAINT behaviors_name_check;
ALTER TABLE public.behaviors ADD CONSTRAINT behaviors_name_check CHECK (
    (internal_kind IS NULL AND name ~ '^[a-z0-9][a-z0-9_-]{0,62}$')
    OR (internal_kind IS NOT NULL AND name = 'internal:' || internal_kind));
CREATE UNIQUE INDEX behaviors_one_internal_kind_idx ON public.behaviors(workspace_id, internal_kind)
    WHERE internal_kind IS NOT NULL;
COMMENT ON COLUMN public.behaviors.internal_kind IS 'Non-NULL for an internal built-in definition; such rows are immutable and hidden from the public behavior paths.';

-- +goose StatementBegin
CREATE FUNCTION public.behaviors_internal_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.internal_kind IS NOT NULL THEN
            RAISE EXCEPTION 'internal behaviors are immutable' USING ERRCODE = 'check_violation';
        END IF;
        RETURN OLD;
    END IF;
    IF OLD.internal_kind IS NOT NULL OR NEW.internal_kind IS NOT NULL THEN
        RAISE EXCEPTION 'internal behaviors are immutable' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER behaviors_internal_no_update BEFORE UPDATE OR DELETE ON public.behaviors
    FOR EACH ROW EXECUTE FUNCTION public.behaviors_internal_immutable();

ALTER TABLE public.agent_jobs ADD CONSTRAINT agent_jobs_workspace_id_id_key UNIQUE (workspace_id, id);
ALTER TABLE public.agent_jobs ADD COLUMN workflow_execution_id bigint;
ALTER TABLE public.agent_jobs ADD COLUMN workflow_attempt_deadline timestamptz;
ALTER TABLE public.agent_jobs ADD CONSTRAINT agent_jobs_workflow_execution_fkey
    FOREIGN KEY (workspace_id, workflow_execution_id) REFERENCES public.workflow_executions(workspace_id, id);
ALTER TABLE public.agent_jobs ADD CONSTRAINT agent_jobs_workflow_claim_check
    CHECK (workflow_execution_id IS NULL OR claim_token IS NULL);
CREATE UNIQUE INDEX agent_jobs_workflow_execution_idx ON public.agent_jobs(workflow_execution_id)
    WHERE workflow_execution_id IS NOT NULL;
COMMENT ON COLUMN public.agent_jobs.workflow_execution_id IS 'The workflow execution that owns this batch; NULL for a legacy-claimed job.';
COMMENT ON COLUMN public.agent_jobs.workflow_attempt_deadline IS 'Deadline of the current attempt of a workflow-owned batch; NULL otherwise.';

CREATE TABLE public.agent_batch_results (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL,
    agent_job_id bigint NOT NULL,
    execution_id bigint NOT NULL,
    stage text NOT NULL CHECK (stage IN ('triage','investigation')),
    outcome text NOT NULL DEFAULT '' CHECK (outcome IN ('','ignore','notify','investigate')),
    through_message_id bigint NOT NULL DEFAULT 0 CHECK (through_message_id >= 0),
    last_triaged_id bigint NOT NULL DEFAULT 0 CHECK (last_triaged_id >= 0),
    max_message_id bigint NOT NULL DEFAULT 0 CHECK (max_message_id >= 0),
    category text NOT NULL DEFAULT '' CHECK (octet_length(category) <= 64),
    urgency text NOT NULL DEFAULT '' CHECK (octet_length(urgency) <= 64),
    confidence double precision NOT NULL DEFAULT 0,
    summary text NOT NULL DEFAULT '' CHECK (octet_length(summary) <= 16384),
    goal text NOT NULL DEFAULT '' CHECK (octet_length(goal) <= 16384),
    router_note text NOT NULL DEFAULT '' CHECK (octet_length(router_note) <= 32768),
    tools_available boolean NOT NULL DEFAULT false,
    unseen_count integer NOT NULL DEFAULT 0 CHECK (unseen_count >= 0),
    report_text text NOT NULL DEFAULT '' CHECK (octet_length(report_text) <= 262144),
    tools_line text NOT NULL DEFAULT '' CHECK (octet_length(tools_line) <= 16384),
    total_input_tokens bigint NOT NULL DEFAULT 0 CHECK (total_input_tokens >= 0),
    total_output_tokens bigint NOT NULL DEFAULT 0 CHECK (total_output_tokens >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, agent_job_id) REFERENCES public.agent_jobs(workspace_id, id),
    FOREIGN KEY (workspace_id, execution_id) REFERENCES public.workflow_executions(workspace_id, id),
    UNIQUE (workspace_id, agent_job_id, stage),
    UNIQUE (workspace_id, id)
);
COMMENT ON TABLE public.agent_batch_results IS 'Validated triage/investigation results of one agent batch, referenced by workflow checkpoints; written once per stage.';
ALTER TABLE public.agent_batch_results ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE public.agent_batch_results;
DROP INDEX public.agent_jobs_workflow_execution_idx;
ALTER TABLE public.agent_jobs DROP CONSTRAINT agent_jobs_workflow_claim_check;
ALTER TABLE public.agent_jobs DROP CONSTRAINT agent_jobs_workflow_execution_fkey;
ALTER TABLE public.agent_jobs DROP COLUMN workflow_attempt_deadline;
ALTER TABLE public.agent_jobs DROP COLUMN workflow_execution_id;
ALTER TABLE public.agent_jobs DROP CONSTRAINT agent_jobs_workspace_id_id_key;
DROP TRIGGER behaviors_internal_no_update ON public.behaviors;
DROP FUNCTION public.behaviors_internal_immutable();
DROP INDEX public.behaviors_one_internal_kind_idx;
ALTER TABLE public.behaviors DROP CONSTRAINT behaviors_name_check;
DELETE FROM public.behavior_versions v USING public.behaviors b
    WHERE b.workspace_id = v.workspace_id AND b.id = v.behavior_id AND b.internal_kind IS NOT NULL;
DELETE FROM public.behaviors WHERE internal_kind IS NOT NULL;
ALTER TABLE public.behaviors ADD CONSTRAINT behaviors_name_check CHECK (name ~ '^[a-z0-9][a-z0-9_-]{0,62}$');
ALTER TABLE public.behaviors DROP COLUMN internal_kind;
