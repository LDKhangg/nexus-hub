package behavior

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// TC-B-27 (REQ-7): a model call refused by the daily budget is a distinct,
// non-busy, non-retryable assistant failure with a Vietnamese owner reply;
// other failures keep their own mapping.
func TestPhase12bAssistantMapsBudgetRefusal(t *testing.T) {
	budget := &domain.LLMError{Code: domain.LLMCodeBudgetExceeded}
	ctx := context.Background()
	if got := assistantErr(ctx, budget); !errors.Is(got, errAssistantBudget) {
		t.Fatalf("budget refusal mapped to %v", got)
	}
	if got := assistantErr(ctx, fmt.Errorf("wrapped: %w", budget)); !errors.Is(got, errAssistantBudget) {
		t.Fatalf("wrapped budget refusal mapped to %v", got)
	}
	// Even when the turn context is already done, the owner is told why.
	dead, cancel := context.WithCancel(ctx)
	cancel()
	if got := assistantErr(dead, budget); !errors.Is(got, errAssistantBudget) {
		t.Fatalf("budget refusal with done ctx mapped to %v", got)
	}
	if got := assistantErr(ctx, &domain.LLMError{Code: domain.LLMCodeError}); errors.Is(got, errAssistantBudget) {
		t.Fatal("generic llm_error must not look like a budget refusal")
	}
	if got := assistantErr(ctx, &domain.LLMError{Code: domain.LLMCodeUnavailable, Retryable: true}); !errors.Is(got, errAssistantBusy) {
		t.Fatalf("unavailable mapped to %v", got)
	}
	if got := assistantErr(ctx, domain.ErrLLMRefused); !errors.Is(got, errAssistantRefused) {
		t.Fatalf("safety refusal mapped to %v", got)
	}
	for _, want := range []string{"ngân sách", "llm_daily_budget_usd", "không dùng AI vẫn chạy", "0h UTC"} {
		if !strings.Contains(msgAssistantBudget, want) {
			t.Errorf("owner reply misses %q: %s", want, msgAssistantBudget)
		}
	}
	if len(msgAssistantBudget) > 300 {
		t.Error("owner reply too long")
	}
}

// TC-B-27 (REQ-7): the budget code is a registered workflow reason, so a
// plan can end with it.
func TestPhase12bBudgetCodeIsKnownWorkflowReason(t *testing.T) {
	for _, r := range KnownWorkflowReasons() {
		if string(r) == domain.LLMCodeBudgetExceeded {
			return
		}
	}
	t.Fatal("llm_budget_exceeded is not a known behavior workflow reason")
}
