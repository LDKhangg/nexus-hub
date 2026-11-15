-- Workspace-scoped conversation, binding and instruction persistence.

-- name: EnsureConversation :one
INSERT INTO public.conversations (workspace_id, integration_id, chat_id, user_id)
VALUES (sqlc.arg(workspace_id), sqlc.arg(integration_id), sqlc.arg(chat_id), sqlc.narg(user_id))
ON CONFLICT (workspace_id, integration_id, chat_id) DO UPDATE SET updated_at = public.conversations.updated_at
RETURNING *;

-- name: InsertConversationMessage :one
INSERT INTO public.conversation_messages
    (workspace_id, conversation_id, direction, kind, telegram_message_id, reply_to_message_id, execution_id)
VALUES (sqlc.arg(workspace_id), sqlc.arg(conversation_id), sqlc.arg(direction), sqlc.arg(kind),
    sqlc.arg(telegram_message_id), sqlc.narg(reply_to_message_id), sqlc.narg(execution_id))
RETURNING *;

-- name: BindConversationWorkflow :exec
INSERT INTO public.conversation_workflows (workspace_id, conversation_id, execution_id, source)
VALUES (sqlc.arg(workspace_id), sqlc.arg(conversation_id), sqlc.arg(execution_id), sqlc.arg(source))
ON CONFLICT (workspace_id, conversation_id, execution_id) DO NOTHING;

-- name: ListConversationWorkflowIDs :many
SELECT execution_id FROM public.conversation_workflows
WHERE workspace_id = sqlc.arg(workspace_id) AND conversation_id = sqlc.arg(conversation_id)
ORDER BY created_at, execution_id;

-- name: SetConversationSummary :one
UPDATE public.conversations
SET summary = sqlc.arg(summary), summary_updated_at = now(), updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id)
RETURNING summary;

-- name: GetConversationSummary :one
SELECT summary FROM public.conversations
WHERE workspace_id = sqlc.arg(workspace_id) AND id = sqlc.arg(id);

-- name: BindWorkflowResource :exec
INSERT INTO public.resource_bindings (workspace_id, execution_id, kind, external_id)
VALUES (sqlc.arg(workspace_id), sqlc.arg(execution_id), sqlc.arg(kind), sqlc.arg(external_id))
ON CONFLICT (workspace_id, kind, external_id, execution_id) DO NOTHING;

-- name: ListResourceExecutionIDs :many
SELECT execution_id FROM public.resource_bindings
WHERE workspace_id = sqlc.arg(workspace_id) AND kind = sqlc.arg(kind) AND external_id = sqlc.arg(external_id)
ORDER BY created_at, execution_id;

-- name: ListExecutionResources :many
SELECT workspace_id, execution_id, kind, external_id FROM public.resource_bindings
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id)
ORDER BY kind, created_at, external_id;

-- name: GetInstructionByTelegramUpdate :one
SELECT * FROM public.workflow_instructions
WHERE workspace_id = sqlc.arg(workspace_id) AND telegram_update_id = sqlc.arg(telegram_update_id);

-- name: InsertWorkflowInstruction :one
INSERT INTO public.workflow_instructions
    (workspace_id, execution_id, version, text, conversation_id, telegram_update_id)
VALUES (sqlc.arg(workspace_id), sqlc.arg(execution_id),
    (SELECT COALESCE(MAX(version), 0) + 1 FROM public.workflow_instructions
     WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id)),
    sqlc.arg(text), sqlc.arg(conversation_id), sqlc.arg(telegram_update_id))
ON CONFLICT (workspace_id, telegram_update_id) DO NOTHING
RETURNING *;

-- name: ListWorkflowInstructions :many
SELECT * FROM public.workflow_instructions
WHERE workspace_id = sqlc.arg(workspace_id) AND execution_id = sqlc.arg(execution_id)
ORDER BY version;
