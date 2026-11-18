-- Phase 9 policy_gate approvals: one exact action, approved once by the
-- owner, consumed once by the tool run it authorizes.

-- name: CreatePolicyGateApproval :one
-- Creates the open gate; no row when an open gate for the same
-- (workspace, action, resource, input hash) already exists. The expiry is
-- capped at the execution's hard deadline: a gate never outlives its
-- execution (review B R-B-1).
INSERT INTO public.approvals(workspace_id, kind, action, resource, nonce, expires_at,
    policy_action, resource_key, input_sha256, execution_id, requested_user_id)
VALUES (sqlc.arg(workspace_id), 'policy_gate', sqlc.arg(policy_action)::text, sqlc.arg(resource_key)::text, sqlc.arg(nonce),
    LEAST(sqlc.arg(expires_at)::timestamptz,
          (SELECT e.deadline_at FROM public.workflow_executions e
           WHERE e.workspace_id = sqlc.arg(workspace_id) AND e.id = sqlc.narg(execution_id))),
    sqlc.arg(policy_action)::text, sqlc.arg(resource_key)::text, sqlc.arg(input_sha256)::text, sqlc.narg(execution_id), sqlc.narg(requested_user_id))
ON CONFLICT (workspace_id, policy_action, resource_key, input_sha256) WHERE kind = 'policy_gate' AND decision IS NULL
DO NOTHING
RETURNING id, nonce, expires_at, COALESCE(execution_id, 0)::bigint AS execution_id;

-- name: ExpireStaleOpenPolicyGate :one
-- Expires the open gate for the same (workspace, action, resource, input
-- hash) when it is past its expiry or its execution has ended (completed,
-- failed, cancelled, or past its deadline), so a new request gets its own
-- gate (review B R-B-1, R-B-5). No row when there is none.
UPDATE public.approvals a SET decision = 'expired', decided_by = 'system', decided_at = now(), updated_at = now()
WHERE a.workspace_id = sqlc.arg(workspace_id) AND a.kind = 'policy_gate' AND a.decision IS NULL
  AND a.policy_action = sqlc.arg(policy_action)::text AND a.resource_key = sqlc.arg(resource_key)::text
  AND a.input_sha256 = sqlc.arg(input_sha256)::text
  AND (a.expires_at <= sqlc.arg(at_time)::timestamptz
       OR EXISTS (SELECT 1 FROM public.workflow_executions e
                  WHERE e.workspace_id = a.workspace_id AND e.id = a.execution_id
                    AND (e.state IN ('COMPLETED', 'FAILED', 'CANCELLED')
                         OR (e.deadline_at IS NOT NULL AND e.deadline_at <= sqlc.arg(at_time)::timestamptz))))
RETURNING a.id;

-- name: ExpireEndedExecutionPolicyGates :many
-- Expires up to max_rows open policy gates whose execution has ended
-- (completed, failed, cancelled, or past its deadline); the sweep then
-- closes their messages (review B R-B-1).
UPDATE public.approvals a SET decision = 'expired', decided_by = 'system', decided_at = now(), updated_at = now()
WHERE a.id IN (SELECT g.id FROM public.approvals g
               JOIN public.workflow_executions e ON e.workspace_id = g.workspace_id AND e.id = g.execution_id
               WHERE g.kind = 'policy_gate' AND g.decision IS NULL
                 AND (e.state IN ('COMPLETED', 'FAILED', 'CANCELLED')
                      OR (e.deadline_at IS NOT NULL AND e.deadline_at <= sqlc.arg(at_time)::timestamptz))
               ORDER BY g.id LIMIT sqlc.arg(max_rows)::int FOR UPDATE OF g SKIP LOCKED)
  AND a.decision IS NULL
RETURNING a.workspace_id, a.id, a.nonce, a.expires_at, COALESCE(a.policy_action, '')::text AS policy_action,
       COALESCE(a.resource_key, '')::text AS resource_key, COALESCE(a.input_sha256, '')::text AS input_sha256,
       COALESCE(a.execution_id, 0)::bigint AS execution_id, COALESCE(a.telegram_message_id, 0)::bigint AS telegram_message_id,
       a.payload;

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

-- name: GetPolicyGateApproval :one
-- One policy gate with its decision, binding and message (ids only).
SELECT id, nonce, decision, expires_at, consumed_at, COALESCE(policy_action, '')::text AS policy_action,
       COALESCE(resource_key, '')::text AS resource_key, COALESCE(input_sha256, '')::text AS input_sha256,
       COALESCE(execution_id, 0)::bigint AS execution_id, COALESCE(telegram_message_id, 0)::bigint AS telegram_message_id,
       payload, COALESCE(decided_by, '')::text AS decided_by
FROM public.approvals
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND kind = 'policy_gate';

-- name: SetPolicyGateMessage :execrows
-- Stores where the gate's approval message was sent (chat, topic, message).
UPDATE public.approvals SET telegram_message_id = sqlc.arg(message_id)::bigint, payload = sqlc.arg(payload), updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id) AND kind = 'policy_gate';

-- name: DecidePolicyGateApproval :one
-- Atomic: only an open, unexpired gate whose execution is still running
-- (not completed, failed or cancelled, and before its deadline) is
-- decided, once; records the Telegram actor and the internal user who
-- decided (review B R-B-1).
UPDATE public.approvals a SET decision = sqlc.arg(decision)::text, decided_by = sqlc.arg(decided_by)::text,
    decided_user_id = sqlc.narg(decided_user_id), decided_at = now(), updated_at = now()
WHERE a.workspace_id = sqlc.arg(workspace_id) AND a.nonce = sqlc.arg(nonce) AND a.kind = 'policy_gate'
  AND a.decision IS NULL AND a.expires_at > sqlc.arg(at_time)::timestamptz
  AND NOT EXISTS (SELECT 1 FROM public.workflow_executions e
                  WHERE e.workspace_id = a.workspace_id AND e.id = a.execution_id
                    AND (e.state IN ('COMPLETED', 'FAILED', 'CANCELLED')
                         OR (e.deadline_at IS NOT NULL AND e.deadline_at <= sqlc.arg(at_time)::timestamptz)))
RETURNING a.id, COALESCE(a.execution_id, 0)::bigint AS execution_id;

-- name: GetWorkspaceOwnerMember :one
-- The workspace member with role owner (the policy gate approver).
SELECT user_id FROM public.workspace_members
WHERE workspace_id = sqlc.arg(workspace_id) AND role = 'owner'
ORDER BY created_at, user_id
LIMIT 1;
