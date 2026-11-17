-- AI orchestrator (Phase 7b): sanitized plans, plan tasks and decisions.

-- name: InsertOrchestratorPlan :one
INSERT INTO public.orchestrator_plans (workspace_id, execution_id, version, schema_version, intent, task_type, priority, complexity, confidence, budget, tokens_in, tokens_out, cost_usd)
VALUES (sqlc.arg(workspace_id), sqlc.arg(execution_id), sqlc.arg(version), sqlc.arg(schema_version), sqlc.arg(intent), sqlc.arg(task_type),
        sqlc.arg(priority), sqlc.arg(complexity), sqlc.arg(confidence), sqlc.arg(budget), sqlc.arg(tokens_in), sqlc.arg(tokens_out), sqlc.arg(cost_usd))
RETURNING *;

-- name: InsertOrchestratorPlanTask :one
INSERT INTO public.orchestrator_plan_tasks (workspace_id, plan_id, key, title, capabilities, depends_on, priority, complexity, executor)
VALUES (sqlc.arg(workspace_id), sqlc.arg(plan_id), sqlc.arg(key), sqlc.arg(title), sqlc.arg(capabilities)::text[], sqlc.arg(depends_on)::text[],
        sqlc.arg(priority), sqlc.arg(complexity), sqlc.arg(executor))
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
