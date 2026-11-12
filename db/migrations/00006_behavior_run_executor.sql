-- +goose Up
-- Behavior run executor:
-- * a run can end as 'skipped' when a pre-check on the subject (for
--   example the assignee or list a trigger asked for) does not hold;
-- * the run keeps the context snapshot it gave the model (locator -> line
--   text, secrets redacted, bounded) so citations can be verified and
--   traced later;
-- * tool_runs can belong to a behavior run instead of an agent job, so a
--   behavior run's tool calls share the Phase 2 audit trail and write
--   idempotency without pretending to be agent jobs.
ALTER TABLE public.behavior_runs DROP CONSTRAINT behavior_runs_status_check;
ALTER TABLE public.behavior_runs ADD CONSTRAINT behavior_runs_status_check
    CHECK (status IN ('pending','running','done','failed','canceled','skipped'));
ALTER TABLE public.behavior_runs ADD COLUMN context_snapshot jsonb
    CHECK (context_snapshot IS NULL OR (jsonb_typeof(context_snapshot) = 'object' AND octet_length(context_snapshot::text) <= 1048576));
COMMENT ON COLUMN public.behavior_runs.context_snapshot IS 'Context fragments the model saw, by locator; secrets redacted; at most 512 KiB of text.';

ALTER TABLE public.tool_runs ALTER COLUMN agent_job_id DROP NOT NULL;
ALTER TABLE public.tool_runs ADD COLUMN behavior_run_id bigint REFERENCES public.behavior_runs(id);
ALTER TABLE public.tool_runs ADD CONSTRAINT tool_runs_one_owner CHECK (num_nonnulls(agent_job_id, behavior_run_id) = 1);
CREATE INDEX tool_runs_behavior_run_idx ON public.tool_runs(behavior_run_id) WHERE behavior_run_id IS NOT NULL;

-- +goose Down
DROP INDEX public.tool_runs_behavior_run_idx;
DELETE FROM public.tool_runs WHERE behavior_run_id IS NOT NULL;
ALTER TABLE public.tool_runs DROP CONSTRAINT tool_runs_one_owner;
ALTER TABLE public.tool_runs DROP COLUMN behavior_run_id;
ALTER TABLE public.tool_runs ALTER COLUMN agent_job_id SET NOT NULL;
ALTER TABLE public.behavior_runs DROP COLUMN context_snapshot;
UPDATE public.behavior_runs SET status = 'done' WHERE status = 'skipped';
ALTER TABLE public.behavior_runs DROP CONSTRAINT behavior_runs_status_check;
ALTER TABLE public.behavior_runs ADD CONSTRAINT behavior_runs_status_check
    CHECK (status IN ('pending','running','done','failed','canceled'));
