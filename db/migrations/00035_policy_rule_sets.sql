-- +goose Up
-- Phase 9 P1: versioned policy rule sets and the decision audit.
-- A rule set version is immutable; apply and rollback add a new version and
-- make it the only active one. Only the owner's CLI (nh policy) writes
-- policy_rule_sets; the engine only reads it and appends policy_decisions.
CREATE TABLE public.policy_rule_sets (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    version integer NOT NULL CHECK (version >= 1),
    schema text NOT NULL CHECK (schema = 'policy/v1'),
    body jsonb NOT NULL CHECK (jsonb_typeof(body) = 'object'),
    body_sha256 text NOT NULL CHECK (body_sha256 ~ '^[0-9a-f]{64}$'),
    created_by text NOT NULL CHECK (created_by IN ('migration', 'init', 'cli')),
    restored_from_version integer CHECK (restored_from_version IS NULL OR restored_from_version >= 1),
    is_active boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    activated_at timestamptz,
    CHECK (NOT is_active OR activated_at IS NOT NULL),
    UNIQUE (workspace_id, version),
    UNIQUE (workspace_id, id)
);
CREATE UNIQUE INDEX policy_rule_sets_one_active_idx ON public.policy_rule_sets (workspace_id)
    WHERE is_active;
COMMENT ON TABLE public.policy_rule_sets IS 'Workspace-scoped, immutable policy rule set versions; exactly one active per workspace; written only by the owner CLI.';
ALTER TABLE public.policy_rule_sets ENABLE ROW LEVEL SECURITY;

-- A decision may name the tool run it gated; tool_runs gets the composite
-- key that a workspace-scoped foreign key needs.
ALTER TABLE public.tool_runs ADD CONSTRAINT tool_runs_workspace_id_id_key UNIQUE (workspace_id, id);

-- One row per engine decision: ids, names, effect, reason, rule reference
-- and an input hash only. Never inputs, text, prompts or tokens.
CREATE TABLE public.policy_decisions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    action text NOT NULL CHECK (action ~ '^([a-z][a-z0-9_.]{0,63})?$'),
    tier text NOT NULL CHECK (tier IN ('', 'read', 'write', 'dangerous')),
    resource_key text NOT NULL DEFAULT '' CHECK (octet_length(resource_key) <= 256 AND resource_key ~ '^[!-~]*$'),
    effect text NOT NULL CHECK (effect IN ('allow', 'approval_required', 'deny')),
    reason text NOT NULL CHECK (reason ~ '^[a-z0-9_]{1,64}$'),
    rule_set_version integer CHECK (rule_set_version IS NULL OR rule_set_version >= 1),
    rule_key text NOT NULL DEFAULT '' CHECK (octet_length(rule_key) <= 100),
    actor_kind text NOT NULL CHECK (actor_kind IN ('', 'agent', 'behavior', 'workflow', 'draft', 'system')),
    execution_id bigint,
    tool_run_id bigint,
    approval_id bigint,
    input_sha256 text NOT NULL DEFAULT '' CHECK (input_sha256 = '' OR input_sha256 ~ '^[0-9a-f]{64}$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, rule_set_version) REFERENCES public.policy_rule_sets(workspace_id, version),
    FOREIGN KEY (workspace_id, execution_id) REFERENCES public.workflow_executions(workspace_id, id),
    FOREIGN KEY (workspace_id, tool_run_id) REFERENCES public.tool_runs(workspace_id, id),
    FOREIGN KEY (workspace_id, approval_id) REFERENCES public.approvals(workspace_id, id),
    UNIQUE (workspace_id, id)
);
CREATE INDEX policy_decisions_workspace_created_idx ON public.policy_decisions (workspace_id, created_at DESC, id DESC);
COMMENT ON TABLE public.policy_decisions IS 'Workspace-scoped audit of every policy engine decision; metadata and an input hash only.';
ALTER TABLE public.policy_decisions ENABLE ROW LEVEL SECURITY;

-- No behaviour change on upgrade: every existing workspace gets rule set v1
-- (read allow, write allow, dangerous approval_required). The literal equals
-- policy.DefaultV1JSON byte for byte, and its hash is computed from the same
-- bytes, so it matches the engine's canonical hash.
INSERT INTO public.policy_rule_sets (workspace_id, version, schema, body, body_sha256, created_by, is_active, activated_at)
SELECT w.id, 1, 'policy/v1', v1.body::jsonb, encode(sha256(convert_to(v1.body, 'UTF8')), 'hex'), 'migration', true, now()
FROM public.workspaces w
CROSS JOIN (SELECT '{"schema":"policy/v1","defaults":{"dangerous":"approval_required","read":"allow","write":"allow"},"actions":{"github_pr_merge":"approval_required"},"approval_ttl_hours":24}'::text AS body) v1;

-- +goose Down
DROP TABLE public.policy_decisions;
ALTER TABLE public.tool_runs DROP CONSTRAINT tool_runs_workspace_id_id_key;
DROP TABLE public.policy_rule_sets;
