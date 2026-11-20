-- Phase 12 B2: Telegram alerts for stuck and failed workflow executions.
-- Ids, behavior names, states, times and codes only; never content.

-- name: ListWorkflowAlertCandidates :many
-- Executions in an alert state that has no alert row yet, oldest first:
-- stuck_running: a processing job unchanged since running_before;
-- stuck_queued: a pending job due since queued_before;
-- approval_overdue: WAITING_FOR_APPROVAL with an open bound approval or
--   policy gate that expired before approval_before (the sweep missed it);
-- failed: FAILED and finished since failed_since.
-- since_at is when the alert state began (job change, due time, approval
-- expiry, finish).
SELECT c.workspace_id, c.execution_id, c.alert_state::text AS alert_state, b.name AS behavior_name,
       c.since_at::timestamptz AS since_at, COALESCE(c.error_code, '')::text AS error_code
FROM (
    SELECT j.workspace_id, j.execution_id, 'stuck_running'::text AS alert_state,
           j.updated_at AS since_at, COALESCE(j.last_error_code, e.reason_code) AS error_code
    FROM public.jobs j
    JOIN public.workflow_executions e ON e.workspace_id = j.workspace_id AND e.id = j.execution_id
    WHERE j.status = 'processing' AND j.updated_at <= sqlc.arg(running_before)::timestamptz
      AND e.state NOT IN ('COMPLETED','FAILED','CANCELLED')
    UNION ALL
    SELECT j.workspace_id, j.execution_id, 'stuck_queued'::text,
           j.due_at, COALESCE(j.last_error_code, e.reason_code)
    FROM public.jobs j
    JOIN public.workflow_executions e ON e.workspace_id = j.workspace_id AND e.id = j.execution_id
    WHERE j.status = 'pending' AND j.due_at <= sqlc.arg(queued_before)::timestamptz
      AND e.state NOT IN ('COMPLETED','FAILED','CANCELLED')
    UNION ALL
    SELECT e.workspace_id, e.id, 'approval_overdue'::text,
           min(a.expires_at), e.reason_code
    FROM public.workflow_executions e
    JOIN public.approvals a ON a.workspace_id = e.workspace_id
    WHERE e.state = 'WAITING_FOR_APPROVAL'
      AND a.decision IS NULL AND a.expires_at <= sqlc.arg(approval_before)::timestamptz
      AND (a.execution_id = e.id OR a.id IN (
          SELECT w.approval_id FROM public.workflow_waits w
          WHERE w.workspace_id = e.workspace_id AND w.execution_id = e.id))
    GROUP BY e.workspace_id, e.id, e.reason_code
    UNION ALL
    SELECT e.workspace_id, e.id, 'failed'::text,
           e.finished_at, e.reason_code
    FROM public.workflow_executions e
    WHERE e.state = 'FAILED' AND e.finished_at >= sqlc.arg(failed_since)::timestamptz
) c
JOIN public.workflow_executions x ON x.workspace_id = c.workspace_id AND x.id = c.execution_id
JOIN public.behavior_versions v ON v.workspace_id = x.workspace_id AND v.id = x.behavior_version_id
JOIN public.behaviors b ON b.workspace_id = v.workspace_id AND b.id = v.behavior_id
WHERE NOT EXISTS (
    SELECT 1 FROM public.workflow_alerts wa
    WHERE wa.workspace_id = c.workspace_id AND wa.execution_id = c.execution_id AND wa.state = c.alert_state)
ORDER BY c.since_at, c.execution_id, c.alert_state
LIMIT sqlc.arg(row_limit);

-- name: ClaimWorkflowAlert :execrows
-- Claims the one alert of (execution, state) as pending (or suppressed by
-- the hourly cap); 0 rows when it was claimed before (dedup), so the
-- caller sends nothing. A pending claim is due again at next_attempt_at,
-- so a crash before the send is retried.
INSERT INTO public.workflow_alerts (workspace_id, execution_id, state, status, next_attempt_at, since_at, error_code, created_at)
VALUES (sqlc.arg(workspace_id), sqlc.arg(execution_id), sqlc.arg(state), sqlc.arg(status),
        CASE WHEN sqlc.arg(status)::text = 'pending' THEN sqlc.arg(next_attempt_at)::timestamptz END,
        sqlc.narg(since_at)::timestamptz, sqlc.narg(error_code)::text, sqlc.arg(created_at))
ON CONFLICT (workspace_id, execution_id, state) DO NOTHING;

