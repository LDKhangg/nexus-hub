-- +goose Up
CREATE TABLE public.notification_outbox (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    source_integration_id uuid NOT NULL,
    target_integration_id uuid NOT NULL,
    event_key text NOT NULL CHECK (btrim(event_key) <> ''),
    event jsonb NOT NULL CHECK (jsonb_typeof(event) = 'object'),
    message_id bigint NOT NULL REFERENCES public.message_logs(id),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','processing','sent','failed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    locked_until timestamptz,
    claim_token uuid,
    telegram_message_id bigint,
    last_error_code text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    sent_at timestamptz,
    FOREIGN KEY (workspace_id,source_integration_id) REFERENCES public.integrations(workspace_id,id),
    FOREIGN KEY (workspace_id,target_integration_id) REFERENCES public.integrations(workspace_id,id),
    UNIQUE (source_integration_id,event_key,target_integration_id)
);
CREATE INDEX notification_outbox_due_idx ON public.notification_outbox(status,next_attempt_at,locked_until);
ALTER TABLE public.notification_outbox ENABLE ROW LEVEL SECURITY;
COMMENT ON TABLE public.notification_outbox IS 'Backend-only durable notices. Claims are fenced; ambiguous remote delivery can duplicate.';

-- +goose Down
DROP TABLE public.notification_outbox;
