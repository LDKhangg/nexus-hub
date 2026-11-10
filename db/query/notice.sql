-- name: FindOrCreateNoticeThread :one
INSERT INTO public.hub_threads(id,workspace_id,integration_id,external_scope,thread_type,external_id,title)
VALUES ($1,$2,$3,'','task',$4,$5)
ON CONFLICT (integration_id,external_scope,thread_type,external_id)
DO UPDATE SET external_id=EXCLUDED.external_id
RETURNING *;

-- name: FindNoticeDuplicate :one
SELECT id FROM public.notification_outbox
WHERE workspace_id=$1 AND source_integration_id=$2 AND event_key=$3 AND target_integration_id=$4;

-- name: SaveNoticeMessage :one
INSERT INTO public.message_logs(thread_id,external_message_id,sender_id,sender_name,sender_role,content,occurred_at,metadata)
VALUES ($1,$2,$3,$4,'external_user',$5,$6,$7)
ON CONFLICT (thread_id,external_message_id) DO UPDATE SET external_message_id=EXCLUDED.external_message_id
RETURNING id;

-- name: InsertNoticeJob :exec
INSERT INTO public.notification_outbox(workspace_id,source_integration_id,target_integration_id,event_key,event,message_id)
VALUES ($1,$2,$3,$4,$5,$6);

-- name: ExpireFinalNoticeClaims :exec
UPDATE public.notification_outbox SET status='failed',last_error_code='attempts_exhausted',
locked_until=NULL,claim_token=NULL,updated_at=now()
WHERE attempts>=8 AND (status='pending' OR (status='processing' AND locked_until<=sqlc.arg(at_time)::timestamptz));

-- name: ClaimNotice :one
WITH candidate AS (
 SELECT id FROM public.notification_outbox
 WHERE attempts<8 AND ((status='pending' AND next_attempt_at<=sqlc.arg(at_time)::timestamptz)
 OR (status='processing' AND locked_until<=sqlc.arg(at_time)::timestamptz))
 ORDER BY next_attempt_at,id FOR UPDATE SKIP LOCKED LIMIT 1
)
UPDATE public.notification_outbox AS jobs SET status='processing',attempts=jobs.attempts+1,
claim_token=sqlc.arg(token)::uuid,locked_until=sqlc.arg(at_time)::timestamptz+interval '60 seconds',updated_at=now()
FROM candidate WHERE jobs.id=candidate.id RETURNING jobs.*;

-- name: GetNoticeClaim :one
SELECT * FROM public.notification_outbox WHERE id=$1 AND claim_token=$2 AND status='processing';

-- name: MarkNoticeSent :execrows
UPDATE public.notification_outbox SET status='sent',telegram_message_id=sqlc.arg(message_id)::bigint,
sent_at=now(),updated_at=now(),claim_token=NULL,locked_until=NULL,last_error_code=NULL
WHERE id=sqlc.arg(id) AND claim_token=sqlc.arg(token)::uuid AND status='processing';

-- name: MarkNoticeRetry :execrows
UPDATE public.notification_outbox SET status='pending',next_attempt_at=sqlc.arg(next_at)::timestamptz,
last_error_code=sqlc.arg(code)::text,updated_at=now(),claim_token=NULL,locked_until=NULL
WHERE id=sqlc.arg(id) AND claim_token=sqlc.arg(token)::uuid AND status='processing';

-- name: MarkNoticeFailed :execrows
UPDATE public.notification_outbox SET status='failed',last_error_code=sqlc.arg(code)::text,
updated_at=now(),claim_token=NULL,locked_until=NULL
WHERE id=sqlc.arg(id) AND claim_token=sqlc.arg(token)::uuid AND status='processing';
