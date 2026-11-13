-- +goose Up
-- ClickUp Chat poll state, one row per ClickUp source integration:
-- * cursor_ms is the newest message date (Unix ms) the poller fully
--   processed; it only moves forward. The row is created on the first poll
--   with the cursor at "now", so older messages are never processed.
-- * clickup_team_id is the ClickUp workspace the cursor belongs to; when the
--   integration is pointed at another ClickUp workspace the cursor starts
--   again at "now".
-- * error_streak and backoff_until drive the doubling backoff;
--   unauthorized_streak counts consecutive chat_unauthorized results and
--   unauthorized_notified_at records the one owner notice of that streak.
-- The row holds identifiers, counters and times only, never message text.
CREATE TABLE public.clickup_chat_polls (
    workspace_id uuid NOT NULL,
    integration_id uuid NOT NULL,
    clickup_team_id text NOT NULL CHECK (btrim(clickup_team_id) <> '' AND octet_length(clickup_team_id) <= 64),
    cursor_ms bigint NOT NULL CHECK (cursor_ms >= 0),
    initialized_at timestamptz NOT NULL DEFAULT now(),
    error_streak integer NOT NULL DEFAULT 0 CHECK (error_streak >= 0),
    unauthorized_streak integer NOT NULL DEFAULT 0 CHECK (unauthorized_streak >= 0),
    last_error_code text CHECK (last_error_code IS NULL OR last_error_code ~ '^[a-z_]{1,64}$'),
    backoff_until timestamptz,
    unauthorized_notified_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, integration_id),
    FOREIGN KEY (workspace_id, integration_id) REFERENCES public.integrations(workspace_id, id) ON DELETE CASCADE,
    CHECK (unauthorized_streak <= error_streak)
);
COMMENT ON TABLE public.clickup_chat_polls IS 'ClickUp Chat poller cursor and error state per source integration; identifiers and counters only.';
ALTER TABLE public.clickup_chat_polls ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE public.clickup_chat_polls;
