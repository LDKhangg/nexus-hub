-- +goose Up
-- Phase 10 REQ-11: an orchestrated execution that waits for a local worker
-- job is in WAITING_FOR_WORKER (no lease, like the other waiting states),
-- not WAITING_FOR_USER. Only the execution state vocabulary changes.
ALTER TABLE public.workflow_executions DROP CONSTRAINT workflow_executions_state_check;
ALTER TABLE public.workflow_executions ADD CONSTRAINT workflow_executions_state_check
    CHECK (state IN ('RECEIVED','TRIAGING','PLANNING','QUEUED','RUNNING','EVALUATING','WAITING_FOR_USER','WAITING_FOR_APPROVAL','WAITING_FOR_WORKER','COMPLETED','FAILED','CANCELLED'));
ALTER TABLE public.execution_events DROP CONSTRAINT execution_events_from_state_check;
ALTER TABLE public.execution_events ADD CONSTRAINT execution_events_from_state_check
    CHECK (from_state IS NULL OR from_state IN ('RECEIVED','TRIAGING','PLANNING','QUEUED','RUNNING','EVALUATING','WAITING_FOR_USER','WAITING_FOR_APPROVAL','WAITING_FOR_WORKER','COMPLETED','FAILED','CANCELLED'));
ALTER TABLE public.execution_events DROP CONSTRAINT execution_events_to_state_check;
ALTER TABLE public.execution_events ADD CONSTRAINT execution_events_to_state_check
    CHECK (to_state IS NULL OR to_state IN ('RECEIVED','TRIAGING','PLANNING','QUEUED','RUNNING','EVALUATING','WAITING_FOR_USER','WAITING_FOR_APPROVAL','WAITING_FOR_WORKER','COMPLETED','FAILED','CANCELLED'));

-- +goose Down
-- Executions waiting for a worker go back to WAITING_FOR_USER (the 7b
-- wait); their history rows are rewritten the same way, so the old CHECKs
-- hold again.
UPDATE public.workflow_executions SET state = 'WAITING_FOR_USER' WHERE state = 'WAITING_FOR_WORKER';
ALTER TABLE public.execution_events DISABLE TRIGGER execution_events_no_update;
UPDATE public.execution_events SET from_state = 'WAITING_FOR_USER' WHERE from_state = 'WAITING_FOR_WORKER';
UPDATE public.execution_events SET to_state = 'WAITING_FOR_USER' WHERE to_state = 'WAITING_FOR_WORKER';
ALTER TABLE public.execution_events ENABLE TRIGGER execution_events_no_update;
ALTER TABLE public.execution_events DROP CONSTRAINT execution_events_to_state_check;
ALTER TABLE public.execution_events ADD CONSTRAINT execution_events_to_state_check
    CHECK (to_state IS NULL OR to_state IN ('RECEIVED','TRIAGING','PLANNING','QUEUED','RUNNING','EVALUATING','WAITING_FOR_USER','WAITING_FOR_APPROVAL','COMPLETED','FAILED','CANCELLED'));
ALTER TABLE public.execution_events DROP CONSTRAINT execution_events_from_state_check;
ALTER TABLE public.execution_events ADD CONSTRAINT execution_events_from_state_check
    CHECK (from_state IS NULL OR from_state IN ('RECEIVED','TRIAGING','PLANNING','QUEUED','RUNNING','EVALUATING','WAITING_FOR_USER','WAITING_FOR_APPROVAL','COMPLETED','FAILED','CANCELLED'));
ALTER TABLE public.workflow_executions DROP CONSTRAINT workflow_executions_state_check;
ALTER TABLE public.workflow_executions ADD CONSTRAINT workflow_executions_state_check
    CHECK (state IN ('RECEIVED','TRIAGING','PLANNING','QUEUED','RUNNING','EVALUATING','WAITING_FOR_USER','WAITING_FOR_APPROVAL','COMPLETED','FAILED','CANCELLED'));
