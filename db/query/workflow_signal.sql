-- Durable approval waits and decision signals.

-- name: InsertWorkflowWait :one
INSERT INTO public.workflow_waits (workspace_id, execution_id, step_key, approval_id, wait_kind)
VALUES (sqlc.arg(workspace_id), sqlc.arg(execution_id), sqlc.arg(step_key), sqlc.arg(approval_id), sqlc.arg(wait_kind))
RETURNING *;

-- name: GetWorkflowWaitByApproval :one
SELECT * FROM public.workflow_waits
WHERE workspace_id = sqlc.arg(workspace_id) AND approval_id = sqlc.arg(approval_id);

-- name: ListStepWaits :many
SELECT w.id AS wait_id, w.wait_kind, sqlc.embed(a),
  COALESCE(sg.id, 0)::bigint AS signal_row_id, sg.consumed_at
FROM public.workflow_waits w
JOIN public.approvals a ON a.workspace_id = w.workspace_id AND a.id = w.approval_id
LEFT JOIN public.workflow_signals sg ON sg.workspace_id = w.workspace_id AND sg.wait_id = w.id
WHERE w.workspace_id = sqlc.arg(workspace_id) AND w.execution_id = sqlc.arg(execution_id)
  AND w.step_key = sqlc.arg(step_key)
ORDER BY w.id;

-- name: InsertWorkflowSignal :one
-- One signal per wait: a repeat (same signal ID or same wait) inserts nothing.
INSERT INTO public.workflow_signals (
  workspace_id, signal_id, execution_id, step_key, wait_id, reference_kind, reference_id, decision)
VALUES (sqlc.arg(workspace_id), sqlc.arg(signal_id), sqlc.arg(execution_id), sqlc.arg(step_key),
  sqlc.arg(wait_id), 'approval', sqlc.arg(reference_id), sqlc.arg(decision))
ON CONFLICT DO NOTHING
RETURNING *;

-- name: CountPendingStepSignals :one
SELECT COUNT(*) FROM public.workflow_signals
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id)
  AND step_key = sqlc.arg(step_key) AND consumed_at IS NULL;

-- name: ConsumeWorkflowSignal :execrows
UPDATE public.workflow_signals SET consumed_at = sqlc.arg(at_time)
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id)
  AND execution_id = sqlc.arg(execution_id) AND step_key = sqlc.arg(step_key)
  AND consumed_at IS NULL;

-- name: ListUnsignalledDecidedWaits :many
-- Bound approvals whose decision is recorded but whose signal is missing.
SELECT w.* FROM public.workflow_waits w
JOIN public.approvals a ON a.workspace_id = w.workspace_id AND a.id = w.approval_id
WHERE a.decision IS NOT NULL
  AND (sqlc.narg(execution_id)::bigint IS NULL
       OR (w.workspace_id = sqlc.narg(workspace_id)::uuid AND w.execution_id = sqlc.narg(execution_id)::bigint))
  AND NOT EXISTS (SELECT 1 FROM public.workflow_signals sg
                  WHERE sg.workspace_id = w.workspace_id AND sg.wait_id = w.id)
ORDER BY w.id
LIMIT 100;

-- name: ListWakeableWaitingSteps :many
-- Waiting executions whose waiting step has a recorded, unapplied signal
-- and no live queue row: a lost wakeup.
SELECT DISTINCT e.workspace_id, e.id AS execution_id, st.step_key
FROM public.workflow_executions e
JOIN public.execution_steps st
  ON st.workspace_id = e.workspace_id AND st.execution_id = e.id AND st.state = 'WAITING'
JOIN public.workflow_signals sg
  ON sg.workspace_id = st.workspace_id AND sg.execution_id = st.execution_id
 AND sg.step_key = st.step_key AND sg.consumed_at IS NULL
WHERE e.state IN ('WAITING_FOR_USER','WAITING_FOR_APPROVAL')
  AND NOT EXISTS (SELECT 1 FROM public.jobs j
                  WHERE j.workspace_id = e.workspace_id AND j.execution_id = e.id
                    AND j.status IN ('pending','processing'))
LIMIT 100;

-- name: GetWaitingExecutionStep :one
SELECT * FROM public.execution_steps
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id)
  AND state = 'WAITING'
ORDER BY position
LIMIT 1;