-- name: MarkWorkflowAlertSent :execrows
-- Marks a pending alert delivered.
UPDATE public.workflow_alerts
SET status = 'sent', sent_at = sqlc.arg(sent_at), attempts = attempts + 1,
    next_attempt_at = NULL, last_error_code = NULL
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id) AND state = sqlc.arg(state)
  AND status = 'pending';

-- name: MarkWorkflowAlertAttemptFailed :execrows
-- Counts one failed send attempt of a pending alert made after
-- prev_attempts attempts: the alert is due again at next_attempt_at, or
-- is failed for good when next_attempt_at is NULL.
UPDATE public.workflow_alerts
SET attempts = attempts + 1,
    status = CASE WHEN sqlc.narg(next_attempt_at)::timestamptz IS NULL THEN 'failed' ELSE 'pending' END,
    next_attempt_at = sqlc.narg(next_attempt_at)::timestamptz,
    last_error_code = sqlc.arg(last_error_code)
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id) AND state = sqlc.arg(state)
  AND status = 'pending' AND attempts = sqlc.arg(prev_attempts);

-- name: ListDueWorkflowAlertRetries :many
-- Pending alerts due for a (re)try, oldest first, with what the alert
-- line shows.
SELECT wa.workspace_id, wa.execution_id, wa.state, b.name AS behavior_name,
       COALESCE(wa.since_at, wa.created_at)::timestamptz AS since_at,
       COALESCE(wa.error_code, '')::text AS error_code, wa.attempts
FROM public.workflow_alerts wa
JOIN public.workflow_executions x ON x.workspace_id = wa.workspace_id AND x.id = wa.execution_id
JOIN public.behavior_versions v ON v.workspace_id = x.workspace_id AND v.id = x.behavior_version_id
JOIN public.behaviors b ON b.workspace_id = v.workspace_id AND b.id = v.behavior_id
WHERE wa.status = 'pending' AND wa.next_attempt_at <= sqlc.arg(due_at)::timestamptz
ORDER BY wa.next_attempt_at, wa.created_at, wa.execution_id, wa.state
LIMIT sqlc.arg(row_limit);

-- name: CountWorkflowAlertsSentSince :one
-- Alerts delivered since a time, hub-wide (the hourly rate cap).
SELECT count(*)::bigint AS n
FROM public.workflow_alerts
WHERE status = 'sent' AND sent_at >= sqlc.arg(since)::timestamptz;

-- name: ListUnsummarisedWorkflowAlerts :many
-- Per workspace: suppressed alerts claimed before a time and not yet
-- counted in a summary, with the oldest claim time.
SELECT workspace_id, count(*)::bigint AS n, min(created_at)::timestamptz AS oldest_at
FROM public.workflow_alerts
WHERE status = 'suppressed' AND summarised_at IS NULL AND created_at < sqlc.arg(before)::timestamptz
GROUP BY workspace_id
ORDER BY workspace_id;

-- name: MarkWorkflowAlertsSummarised :execrows
-- Marks a workspace's suppressed alerts claimed before a time summarised.
UPDATE public.workflow_alerts
SET summarised_at = sqlc.arg(summarised_at)
WHERE workspace_id = sqlc.arg(workspace_id) AND status = 'suppressed' AND summarised_at IS NULL
  AND created_at < sqlc.arg(before)::timestamptz;

-- name: ReleaseWorkflowAlert :execrows
-- Deletes an alert row (operator recovery); the next scan may alert again.
DELETE FROM public.workflow_alerts
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id) AND state = sqlc.arg(state);

-- name: CountOpenWorkflowAlerts :one
-- Open alerts: a stuck or overdue alert whose execution has not ended, or
-- a failed alert claimed since failed_since. Hub-wide or for one workspace.
SELECT count(*)::bigint AS n
FROM public.workflow_alerts wa
JOIN public.workflow_executions e ON e.workspace_id = wa.workspace_id AND e.id = wa.execution_id
WHERE (sqlc.narg(workspace_id)::uuid IS NULL OR wa.workspace_id = sqlc.narg(workspace_id)::uuid)
  AND ((wa.state <> 'failed' AND e.state NOT IN ('COMPLETED','FAILED','CANCELLED'))
       OR (wa.state = 'failed' AND wa.created_at >= sqlc.arg(failed_since)::timestamptz));

-- name: CountFailedWorkflowAlerts :one
-- Alerts whose send attempts are exhausted (status failed), hub-wide or
-- for one workspace.
SELECT count(*)::bigint AS n
FROM public.workflow_alerts
WHERE status = 'failed'
  AND (sqlc.narg(workspace_id)::uuid IS NULL OR workspace_id = sqlc.narg(workspace_id)::uuid);
