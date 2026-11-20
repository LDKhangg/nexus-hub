-- name: DeleteRetentionOutboundActions :execrows
WITH picked AS MATERIALIZED (
    SELECT o.id
    FROM outbound_actions o
    WHERE o.status IN ('sent', 'failed')
      AND o.updated_at < sqlc.arg(cutoff)
      AND (sqlc.narg(workspace_id)::uuid IS NULL OR o.workspace_id = sqlc.narg(workspace_id))
    ORDER BY o.updated_at, o.id
    LIMIT sqlc.arg(batch_size)
    FOR UPDATE SKIP LOCKED
)
DELETE FROM outbound_actions o USING picked p WHERE o.id = p.id;

-- name: DeleteRetentionNotificationOutbox :execrows
WITH picked AS MATERIALIZED (
    SELECT o.id
    FROM notification_outbox o
    WHERE o.status IN ('sent', 'failed')
      AND o.updated_at < sqlc.arg(cutoff)
      AND (sqlc.narg(workspace_id)::uuid IS NULL OR o.workspace_id = sqlc.narg(workspace_id))
    ORDER BY o.updated_at, o.id
    LIMIT sqlc.arg(batch_size)
    FOR UPDATE SKIP LOCKED
)
DELETE FROM notification_outbox o USING picked p WHERE o.id = p.id;

-- name: DeleteRetentionMessageLogs :execrows
WITH picked AS MATERIALIZED (
    SELECT m.id
    FROM message_logs m
    JOIN hub_threads t ON t.id = m.thread_id
    WHERE m.created_at < sqlc.arg(cutoff)
      AND (sqlc.narg(workspace_id)::uuid IS NULL OR t.workspace_id = sqlc.narg(workspace_id))
      AND t.summary_through_message_id IS DISTINCT FROM m.id
      AND NOT EXISTS (SELECT 1 FROM notification_outbox o WHERE o.message_id = m.id)
      AND NOT EXISTS (
          SELECT 1 FROM agent_jobs a
          WHERE a.thread_id = t.id AND a.status IN ('pending', 'processing')
      )
    ORDER BY m.created_at, m.id
    LIMIT sqlc.arg(batch_size)
    FOR UPDATE OF m SKIP LOCKED
)
DELETE FROM message_logs m USING picked p WHERE m.id = p.id;

-- name: DeleteRetentionPolicyDecisions :execrows
WITH picked AS MATERIALIZED (
    SELECT p.id
    FROM policy_decisions p
    WHERE p.created_at < sqlc.arg(cutoff)
      AND (sqlc.narg(workspace_id)::uuid IS NULL OR p.workspace_id = sqlc.narg(workspace_id))
      AND NOT EXISTS (
          SELECT 1 FROM workflow_executions e
          WHERE e.id = p.execution_id AND e.state NOT IN ('COMPLETED', 'FAILED', 'CANCELLED')
      )
      AND NOT EXISTS (SELECT 1 FROM approvals a WHERE a.id = p.approval_id AND a.decision IS NULL)
      AND NOT EXISTS (SELECT 1 FROM tool_runs r WHERE r.id = p.tool_run_id AND r.status = 'started')
    ORDER BY p.created_at, p.id
    LIMIT sqlc.arg(batch_size)
    FOR UPDATE OF p SKIP LOCKED
)
DELETE FROM policy_decisions p USING picked x WHERE p.id = x.id;

-- name: DeleteRetentionToolRuns :execrows
WITH picked AS MATERIALIZED (
    SELECT r.id
    FROM tool_runs r
    WHERE r.created_at < sqlc.arg(cutoff)
      AND r.status <> 'started'
      AND (sqlc.narg(workspace_id)::uuid IS NULL OR r.workspace_id = sqlc.narg(workspace_id))
      AND NOT EXISTS (SELECT 1 FROM policy_decisions p WHERE p.tool_run_id = r.id)
      AND NOT EXISTS (
          SELECT 1 FROM agent_jobs a
          WHERE a.id = r.agent_job_id AND a.status IN ('pending', 'processing')
      )
      AND NOT EXISTS (
          SELECT 1 FROM behavior_runs b
          WHERE b.id = r.behavior_run_id AND b.status IN ('pending', 'running')
      )
      AND NOT EXISTS (
          SELECT 1 FROM workflow_executions e
          WHERE e.id = r.workflow_execution_id AND e.state NOT IN ('COMPLETED', 'FAILED', 'CANCELLED')
      )
      AND NOT EXISTS (
          SELECT 1 FROM clickup_drafts d
          WHERE d.id = r.clickup_draft_id AND d.state IN ('pending', 'posting', 'failed')
      )
    ORDER BY r.created_at, r.id
    LIMIT sqlc.arg(batch_size)
    FOR UPDATE OF r SKIP LOCKED
)
DELETE FROM tool_runs r USING picked p WHERE r.id = p.id;

