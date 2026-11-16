-- +goose Up
-- Local worker bridge (Phase 7a). Devices authenticate by a random token
-- whose SHA-256 hash is stored; enrollment codes are single use and stored
-- hashed. Jobs and events hold identifiers, fixed codes and bounded JSON only:
-- never tokens, prompts, code or model output.
CREATE TABLE public.worker_devices (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    name text NOT NULL CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,39}$'),
    token_hash bytea NOT NULL CHECK (octet_length(token_hash) = 32),
    capabilities jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(capabilities) = 'array' AND octet_length(capabilities::text) <= 4096),
    state text NOT NULL DEFAULT 'offline' CHECK (state IN ('online', 'offline', 'busy')),
    last_seen_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    UNIQUE (workspace_id, id),
    UNIQUE (token_hash)
);
-- A revoked name may be enrolled again (lost laptop replaced).
CREATE UNIQUE INDEX worker_devices_active_name_key ON public.worker_devices (workspace_id, name) WHERE revoked_at IS NULL;
COMMENT ON TABLE public.worker_devices IS 'Enrolled local worker devices; only the SHA-256 hash of the device token is stored.';
ALTER TABLE public.worker_devices ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.worker_enroll_codes (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    code_hash bytea NOT NULL CHECK (octet_length(code_hash) = 32),
    expires_at timestamptz NOT NULL,
    used_at timestamptz,
    created_by text NOT NULL DEFAULT '' CHECK (octet_length(created_by) <= 128),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (code_hash)
);
COMMENT ON TABLE public.worker_enroll_codes IS 'One-time worker enrollment codes; only the SHA-256 hash is stored.';
ALTER TABLE public.worker_enroll_codes ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.worker_jobs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    execution_id bigint,
    step_id text NOT NULL DEFAULT '' CHECK (octet_length(step_id) <= 128),
    required_capabilities jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(required_capabilities) = 'array'),
    state text NOT NULL DEFAULT 'WAITING_FOR_WORKER' CHECK (state IN ('WAITING_FOR_WORKER', 'LEASED', 'RUNNING', 'CANCELLING', 'INTERRUPTED', 'SUCCEEDED', 'FAILED', 'NEEDS_INPUT', 'CANCELLED')),
    device_id uuid,
    lease_token_hash bytea CHECK (lease_token_hash IS NULL OR octet_length(lease_token_hash) = 32),
    lease_expires_at timestamptz,
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    result jsonb CHECK (result IS NULL OR jsonb_typeof(result) = 'object'),
    idempotency_key text CHECK (idempotency_key IS NULL OR octet_length(idempotency_key) <= 128),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    UNIQUE (workspace_id, id),
    FOREIGN KEY (workspace_id, execution_id) REFERENCES public.workflow_executions(workspace_id, id),
    FOREIGN KEY (workspace_id, device_id) REFERENCES public.worker_devices(workspace_id, id)
);
CREATE INDEX worker_jobs_state_idx ON public.worker_jobs (workspace_id, state);
COMMENT ON TABLE public.worker_jobs IS 'Jobs leased to local workers; identifiers, fixed states and the structured result only.';
ALTER TABLE public.worker_jobs ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.worker_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    device_id uuid NOT NULL,
    job_id uuid,
    kind text NOT NULL CHECK (kind ~ '^[a-z][a-z0-9_]{0,63}$'),
    reason_code text NOT NULL DEFAULT '' CHECK (reason_code = '' OR reason_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (workspace_id, device_id) REFERENCES public.worker_devices(workspace_id, id),
    FOREIGN KEY (workspace_id, job_id) REFERENCES public.worker_jobs(workspace_id, id)
);
CREATE INDEX worker_events_device_idx ON public.worker_events (workspace_id, device_id, id DESC);
COMMENT ON TABLE public.worker_events IS 'Worker audit trail: fixed kind and reason codes only, no free text.';
ALTER TABLE public.worker_events ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE public.worker_events;
DROP TABLE public.worker_jobs;
DROP TABLE public.worker_enroll_codes;
DROP TABLE public.worker_devices;
