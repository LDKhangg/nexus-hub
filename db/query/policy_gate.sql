-- Phase 9 policy_gate approvals: one exact action, approved once by the
-- owner, consumed once by the tool run it authorizes.

-- name: CreatePolicyGateApproval :one
-- Creates the open gate; no row when an open gate for the same
-- (workspace, action, resource, input hash) already exists.
INSERT INTO public.approvals(workspace_id, kind, action, resource, nonce, expires_at,
    policy_action, resource_key, input_sha256, execution_id, requested_user_id)
VALUES (sqlc.arg(workspace_id), 'policy_gate', sqlc.arg(policy_action)::text, sqlc.arg(resource_key)::text, sqlc.arg(nonce), sqlc.arg(expires_at),
    sqlc.arg(policy_action)::text, sqlc.arg(resource_key)::text, sqlc.arg(input_sha256)::text, sqlc.narg(execution_id), sqlc.narg(requested_user_id))
ON CONFLICT (workspace_id, policy_action, resource_key, input_sha256) WHERE kind = 'policy_gate' AND decision IS NULL
DO NOTHING
RETURNING id, nonce, expires_at, COALESCE(execution_id, 0)::bigint AS execution_id;

-- name: GetOpenPolicyGateApproval :one
-- The open gate for the same (workspace, action, resource, input hash).
SELECT id, nonce, expires_at, COALESCE(execution_id, 0)::bigint AS execution_id
FROM public.approvals
WHERE workspace_id = sqlc.arg(workspace_id) AND kind = 'policy_gate' AND decision IS NULL
  AND policy_action = sqlc.arg(policy_action)::text AND resource_key = sqlc.arg(resource_key)::text
  AND input_sha256 = sqlc.arg(input_sha256)::text;

-- name: LockPolicyGateApproval :one
-- Locks one policy gate for the grant check in the tool run transaction.
SELECT id, decision, expires_at, consumed_at, COALESCE(policy_action, '')::text AS policy_action,
       COALESCE(resource_key, '')::text AS resource_key, COALESCE(input_sha256, '')::text AS input_sha256,
       COALESCE(execution_id, 0)::bigint AS execution_id
FROM public.approvals
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND kind = 'policy_gate'
FOR UPDATE;

-- name: ConsumePolicyGateApproval :execrows
-- Marks an approved, unexpired, unconsumed gate as used; once only.
UPDATE public.approvals SET consumed_at = now(), updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND kind = 'policy_gate'
  AND decision = 'approved' AND consumed_at IS NULL AND expires_at > sqlc.arg(at_time)::timestamptz;
