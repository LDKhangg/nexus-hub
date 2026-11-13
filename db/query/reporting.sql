-- name: GetReportPreferences :one
SELECT * FROM public.report_preferences
WHERE user_id = sqlc.arg(user_id)::uuid
  AND workspace_id = sqlc.arg(workspace_id)::uuid
  AND behavior_name IS NOT DISTINCT FROM sqlc.narg(behavior_name)::text;

-- name: ResolveReportPreferences :one
SELECT * FROM public.report_preferences
WHERE user_id = sqlc.arg(user_id)::uuid
  AND workspace_id = sqlc.arg(workspace_id)::uuid
  AND (behavior_name = sqlc.narg(behavior_name)::text OR behavior_name IS NULL)
ORDER BY (behavior_name IS NOT NULL) DESC
LIMIT 1;

-- name: SetReportPreferences :one
INSERT INTO public.report_preferences(
  user_id, workspace_id, behavior_name, style, show_reaction, show_ai_summary,
  show_links, show_pr_titles, show_trace_hint, language
) VALUES (
  sqlc.arg(user_id)::uuid, sqlc.arg(workspace_id)::uuid,
  sqlc.narg(behavior_name)::text, sqlc.arg(style), sqlc.arg(show_reaction)::boolean,
  sqlc.arg(show_ai_summary)::boolean, sqlc.arg(show_links)::boolean,
  sqlc.arg(show_pr_titles)::boolean, sqlc.arg(show_trace_hint)::boolean,
  sqlc.narg(language)::text
)
ON CONFLICT (user_id, workspace_id, COALESCE(behavior_name, '')) DO UPDATE SET
  style = EXCLUDED.style, show_reaction = EXCLUDED.show_reaction,
  show_ai_summary = EXCLUDED.show_ai_summary, show_links = EXCLUDED.show_links,
  show_pr_titles = EXCLUDED.show_pr_titles, show_trace_hint = EXCLUDED.show_trace_hint,
  language = EXCLUDED.language, updated_at = now()
RETURNING *;

-- name: GetReportTemplate :one
SELECT * FROM public.report_templates
WHERE user_id = sqlc.arg(user_id)::uuid
  AND workspace_id = sqlc.arg(workspace_id)::uuid
  AND behavior_name IS NOT DISTINCT FROM sqlc.narg(behavior_name)::text
  AND kind = sqlc.arg(kind);

-- name: ResolveReportTemplate :one
SELECT * FROM public.report_templates
WHERE user_id = sqlc.arg(user_id)::uuid
  AND workspace_id = sqlc.arg(workspace_id)::uuid
  AND kind = sqlc.arg(kind)
  AND (behavior_name = sqlc.narg(behavior_name)::text OR behavior_name IS NULL)
ORDER BY (behavior_name IS NOT NULL) DESC
LIMIT 1;

-- name: SetReportTemplate :one
INSERT INTO public.report_templates(user_id, workspace_id, behavior_name, kind, body)
VALUES (sqlc.arg(user_id)::uuid, sqlc.arg(workspace_id)::uuid,
  sqlc.narg(behavior_name)::text, sqlc.arg(kind), sqlc.arg(body))
ON CONFLICT (user_id, workspace_id, COALESCE(behavior_name, ''), kind) DO UPDATE SET
  body = EXCLUDED.body, updated_at = now()
RETURNING *;

-- name: DeleteReportTemplate :execrows
DELETE FROM public.report_templates
WHERE user_id = sqlc.arg(user_id)::uuid
  AND workspace_id = sqlc.arg(workspace_id)::uuid
  AND behavior_name IS NOT DISTINCT FROM sqlc.narg(behavior_name)::text
  AND kind = sqlc.arg(kind);
