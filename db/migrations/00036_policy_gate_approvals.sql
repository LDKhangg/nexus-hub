-- +goose Up
-- Phase 9 P2: an approval of kind policy_gate binds one policy decision to
-- one exact action: workspace, policy action, resource key, input hash and
-- execution. It authorizes that action once (consumed_at), only after the
-- owner approved it and before it expires. Ids, names and a hash only:
-- never inputs, text, prompts or tokens.
ALTER TABLE public.approvals DROP CONSTRAINT approvals_kind_check;
ALTER TABLE public.approvals ADD CONSTRAINT approvals_kind_check
    CHECK (kind IN ('item_answer','post_draft','confirm_action','feedback','feedback_reason','contact_alias','assistant_action','workflow_target','worker_job','orchestrator_gate','clickup_draft','policy_gate'));

ALTER TABLE public.approvals
    ADD COLUMN policy_action text CHECK (policy_action IS NULL OR policy_action ~ '^[a-z][a-z0-9_.]{0,63}$'),
    ADD COLUMN resource_key text CHECK (resource_key IS NULL OR (octet_length(resource_key) <= 256 AND resource_key ~ '^[!-~]*$')),
    ADD COLUMN input_sha256 text CHECK (input_sha256 IS NULL OR input_sha256 = '' OR input_sha256 ~ '^[0-9a-f]{64}$'),
    ADD COLUMN execution_id bigint,
    ADD COLUMN requested_user_id uuid,
    ADD COLUMN decided_user_id uuid,
    ADD COLUMN consumed_at timestamptz,
    ADD CONSTRAINT approvals_execution_fkey FOREIGN KEY (workspace_id, execution_id)
        REFERENCES public.workflow_executions(workspace_id, id),
    ADD CONSTRAINT approvals_requested_user_fkey FOREIGN KEY (requested_user_id, workspace_id)
        REFERENCES public.workspace_members(user_id, workspace_id),
    ADD CONSTRAINT approvals_decided_user_fkey FOREIGN KEY (decided_user_id, workspace_id)
        REFERENCES public.workspace_members(user_id, workspace_id),
    -- A policy gate always names its action, resource and input hash.
    ADD CONSTRAINT approvals_policy_gate_binding_check
        CHECK (kind <> 'policy_gate' OR (policy_action IS NOT NULL AND resource_key IS NOT NULL AND input_sha256 IS NOT NULL)),
    -- Only an approved policy gate can be consumed, and only once decided.
    ADD CONSTRAINT approvals_consumed_check
        CHECK (consumed_at IS NULL OR (kind = 'policy_gate' AND decision = 'approved'));

COMMENT ON COLUMN public.approvals.policy_action IS 'policy_gate: the tool or capability name the gate authorizes.';
COMMENT ON COLUMN public.approvals.resource_key IS 'policy_gate: the bounded resource key, such as owner/repo#12.';
COMMENT ON COLUMN public.approvals.input_sha256 IS 'policy_gate: SHA-256 of the canonical action input; never the input itself.';
COMMENT ON COLUMN public.approvals.consumed_at IS 'policy_gate: set once, in the same transaction as the tool run it authorized.';

-- One open policy gate per (workspace, action, resource, input hash).
CREATE UNIQUE INDEX approvals_one_open_policy_gate_idx
    ON public.approvals (workspace_id, policy_action, resource_key, input_sha256)
    WHERE kind = 'policy_gate' AND decision IS NULL;
CREATE INDEX approvals_execution_idx ON public.approvals (workspace_id, execution_id)
    WHERE execution_id IS NOT NULL;

-- +goose Down
DELETE FROM public.policy_decisions d USING public.approvals a
    WHERE d.workspace_id = a.workspace_id AND d.approval_id = a.id AND a.kind = 'policy_gate';
DELETE FROM public.workflow_signals WHERE reference_id IN (SELECT id FROM public.approvals WHERE kind = 'policy_gate');
DELETE FROM public.workflow_waits WHERE approval_id IN (SELECT id FROM public.approvals WHERE kind = 'policy_gate');
DELETE FROM public.approvals WHERE kind = 'policy_gate';
DROP INDEX public.approvals_execution_idx;
DROP INDEX public.approvals_one_open_policy_gate_idx;
ALTER TABLE public.approvals
    DROP CONSTRAINT approvals_consumed_check,
    DROP CONSTRAINT approvals_policy_gate_binding_check,
    DROP CONSTRAINT approvals_decided_user_fkey,
    DROP CONSTRAINT approvals_requested_user_fkey,
    DROP CONSTRAINT approvals_execution_fkey,
    DROP COLUMN consumed_at,
    DROP COLUMN decided_user_id,
    DROP COLUMN requested_user_id,
    DROP COLUMN execution_id,
    DROP COLUMN input_sha256,
    DROP COLUMN resource_key,
    DROP COLUMN policy_action;
ALTER TABLE public.approvals DROP CONSTRAINT approvals_kind_check;
ALTER TABLE public.approvals ADD CONSTRAINT approvals_kind_check
    CHECK (kind IN ('item_answer','post_draft','confirm_action','feedback','feedback_reason','contact_alias','assistant_action','workflow_target','worker_job','orchestrator_gate','clickup_draft'));
