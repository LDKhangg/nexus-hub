-- AI orchestrator (Phase 7b): sanitized plans, plan tasks and decisions.

-- name: InsertOrchestratorPlan :one
INSERT INTO public.orchestrator_plans (workspace_id, execution_id, version, schema_version, intent, task_type, priority, complexity, confidence, budget, tokens_in, tokens_out, cost_usd)
VALUES (sqlc.arg(workspace_id), sqlc.arg(execution_id), sqlc.arg(version), sqlc.arg(schema_version), sqlc.arg(intent), sqlc.arg(task_type),
        sqlc.arg(priority), sqlc.arg(complexity), sqlc.arg(confidence), sqlc.arg(budget), sqlc.arg(tokens_in), sqlc.arg(tokens_out), sqlc.arg(cost_usd))
RETURNING *;

-- name: InsertOrchestratorPlanTask :one
INSERT INTO public.orchestrator_plan_tasks (workspace_id, plan_id, key, title, capabilities, depends_on, priority, complexity, executor,
                                            state, result_step_id, result, evidence_refs, merge_target)
VALUES (sqlc.arg(workspace_id), sqlc.arg(plan_id), sqlc.arg(key), sqlc.arg(title), sqlc.arg(capabilities)::text[], sqlc.arg(depends_on)::text[],
        sqlc.arg(priority), sqlc.arg(complexity), sqlc.arg(executor),
        sqlc.arg(state), sqlc.arg(result_step_id), sqlc.arg(result), sqlc.arg(evidence_refs), sqlc.narg(merge_target))
RETURNING *;

-- name: SetOrchestratorPlanStatus :one
UPDATE public.orchestrator_plans
SET status = sqlc.arg(status)
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id) AND version = sqlc.arg(version)
RETURNING *;

-- name: UpdateOrchestratorPlanTask :one
UPDATE public.orchestrator_plan_tasks
SET state = sqlc.arg(state), executor = sqlc.arg(executor), result_step_id = sqlc.arg(result_step_id),
    result = sqlc.arg(result), evidence_refs = sqlc.arg(evidence_refs), updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND plan_id = sqlc.arg(plan_id) AND key = sqlc.arg(key)
RETURNING *;

-- name: InsertOrchestratorDecision :one
INSERT INTO public.orchestrator_decisions (workspace_id, execution_id, plan_version, task_key, decision, reason_code, converted_from, evidence_refs)
VALUES (sqlc.arg(workspace_id), sqlc.arg(execution_id), sqlc.arg(plan_version), sqlc.arg(task_key), sqlc.arg(decision),
        sqlc.arg(reason_code), sqlc.arg(converted_from), sqlc.arg(evidence_refs))
RETURNING *;

-- name: ListOrchestratorPlans :many
SELECT * FROM public.orchestrator_plans
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id)
ORDER BY version;

-- name: ListOrchestratorPlanTasks :many
SELECT t.* FROM public.orchestrator_plan_tasks t
JOIN public.orchestrator_plans p ON p.workspace_id = t.workspace_id AND p.id = t.plan_id
WHERE p.workspace_id = sqlc.arg(workspace_id) AND p.execution_id = sqlc.arg(execution_id)
ORDER BY p.version, t.id;

-- name: ListOrchestratorDecisions :many
SELECT * FROM public.orchestrator_decisions
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id)
ORDER BY id;

-- name: InsertOrchestratorRoute :exec
INSERT INTO public.orchestrator_routes (workspace_id, execution_id, integration_id, chat_id, thread_id, repo, task_ref, clickup_task_id)
VALUES (sqlc.arg(workspace_id), sqlc.arg(execution_id), sqlc.arg(integration_id), sqlc.arg(chat_id), sqlc.arg(thread_id),
        sqlc.arg(repo), sqlc.arg(task_ref), sqlc.arg(clickup_task_id))
ON CONFLICT (workspace_id, execution_id) DO NOTHING;

-- name: GetOrchestratorRoute :one
SELECT * FROM public.orchestrator_routes
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id);

-- name: ListOrchestratorStepsToWake :many
-- Waiting orchestrated task run steps that must run again without a
-- signal (Phase 10 REQ-10/11): the wall-time budget of a worker wait is
-- over (the step's orchestrator_wake_at reference, unix seconds); the
-- worker job the step waits on for the owner (job_owner:<id>) was retried
-- and runs again; or the ClickUp draft the step waits on (task:draft:<id>)
-- has ended (posted, cancelled, expired or failed_final); or the policy
-- gate the step waits on (task:gate:<id>, Phase 9) was decided or expired.
-- Executions past their deadline are left to the reconciler, and one with
-- a live queue row is already running.
SELECT s.workspace_id, s.execution_id, s.step_key
FROM public.execution_steps s
JOIN public.workflow_executions e ON e.workspace_id = s.workspace_id AND e.id = s.execution_id
WHERE s.kind = 'orchestrator.run_task' AND s.state = 'WAITING'
  AND e.state IN ('WAITING_FOR_USER', 'WAITING_FOR_APPROVAL', 'WAITING_FOR_WORKER')
  AND (e.deadline_at IS NULL OR e.deadline_at > sqlc.arg(at_time)::timestamptz)
  AND (
    COALESCE(jsonb_path_exists(s.checkpoint,
        '$.references[*] ? (@.kind == "orchestrator_wake_at" && @.reference_id.double() <= $now)',
        jsonb_build_object('now', sqlc.arg(now_unix)::bigint), true), false)
    OR EXISTS (SELECT 1 FROM public.worker_jobs j
               WHERE j.workspace_id = s.workspace_id AND j.execution_id = s.execution_id AND j.step_id = s.step_key
                 AND j.state IN ('WAITING_FOR_WORKER', 'LEASED', 'RUNNING')
                 AND COALESCE(jsonb_path_exists(s.checkpoint,
                     '$.references[*] ? (@.kind == "orchestrator_wait" && @.reference_id == $ref)',
                     jsonb_build_object('ref', 'job_owner:' || j.id::text), true), false))
    OR EXISTS (SELECT 1 FROM public.clickup_drafts d
               WHERE d.workspace_id = s.workspace_id AND d.execution_id = s.execution_id
                 AND d.state IN ('posted', 'cancelled', 'expired', 'failed_final')
                 AND COALESCE(jsonb_path_exists(s.checkpoint,
                     '$.references[*] ? (@.kind == "orchestrator_wait" && @.reference_id == $ref)',
                     jsonb_build_object('ref', 'task:draft:' || d.id::text), true), false))
    OR EXISTS (SELECT 1 FROM public.approvals a
               WHERE a.workspace_id = s.workspace_id AND a.execution_id = s.execution_id
                 AND a.kind = 'policy_gate' AND a.decision IS NOT NULL
                 AND COALESCE(jsonb_path_exists(s.checkpoint,
                     '$.references[*] ? (@.kind == "orchestrator_wait" && @.reference_id == $ref)',
                     jsonb_build_object('ref', 'task:gate:' || a.id::text), true), false))
  )
  AND NOT EXISTS (SELECT 1 FROM public.jobs q
                  WHERE q.workspace_id = e.workspace_id AND q.execution_id = e.id AND q.status IN ('pending', 'processing'))
ORDER BY s.updated_at
LIMIT sqlc.arg(row_limit);
