-- Phase 9 P1: policy rule sets and decisions. Only the owner CLI writes
-- policy_rule_sets (InsertPolicyRuleSet, DeactivatePolicyRuleSets,
-- EnsureDefaultPolicyRuleSet).

-- name: LockPolicyWorkspace :one
SELECT id FROM public.workspaces WHERE id = $1 FOR UPDATE;

-- name: GetActivePolicyRuleSet :one
SELECT * FROM public.policy_rule_sets
WHERE workspace_id = $1 AND is_active;

-- name: GetPolicyRuleSetVersion :one
SELECT * FROM public.policy_rule_sets
WHERE workspace_id = $1 AND version = $2;

-- name: NextPolicyRuleSetVersion :one
SELECT (COALESCE(MAX(version), 0) + 1)::integer AS next_version
FROM public.policy_rule_sets WHERE workspace_id = $1;

-- name: DeactivatePolicyRuleSets :exec
UPDATE public.policy_rule_sets SET is_active = false
WHERE workspace_id = $1 AND is_active;

-- name: InsertPolicyRuleSet :one
INSERT INTO public.policy_rule_sets (workspace_id, version, schema, body, body_sha256, created_by, restored_from_version, is_active, activated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, true, now())
RETURNING *;

-- name: EnsureDefaultPolicyRuleSet :execrows
-- Gives a workspace that has no rule set version 1 (nh init).
INSERT INTO public.policy_rule_sets (workspace_id, version, schema, body, body_sha256, created_by, is_active, activated_at)
SELECT $1, 1, $2, $3, $4, 'init', true, now()
WHERE NOT EXISTS (SELECT 1 FROM public.policy_rule_sets p WHERE p.workspace_id = $1);

-- name: ListPolicyRuleSets :many
SELECT * FROM public.policy_rule_sets
WHERE workspace_id = $1
ORDER BY version DESC
LIMIT $2;

-- name: InsertPolicyDecision :one
INSERT INTO public.policy_decisions (workspace_id, action, tier, resource_key, effect, reason, rule_set_version, rule_key, actor_kind, execution_id, tool_run_id, approval_id, input_sha256, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
RETURNING id;

-- name: ListPolicyDecisions :many
SELECT * FROM public.policy_decisions
WHERE workspace_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2;
