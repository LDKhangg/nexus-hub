-- +goose Up
-- Durable reply prompts for owner-edited report templates. Template text is
-- staged here instead of in the Telegram update queue, whose arguments are
-- deliberately short and non-content-bearing.
CREATE TABLE public.report_template_prompts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL,
    workspace_id uuid NOT NULL,
    message_id bigint NOT NULL CHECK (message_id > 0),
    behavior_name text,
    kind text NOT NULL CHECK (kind IN ('pr_approve','pr_skipped','verdict_unavailable','mention','notify','fyi','digest','failure')),
    pending_body text,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (user_id, workspace_id) REFERENCES public.workspace_members(user_id, workspace_id) ON DELETE CASCADE,
    UNIQUE (workspace_id, message_id)
);
ALTER TABLE public.report_template_prompts ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE public.report_template_prompts;