-- name: PickRetentionWorkflowExecutions :many
WITH picked AS MATERIALIZED (
    SELECT e.id
    FROM workflow_executions e
    WHERE e.state IN ('COMPLETED', 'FAILED', 'CANCELLED')
      AND e.finished_at < sqlc.arg(cutoff)
      AND (sqlc.narg(workspace_id)::uuid IS NULL OR e.workspace_id = sqlc.narg(workspace_id))
      AND NOT (e.id = ANY(sqlc.arg(skip_ids)::bigint[]))
      AND NOT EXISTS (SELECT 1 FROM workflow_executions c WHERE c.parent_execution_id = e.id)
      AND NOT EXISTS (SELECT 1 FROM approvals a WHERE a.execution_id = e.id AND a.decision IS NULL)
      AND NOT EXISTS (SELECT 1 FROM workflow_waits w WHERE w.execution_id = e.id AND w.closed_at IS NULL)
      AND NOT EXISTS (
          SELECT 1 FROM tool_runs r
          WHERE r.workflow_execution_id = e.id
             OR r.agent_job_id IN (SELECT id FROM agent_jobs WHERE workflow_execution_id = e.id)
             OR r.clickup_draft_id IN (SELECT id FROM clickup_drafts WHERE execution_id = e.id)
      )
      AND NOT EXISTS (
          SELECT 1 FROM policy_decisions p
          WHERE p.execution_id = e.id
             OR p.approval_id IN (SELECT id FROM approvals WHERE execution_id = e.id)
      )
      AND NOT EXISTS (
          SELECT 1 FROM workflow_waits w JOIN approvals a ON a.id = w.approval_id
          WHERE w.execution_id = e.id AND a.decision IS NULL
      )
       AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.execution_id = e.id AND j.status IN ('pending', 'processing'))
       AND NOT EXISTS (
           SELECT 1 FROM agent_jobs a
           WHERE a.workflow_execution_id = e.id AND a.status IN ('pending', 'processing')
       )
       AND NOT EXISTS (SELECT 1 FROM workflow_signals s WHERE s.execution_id = e.id AND s.consumed_at IS NULL)
      AND NOT EXISTS (SELECT 1 FROM workflow_alerts a WHERE a.execution_id = e.id AND a.status = 'pending')
      AND NOT EXISTS (
          SELECT 1 FROM worker_jobs w
          WHERE w.execution_id = e.id AND w.state NOT IN ('SUCCEEDED', 'FAILED', 'CANCELLED')
      )
    ORDER BY e.finished_at, e.id
    LIMIT sqlc.arg(batch_size)
    FOR UPDATE OF e SKIP LOCKED
)
SELECT id FROM picked;

