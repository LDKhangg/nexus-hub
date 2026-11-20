-- +goose Up
-- Phase 12 B2: dedup of the Telegram alerts for stuck and failed workflow
-- executions. One row per (execution, alert state): the alert watcher
-- inserts it first and sends only when the insert created the row, so the
-- same execution and state never alerts twice, across restarts too. A send
-- that failed deletes its row, so the next tick retries it. Ids, a state
-- and a time only: never message text, prompts or payloads.
CREATE TABLE public.workflow_alerts (
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    execution_id bigint NOT NULL,
    state text NOT NULL CHECK (state IN ('stuck_running','stuck_queued','approval_overdue','failed')),
    sent_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, execution_id, state),
    FOREIGN KEY (workspace_id, execution_id) REFERENCES public.workflow_executions(workspace_id, id)
);
CREATE INDEX workflow_alerts_sent_idx ON public.workflow_alerts (sent_at);
COMMENT ON TABLE public.workflow_alerts IS 'One Telegram alert per workflow execution and alert state (dedup); ids, state and time only.';
COMMENT ON COLUMN public.workflow_alerts.sent_at IS 'When the alert was claimed; a row of an alert merged into the hourly summary (rate cap) is kept too.';
ALTER TABLE public.workflow_alerts ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE public.workflow_alerts;
