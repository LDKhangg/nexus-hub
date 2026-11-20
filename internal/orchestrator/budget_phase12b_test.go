package orchestrator

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// TC-B-27 (REQ-7): a plan whose model call is refused by the daily budget
// fails with llm_budget_exceeded (never retried, never a generic llm_error).
func TestPhase12bBudgetRefusalIsDetectedAndRegistered(t *testing.T) {
	budget := &domain.LLMError{Code: domain.LLMCodeBudgetExceeded}
	if !isBudgetExceeded(budget) || !isBudgetExceeded(fmt.Errorf("plan: %w", budget)) {
		t.Fatal("budget refusal not detected")
	}
	for _, err := range []error{nil, errors.New("llm_budget_exceeded"), &domain.LLMError{Code: domain.LLMCodeError}, domain.ErrLLMRefused} {
		if isBudgetExceeded(err) {
			t.Fatalf("%v detected as budget refusal", err)
		}
	}
	if ReasonLLMBudgetExceeded != domain.LLMCodeBudgetExceeded {
		t.Fatalf("reason %q differs from the provider code", ReasonLLMBudgetExceeded)
	}
	if !slices.Contains(fixedFailReasons, ReasonLLMBudgetExceeded) {
		t.Fatal("budget reason is not an allowed FAIL reason (it would end as orchestrator_failed)")
	}
	found := false
	for _, r := range KnownWorkflowReasons() {
		if string(r) == ReasonLLMBudgetExceeded {
			found = true
		}
	}
	if !found {
		t.Fatal("budget reason not registered with the workflow")
	}
}
