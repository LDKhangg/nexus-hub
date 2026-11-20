package agent

// TriageError is an agent LLM failure with a safe code. Codes:
// llm_unconfigured, llm_refused, llm_error, invalid_decision, token_budget.
type TriageError struct {
	Code      string
	Retryable bool
}

func (e *TriageError) Error() string { return "agent: " + e.Code }

const (
	CodeLLMUnconfigured = "llm_unconfigured"
	CodeLLMRefused      = "llm_refused"
	CodeLLMError        = "llm_error"
	// CodeLLMBudgetExceeded: llm_daily_budget_usd is spent for the UTC
	// day; the batch falls back to a plain notice without a model call.
	CodeLLMBudgetExceeded = "llm_budget_exceeded"
	CodeInvalidDecision   = "invalid_decision"
	// CodeTokenBudget means the job's token budget was already used up
	// before an LLM call; the job falls back without calling the model.
	CodeTokenBudget = StopTokenBudget
)
