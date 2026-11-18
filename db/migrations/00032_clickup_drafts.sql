-- +goose Up
-- Phase 10 W1: ClickUp drafts. Every ClickUp comment the owner did not write
-- in ClickUp is first a draft, shown in Telegram as an exact preview with
-- "Đăng" / "Sửa" / "Huỷ"; only "Đăng" posts it, once (idempotency key
-- draft:<id>). The body is kept only while it may still be posted: posted,
-- cancelled and expired drafts keep metadata only (ids, state, times, hash).
CREATE TABLE public.clickup_drafts (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    integration_id uuid NOT NULL,
    chat_id bigint NOT NULL CHECK (chat_id <> 0),
    thread_id bigint NOT NULL DEFAULT 0 CHECK (thread_id >= 0),
    task_id text NOT NULL CHECK (task_id ~ '^[A-Za-z0-9_-]{1,64}$'),
    parent_comment_id text NOT NULL DEFAULT '' CHECK (parent_comment_id = '' OR parent_comment_id ~ '^[0-9]{1,20}$'),
    mention_user_ids text[] NOT NULL DEFAULT '{}' CHECK (cardinality(mention_user_ids) <= 5),
    new_status text NOT NULL DEFAULT '' CHECK (char_length(new_status) <= 100),
    body text CHECK (body IS NULL OR char_length(body) BETWEEN 1 AND 8000),
    body_sha256 text NOT NULL CHECK (body_sha256 ~ '^[0-9a-f]{64}$'),
    state text NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'posting', 'posted', 'cancelled', 'expired', 'failed')),
    source text NOT NULL CHECK (source IN ('assistant', 'execution_report', 'orchestrator')),
    execution_id bigint,
    plan_task_key text NOT NULL DEFAULT '' CHECK (octet_length(plan_task_key) <= 64),
    preview_message_id bigint NOT NULL DEFAULT 0 CHECK (preview_message_id >= 0),
    posted_comment_id text NOT NULL DEFAULT '' CHECK (octet_length(posted_comment_id) <= 64),
    reason_code text NOT NULL DEFAULT '' CHECK (reason_code ~ '^[a-z0-9_]{0,64}$'),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    -- The body exists exactly while the draft may still be posted.
    CHECK ((state IN ('posted', 'cancelled', 'expired')) = (body IS NULL)),
    CHECK ((state IN ('posted', 'cancelled', 'expired')) = (finished_at IS NOT NULL)),
    FOREIGN KEY (workspace_id, integration_id) REFERENCES public.integrations(workspace_id, id),
    FOREIGN KEY (workspace_id, execution_id) REFERENCES public.workflow_executions(workspace_id, id),
    UNIQUE (workspace_id, id)
);
CREATE INDEX clickup_drafts_open_idx ON public.clickup_drafts (expires_at)
    WHERE state IN ('pending', 'failed');
CREATE INDEX clickup_drafts_posting_idx ON public.clickup_drafts (updated_at)
    WHERE state = 'posting';
COMMENT ON TABLE public.clickup_drafts IS 'Workspace-scoped ClickUp comment drafts awaiting the owner''s confirm; the body is cleared when posted, cancelled or expired.';
ALTER TABLE public.clickup_drafts ENABLE ROW LEVEL SECURITY;

-- A draft's buttons are approvals of kind clickup_draft (nonce, message
-- binding, free-text reply for "Sửa").
ALTER TABLE public.approvals DROP CONSTRAINT approvals_kind_check;
ALTER TABLE public.approvals ADD CONSTRAINT approvals_kind_check
    CHECK (kind IN ('item_answer','post_draft','confirm_action','feedback','feedback_reason','contact_alias','assistant_action','workflow_target','worker_job','orchestrator_gate','clickup_draft'));

-- A tool run can belong to a draft (its confirmed post), next to an agent
-- job, a behavior run or a workflow execution. Exactly one owner per row.
ALTER TABLE public.tool_runs ADD COLUMN clickup_draft_id bigint;
ALTER TABLE public.tool_runs ADD CONSTRAINT tool_runs_clickup_draft_fkey
    FOREIGN KEY (workspace_id, clickup_draft_id) REFERENCES public.clickup_drafts(workspace_id, id);
ALTER TABLE public.tool_runs DROP CONSTRAINT tool_runs_one_owner;
ALTER TABLE public.tool_runs ADD CONSTRAINT tool_runs_one_owner
    CHECK (num_nonnulls(agent_job_id, behavior_run_id, workflow_execution_id, clickup_draft_id) = 1);
CREATE INDEX tool_runs_clickup_draft_idx ON public.tool_runs(clickup_draft_id)
    WHERE clickup_draft_id IS NOT NULL;

-- +goose Down
-- Rows owned only by a draft cannot satisfy the old CHECK, so they are
-- deleted first (their audit trail is lost on rollback), and so are the
-- drafts' approvals.
DROP INDEX public.tool_runs_clickup_draft_idx;
DELETE FROM public.tool_runs WHERE clickup_draft_id IS NOT NULL;
ALTER TABLE public.tool_runs DROP CONSTRAINT tool_runs_one_owner;
ALTER TABLE public.tool_runs ADD CONSTRAINT tool_runs_one_owner
    CHECK (num_nonnulls(agent_job_id, behavior_run_id, workflow_execution_id) = 1);
ALTER TABLE public.tool_runs DROP CONSTRAINT tool_runs_clickup_draft_fkey;
ALTER TABLE public.tool_runs DROP COLUMN clickup_draft_id;
DELETE FROM public.workflow_signals WHERE reference_id IN (SELECT id FROM public.approvals WHERE kind = 'clickup_draft');
DELETE FROM public.workflow_waits WHERE approval_id IN (SELECT id FROM public.approvals WHERE kind = 'clickup_draft');
DELETE FROM public.approvals WHERE kind = 'clickup_draft';
ALTER TABLE public.approvals DROP CONSTRAINT approvals_kind_check;
ALTER TABLE public.approvals ADD CONSTRAINT approvals_kind_check
    CHECK (kind IN ('item_answer','post_draft','confirm_action','feedback','feedback_reason','contact_alias','assistant_action','workflow_target','worker_job','orchestrator_gate'));
DROP TABLE public.clickup_drafts;
