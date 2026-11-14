-- +goose Up
-- Phase 4 people directory, report preferences and the durable owner identity.
CREATE TABLE public.users (
    id uuid PRIMARY KEY,
    display_name text NOT NULL CHECK (btrim(display_name) <> ''),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE public.users ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.workspace_members (
    user_id uuid NOT NULL REFERENCES public.users(id) ON DELETE CASCADE,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id) ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('owner','member')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, workspace_id)
);
ALTER TABLE public.workspace_members ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.contacts (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id) ON DELETE CASCADE,
    display_name text,
    note text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, id)
);
ALTER TABLE public.contacts ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.contact_accounts (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id) ON DELETE CASCADE,
    contact_id uuid NOT NULL,
    platform text NOT NULL CHECK (platform IN ('clickup','github','telegram')),
    external_id text NOT NULL CHECK (btrim(external_id) <> ''),
    platform_name text,
    username text,
    source text NOT NULL CHECK (source IN ('auto','manual')),
    is_self boolean NOT NULL DEFAULT false,
    first_seen timestamptz NOT NULL DEFAULT now(),
    last_seen timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, contact_id) REFERENCES public.contacts(workspace_id, id) ON DELETE CASCADE,
    UNIQUE (workspace_id, platform, external_id)
);
CREATE INDEX contact_accounts_contact_idx ON public.contact_accounts(workspace_id, contact_id);
ALTER TABLE public.contact_accounts ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.report_preferences (
    user_id uuid NOT NULL,
    workspace_id uuid NOT NULL,
    behavior_name text,
    style text NOT NULL CHECK (style IN ('short','normal','detailed')),
    show_reaction boolean NOT NULL,
    show_ai_summary boolean NOT NULL,
    show_links boolean NOT NULL,
    show_pr_titles boolean NOT NULL,
    show_trace_hint boolean NOT NULL,
    language text CHECK (language IN ('vi','en')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (user_id, workspace_id) REFERENCES public.workspace_members(user_id, workspace_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX report_preferences_scope_idx
    ON public.report_preferences(user_id, workspace_id, COALESCE(behavior_name, ''));
ALTER TABLE public.report_preferences ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.report_templates (
    user_id uuid NOT NULL,
    workspace_id uuid NOT NULL,
    behavior_name text,
    kind text NOT NULL CHECK (kind IN ('pr_approve','pr_skipped','verdict_unavailable','mention','notify','fyi','digest','failure')),
    body text NOT NULL CHECK (char_length(body) <= 1000),
    updated_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (user_id, workspace_id) REFERENCES public.workspace_members(user_id, workspace_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX report_templates_scope_kind_idx
    ON public.report_templates(user_id, workspace_id, COALESCE(behavior_name, ''), kind);
ALTER TABLE public.report_templates ENABLE ROW LEVEL SECURITY;

-- The UUIDs below are stable so reapplying the bootstrap cannot duplicate the
-- one owner or the owner's per-workspace contact. Settings are optional, and
-- an empty database therefore remains valid.
INSERT INTO public.users(id, display_name, status)
VALUES ('00000000-0000-0000-0000-000000000001', 'Owner', 'active')
ON CONFLICT (id) DO NOTHING;

INSERT INTO public.workspace_members(user_id, workspace_id, role)
SELECT '00000000-0000-0000-0000-000000000001', id, 'owner'
FROM public.workspaces
ON CONFLICT (user_id, workspace_id) DO UPDATE SET role = 'owner', updated_at = now();

WITH owner_ids AS (
    SELECT
        COALESCE(
            (SELECT value #>> '{}' FROM public.app_settings WHERE key = 'owner_telegram_user_id' AND (value #>> '{}') ~ '^[1-9][0-9]*$'),
            (SELECT value #>> '{}' FROM public.app_settings WHERE key = 'owner_telegram_chat_id' AND (value #>> '{}') ~ '^[1-9][0-9]*$')
        ) AS telegram_id,
        (SELECT value #>> '{}' FROM public.app_settings WHERE key = 'owner_clickup_user_id' AND (value #>> '{}') ~ '^[0-9]+$') AS clickup_id
), github_logins AS (
    SELECT DISTINCT i.workspace_id, account.value ->> 'login' AS login
    FROM public.integrations i
    CROSS JOIN LATERAL jsonb_each(COALESCE(i.config #> '{agent,tools,github,accounts}', '{}'::jsonb)) AS account
    WHERE NULLIF(btrim(account.value ->> 'login'), '') IS NOT NULL
), owner_workspaces AS (
    SELECT w.id AS workspace_id
    FROM public.workspaces w
    CROSS JOIN owner_ids o
    WHERE o.telegram_id IS NOT NULL OR o.clickup_id IS NOT NULL
       OR EXISTS (SELECT 1 FROM github_logins g WHERE g.workspace_id = w.id)
)
INSERT INTO public.contacts(id, workspace_id)
SELECT md5('owner-contact:' || workspace_id::text)::uuid, workspace_id
FROM owner_workspaces
ON CONFLICT (id) DO NOTHING;

WITH owner_ids AS (
    SELECT
        COALESCE(
            (SELECT value #>> '{}' FROM public.app_settings WHERE key = 'owner_telegram_user_id' AND (value #>> '{}') ~ '^[1-9][0-9]*$'),
            (SELECT value #>> '{}' FROM public.app_settings WHERE key = 'owner_telegram_chat_id' AND (value #>> '{}') ~ '^[1-9][0-9]*$')
        ) AS telegram_id,
        (SELECT value #>> '{}' FROM public.app_settings WHERE key = 'owner_clickup_user_id' AND (value #>> '{}') ~ '^[0-9]+$') AS clickup_id
), owner_accounts AS (
    SELECT w.id AS workspace_id, 'telegram'::text AS platform, o.telegram_id AS external_id, NULL::text AS username
    FROM public.workspaces w CROSS JOIN owner_ids o WHERE o.telegram_id IS NOT NULL
    UNION ALL
    SELECT w.id, 'clickup', o.clickup_id, NULL::text
    FROM public.workspaces w CROSS JOIN owner_ids o WHERE o.clickup_id IS NOT NULL
    UNION
    SELECT i.workspace_id, 'github', account.value ->> 'login', account.value ->> 'login'
    FROM public.integrations i
    CROSS JOIN LATERAL jsonb_each(COALESCE(i.config #> '{agent,tools,github,accounts}', '{}'::jsonb)) AS account
    WHERE NULLIF(btrim(account.value ->> 'login'), '') IS NOT NULL
)
INSERT INTO public.contact_accounts(
    id, workspace_id, contact_id, platform, external_id, username, source, is_self
)
SELECT md5('owner-account:' || workspace_id::text || ':' || platform || ':' || external_id)::uuid,
       workspace_id, md5('owner-contact:' || workspace_id::text)::uuid,
       platform, external_id, username, 'auto', true
FROM owner_accounts
ON CONFLICT (workspace_id, platform, external_id)
DO UPDATE SET is_self = true, last_seen = now();

-- +goose Down
DROP TABLE public.report_templates;
DROP TABLE public.report_preferences;
DROP TABLE public.contact_accounts;
DROP TABLE public.contacts;
DROP TABLE public.workspace_members;
DROP TABLE public.users;
