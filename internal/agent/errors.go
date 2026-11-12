package agent

// TriageError is an agent LLM failure with a safe code. Codes:
// llm_unconfigured, llm_refused, llm_error, invalid_decision.
type TriageError struct {
	Code      string
	Retryable bool
}

func (e *TriageError) Error() string { return "agent: " + e.Code }

const (
	CodeLLMUnconfigured = "llm_unconfigured"
	CodeLLMRefused      = "llm_refused"
	CodeLLMError        = "llm_error"
	CodeInvalidDecision = "invalid_decision"
)
