-- +goose Up
-- Phase 12 B1: the LLM spend ledger. One row per workspace, UTC day,
-- provider and model, upserted after every model call that reports usage.
-- Token counts and an estimated USD cost only: never prompts, answers or
-- credentials. llm_daily_budget_usd is enforced from this table.
CREATE TABLE public.llm_usage_daily (
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id),
    day date NOT NULL,
    provider text NOT NULL CHECK (provider ~ '^[a-z][a-z0-9_]{0,31}$'),
    model text NOT NULL CHECK (model ~ '^[A-Za-z0-9][A-Za-z0-9._:/#@-]{0,127}$'),
    input_tokens bigint NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens bigint NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    cost_usd double precision NOT NULL DEFAULT 0 CHECK (cost_usd >= 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, day, provider, model)
);
CREATE INDEX llm_usage_daily_day_idx ON public.llm_usage_daily (day);
COMMENT ON TABLE public.llm_usage_daily IS 'Workspace-scoped daily LLM token and estimated cost ledger; counts only, never content.';
COMMENT ON COLUMN public.llm_usage_daily.input_tokens IS 'All prompt tokens of the day, cache reads and writes included.';
COMMENT ON COLUMN public.llm_usage_daily.cost_usd IS 'Estimate from the hub price table (internal/adapters/llm/pricing.go); 0 for unknown models and subscription CLI providers.';
ALTER TABLE public.llm_usage_daily ENABLE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE public.llm_usage_daily;
