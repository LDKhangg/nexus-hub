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
    payload jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(payload) = 'object' AND octet_length(payload::text) <= 16384),
    progress jsonb CHECK (progress IS NULL OR (jsonb_typeof(progress) = 'object' AND octet_length(progress::text) <= 1024)),
    progress_seq bigint NOT NULL DEFAULT 0 CHECK (progress_seq >= 0),
    leased_at timestamptz,
    interrupted_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    UNIQUE (workspace_id, id),
    FOREIGN KEY (workspace_id, execution_id) REFERENCES public.workflow_executions(workspace_id, id),
    FOREIGN KEY (workspace_id, device_id) REFERENCES public.worker_devices(workspace_id, id),
    CHECK (state IN ('WAITING_FOR_WORKER', 'CANCELLED') OR device_id IS NOT NULL)
);
CREATE INDEX worker_jobs_state_idx ON public.worker_jobs (workspace_id, state);
CREATE INDEX worker_jobs_waiting_idx ON public.worker_jobs (workspace_id, created_at, id) WHERE state = 'WAITING_FOR_WORKER';
CREATE INDEX worker_jobs_lease_expiry_idx ON public.worker_jobs (lease_expires_at) WHERE state IN ('LEASED', 'RUNNING', 'CANCELLING');
-- One job per device at a time (Phase 7a): a second active lease is a unique violation.
CREATE UNIQUE INDEX worker_jobs_one_active_per_device ON public.worker_jobs (device_id) WHERE state IN ('LEASED', 'RUNNING', 'CANCELLING');
CREATE INDEX worker_jobs_recent_idx ON public.worker_jobs (workspace_id, created_at DESC);
COMMENT ON TABLE public.worker_jobs IS 'Jobs leased to local workers; identifiers, fixed states and the structured result only.';
ALTER TABLE public.worker_jobs ENABLE ROW LEVEL SECURITY;

CREATE TABLE public.worker_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    device_id uuid,
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
CREATE INDEX worker_events_recent_idx ON public.worker_events (workspace_id, id DESC);
CREATE INDEX worker_events_job_idx ON public.worker_events (workspace_id, job_id, id DESC) WHERE job_id IS NOT NULL;

-- Latest telemetry snapshot per device (REQ-18): bounded numbers and fixed
-- codes only; no paths, process lists, hostnames or IP addresses.
CREATE TABLE public.worker_telemetry (
    device_id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    snapshot jsonb NOT NULL CHECK (jsonb_typeof(snapshot) = 'object' AND octet_length(snapshot::text) <= 4096),
    received_at timestamptz NOT NULL,
    FOREIGN KEY (workspace_id, device_id) REFERENCES public.worker_devices(workspace_id, id)
);
COMMENT ON TABLE public.worker_telemetry IS 'Latest bounded telemetry snapshot per worker device.';
ALTER TABLE public.worker_telemetry ENABLE ROW LEVEL SECURITY;

-- Hourly telemetry rollups, kept 30 days.
CREATE TABLE public.worker_telemetry_hourly (
    device_id uuid NOT NULL,
    hour timestamptz NOT NULL,
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    samples integer NOT NULL DEFAULT 0 CHECK (samples >= 0),
    cpu_load_sum double precision NOT NULL DEFAULT 0,
    cpu_load_max double precision,
    ram_used_pct_max double precision,
    disk_free_min_bytes bigint,
    claude_session_left_min smallint,
    claude_weekly_left_min smallint,
    jobs_total bigint,
    failures_total bigint,
    executor_tokens_total bigint,
    busy_seconds_total bigint,
    PRIMARY KEY (device_id, hour),
    FOREIGN KEY (workspace_id, device_id) REFERENCES public.worker_devices(workspace_id, id)
);
CREATE INDEX worker_telemetry_hourly_hour_idx ON public.worker_telemetry_hourly (hour);
COMMENT ON TABLE public.worker_telemetry_hourly IS 'Hourly worker telemetry rollups (30-day retention).';
ALTER TABLE public.worker_telemetry_hourly ENABLE ROW LEVEL SECURITY;

-- Local tasks (W-4): the owner's /task starts the built-in workflow
-- local_task, whose single worker.run step waits for one worker job. The
-- route row remembers where the topic messages go and what the job is for;
-- it holds ids only, never the instruction text.
ALTER TABLE public.approvals DROP CONSTRAINT approvals_kind_check;
ALTER TABLE public.approvals ADD CONSTRAINT approvals_kind_check
    CHECK (kind IN ('item_answer','post_draft','confirm_action','feedback','feedback_reason','contact_alias','assistant_action','workflow_target','worker_job'));
ALTER TABLE public.behaviors DROP CONSTRAINT behaviors_internal_kind_check;
ALTER TABLE public.behaviors ADD CONSTRAINT behaviors_internal_kind_check
    CHECK (internal_kind IS NULL OR internal_kind IN ('agent_comment_pipeline','local_task'));

CREATE TABLE public.worker_task_routes (
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    execution_id bigint NOT NULL,
    integration_id uuid NOT NULL,
    chat_id bigint NOT NULL,
    thread_id bigint NOT NULL DEFAULT 0,
    repo text NOT NULL CHECK (octet_length(repo) BETWEEN 3 AND 200),
    task_ref text NOT NULL DEFAULT '' CHECK (octet_length(task_ref) <= 64),
    clickup_task_id text NOT NULL DEFAULT '' CHECK (octet_length(clickup_task_id) <= 64),
    progress_message_id bigint NOT NULL DEFAULT 0,
    progress_job_attempt integer NOT NULL DEFAULT 0,
    waiting_notice text NOT NULL DEFAULT '' CHECK (octet_length(waiting_notice) <= 64),
    instruction_version bigint NOT NULL DEFAULT 0 CHECK (instruction_version >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, execution_id),
    FOREIGN KEY (workspace_id, execution_id) REFERENCES public.workflow_executions(workspace_id, id)
);
COMMENT ON TABLE public.worker_task_routes IS 'Where the topic messages of a local task go, and what it works on; ids and counters only.';
ALTER TABLE public.worker_task_routes ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE public.worker_task_routes;
DELETE FROM public.approvals WHERE kind IN ('workflow_target','worker_job');
ALTER TABLE public.approvals DROP CONSTRAINT approvals_kind_check;
ALTER TABLE public.approvals ADD CONSTRAINT approvals_kind_check
    CHECK (kind IN ('item_answer','post_draft','confirm_action','feedback','feedback_reason','contact_alias','assistant_action'));
ALTER TABLE public.behaviors DROP CONSTRAINT behaviors_internal_kind_check;
ALTER TABLE public.behaviors ADD CONSTRAINT behaviors_internal_kind_check
    CHECK (internal_kind IS NULL OR internal_kind IN ('agent_comment_pipeline'));
DROP TABLE public.worker_telemetry_hourly;
DROP TABLE public.worker_telemetry;
DROP TABLE public.worker_events;
DROP TABLE public.worker_jobs;
DROP TABLE public.worker_enroll_codes;
DROP TABLE public.worker_devices;
