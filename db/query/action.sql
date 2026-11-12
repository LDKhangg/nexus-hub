-- name: ExpireFinalActionClaims :exec
UPDATE public.outbound_actions SET status = 'failed', last_error_code = 'attempts_exhausted',
  locked_until = NULL, claim_token = NULL, updated_at = now()
WHERE attempts >= 8 AND (status = 'pending' OR (status = 'processing' AND locked_until <= sqlc.arg(at_time)::timestamptz));

-- name: ClaimOutboundAction :one
WITH candidate AS (
 SELECT id FROM public.outbound_actions
 WHERE attempts < 8 AND ((status = 'pending' AND next_attempt_at <= sqlc.arg(at_time)::timestamptz)
   OR (status = 'processing' AND locked_until <= sqlc.arg(at_time)::timestamptz))
 ORDER BY next_attempt_at, id FOR UPDATE SKIP LOCKED LIMIT 1
)
UPDATE public.outbound_actions AS a SET status = 'processing', attempts = a.attempts + 1,
  claim_token = sqlc.arg(token)::uuid, locked_until = sqlc.arg(at_time)::timestamptz + interval '60 seconds', updated_at = now()
FROM candidate WHERE a.id = candidate.id
RETURNING a.id, a.workspace_id, a.integration_id, a.kind, a.payload, a.attempts;

-- name: GetActionClaim :one
SELECT id, workspace_id, integration_id FROM public.outbound_actions
WHERE id = sqlc.arg(id) AND claim_token = sqlc.arg(token)::uuid AND status = 'processing';

-- name: MarkActionSent :execrows
UPDATE public.outbound_actions SET status = 'sent', provider_message_id = sqlc.arg(message_id)::bigint,
  sent_at = now(), updated_at = now(), claim_token = NULL, locked_until = NULL, last_error_code = NULL
WHERE id = sqlc.arg(id) AND claim_token = sqlc.arg(token)::uuid AND status = 'processing';

-- name: MarkActionRetry :execrows
UPDATE public.outbound_actions SET status = 'pending', next_attempt_at = sqlc.arg(next_at)::timestamptz,
  last_error_code = sqlc.arg(code)::text, updated_at = now(), claim_token = NULL, locked_until = NULL
WHERE id = sqlc.arg(id) AND claim_token = sqlc.arg(token)::uuid AND status = 'processing';

-- name: MarkActionFailed :execrows
UPDATE public.outbound_actions SET status = 'failed', last_error_code = sqlc.arg(code)::text,
  updated_at = now(), claim_token = NULL, locked_until = NULL
WHERE id = sqlc.arg(id) AND claim_token = sqlc.arg(token)::uuid AND status = 'processing';