-- name: CountRetentionCandidates :one
SELECT
    (SELECT count(*) FROM message_logs m JOIN hub_threads t ON t.id = m.thread_id
     WHERE m.created_at < sqlc.arg(message_cutoff)
       AND (sqlc.narg(workspace_id)::uuid IS NULL OR t.workspace_id = sqlc.narg(workspace_id))
       AND t.summary_through_message_id IS DISTINCT FROM m.id
       AND NOT EXISTS (SELECT 1 FROM notification_outbox o WHERE o.message_id = m.id)
       AND NOT EXISTS (SELECT 1 FROM agent_jobs a WHERE a.thread_id = t.id AND a.status IN ('pending', 'processing'))) AS message_logs,
    (SELECT count(*) FROM tool_runs r WHERE r.created_at < sqlc.arg(tool_cutoff) AND r.status <> 'started'
       AND (sqlc.narg(workspace_id)::uuid IS NULL OR r.workspace_id = sqlc.narg(workspace_id))
       AND NOT EXISTS (SELECT 1 FROM policy_decisions p WHERE p.tool_run_id = r.id)
       AND NOT EXISTS (SELECT 1 FROM agent_jobs a WHERE a.id = r.agent_job_id AND a.status IN ('pending', 'processing'))
       AND NOT EXISTS (SELECT 1 FROM behavior_runs b WHERE b.id = r.behavior_run_id AND b.status IN ('pending', 'running'))
       AND NOT EXISTS (SELECT 1 FROM workflow_executions e WHERE e.id = r.workflow_execution_id AND e.state NOT IN ('COMPLETED', 'FAILED', 'CANCELLED'))
       AND NOT EXISTS (SELECT 1 FROM clickup_drafts d WHERE d.id = r.clickup_draft_id AND d.state IN ('pending', 'posting', 'failed'))) AS tool_runs,
    (SELECT count(*) FROM policy_decisions p WHERE p.created_at < sqlc.arg(policy_cutoff)
       AND (sqlc.narg(workspace_id)::uuid IS NULL OR p.workspace_id = sqlc.narg(workspace_id))
       AND NOT EXISTS (SELECT 1 FROM workflow_executions e WHERE e.id = p.execution_id AND e.state NOT IN ('COMPLETED', 'FAILED', 'CANCELLED'))
       AND NOT EXISTS (SELECT 1 FROM approvals a WHERE a.id = p.approval_id AND a.decision IS NULL)
       AND NOT EXISTS (SELECT 1 FROM tool_runs r WHERE r.id = p.tool_run_id AND r.status = 'started')) AS policy_decisions,
    (SELECT count(*) FROM workflow_executions e WHERE e.finished_at < sqlc.arg(workflow_cutoff)
       AND e.state IN ('COMPLETED', 'FAILED', 'CANCELLED')
       AND (sqlc.narg(workspace_id)::uuid IS NULL OR e.workspace_id = sqlc.narg(workspace_id))
       AND NOT EXISTS (SELECT 1 FROM workflow_executions c WHERE c.parent_execution_id = e.id)
       AND NOT EXISTS (SELECT 1 FROM approvals a WHERE a.execution_id = e.id AND a.decision IS NULL)
       AND NOT EXISTS (SELECT 1 FROM workflow_waits w WHERE w.execution_id = e.id AND w.closed_at IS NULL)
       AND NOT EXISTS (SELECT 1 FROM tool_runs r WHERE r.workflow_execution_id = e.id OR r.agent_job_id IN (SELECT id FROM agent_jobs WHERE workflow_execution_id = e.id) OR r.clickup_draft_id IN (SELECT id FROM clickup_drafts WHERE execution_id = e.id))
       AND NOT EXISTS (SELECT 1 FROM policy_decisions p WHERE p.execution_id = e.id OR p.approval_id IN (SELECT id FROM approvals WHERE execution_id = e.id))
       AND NOT EXISTS (SELECT 1 FROM workflow_waits w JOIN approvals a ON a.id = w.approval_id WHERE w.execution_id = e.id AND a.decision IS NULL)
       AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.execution_id = e.id AND j.status IN ('pending', 'processing'))
       AND NOT EXISTS (SELECT 1 FROM agent_jobs a WHERE a.workflow_execution_id = e.id AND a.status IN ('pending', 'processing'))
       AND NOT EXISTS (SELECT 1 FROM workflow_signals s WHERE s.execution_id = e.id AND s.consumed_at IS NULL)
       AND NOT EXISTS (SELECT 1 FROM workflow_alerts a WHERE a.execution_id = e.id AND a.status = 'pending')
       AND NOT EXISTS (SELECT 1 FROM worker_jobs w WHERE w.execution_id = e.id AND w.state NOT IN ('SUCCEEDED', 'FAILED', 'CANCELLED'))) AS workflow_executions,
    (SELECT count(*) FROM notification_outbox o WHERE o.updated_at < sqlc.arg(outbound_cutoff)
       AND o.status IN ('sent', 'failed')
       AND (sqlc.narg(workspace_id)::uuid IS NULL OR o.workspace_id = sqlc.narg(workspace_id))) AS notification_outbox,
    (SELECT count(*) FROM outbound_actions o WHERE o.updated_at < sqlc.arg(outbound_cutoff)
       AND o.status IN ('sent', 'failed')
       AND (sqlc.narg(workspace_id)::uuid IS NULL OR o.workspace_id = sqlc.narg(workspace_id))) AS outbound_actions;
