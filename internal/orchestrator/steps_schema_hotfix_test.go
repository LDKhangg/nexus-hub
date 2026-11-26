package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/obs"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

type hotfixHTTPError struct {
	status    int
	retryable bool
}

func (e *hotfixHTTPError) Error() string       { return "PROVIDER-MESSAGE" }
func (e *hotfixHTTPError) HTTPStatusCode() int { return e.status }
func (e *hotfixHTTPError) IsRetryable() bool   { return e.retryable }

// TC-HS-4 (REQ-3): Plan and Evaluate failures each produce one safe JSON
// record with stable classification and no request, schema, or provider text.
func TestStepsFlow_LLMFailuresLogOnlySafeMetadata(t *testing.T) {
	oldDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(oldDefault) })

	cases := []struct {
		name       string
		step       string
		err        error
		wantCode   string
		retryable  bool
		failReason string
		budgetGate bool
	}{
		{name: "plan status", step: StepKeyPlan, err: &hotfixHTTPError{status: 400}, wantCode: "llm_status_400"},
		{name: "evaluate status", step: "eval_01", err: &hotfixHTTPError{status: 503, retryable: true}, wantCode: "llm_status_503", retryable: true},
		{name: "plan invalid output", step: StepKeyPlan, err: fmt.Errorf("PROVIDER-MESSAGE: %w", ErrInvalidOutput), wantCode: "llm_invalid_output", failReason: ReasonInvalidOutput},
		{name: "evaluate invalid output", step: "eval_01", err: fmt.Errorf("PROVIDER-MESSAGE: %w", ErrInvalidOutput), wantCode: "llm_invalid_output", failReason: ReasonInvalidOutput},
		{name: "plan budget", step: StepKeyPlan, err: fmt.Errorf("PROVIDER-MESSAGE: %w", &BudgetExceededError{Kind: BudgetTokens}), wantCode: "llm_budget", budgetGate: true},
		{name: "evaluate budget", step: "eval_01", err: fmt.Errorf("PROVIDER-MESSAGE: %w", &domain.LLMError{Code: domain.LLMCodeBudgetExceeded}), wantCode: "llm_budget", failReason: ReasonLLMBudgetExceeded},
		{name: "plan timeout", step: StepKeyPlan, err: fmt.Errorf("PROVIDER-MESSAGE: %w", context.DeadlineExceeded), wantCode: "llm_timeout", retryable: true},
		{name: "evaluate timeout", step: "eval_01", err: fmt.Errorf("PROVIDER-MESSAGE: %w", &domain.LLMError{Code: domain.LLMCodeTimeout, Retryable: false}), wantCode: "llm_timeout"},
		{name: "plan unknown", step: StepKeyPlan, err: errors.New("PROVIDER-MESSAGE"), wantCode: "llm_unknown", retryable: true},
		{name: "evaluate unknown", step: "eval_01", err: fmt.Errorf("PROVIDER-MESSAGE: %w", &domain.LLMError{Code: domain.LLMCodeError, Retryable: false}), wantCode: "llm_unknown"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			obs.Setup("info", &logs)
			h := newSH(t)
			h.instr.list[0].Text = "REQUEST-SECRET"
			if tc.step == StepKeyPlan {
				h.rt.Script("plan", FakeReply{Err: tc.err})
			} else {
				h.plan(DecisionExecute, shRead("a", "First"))
				h.must(StepKeyPlan)
				h.must("run_01")
				h.rt.Script("evaluate", FakeReply{Err: tc.err})
			}
			res, stepErr := h.step(tc.step)
			switch {
			case tc.budgetGate:
				if stepErr != nil || res.WaitState != workflow.StateWaitingForUser || len(h.owner.asked) != 1 {
					t.Fatalf("budget gate: wait=%q err=%v asked=%d", res.WaitState, stepErr, len(h.owner.asked))
				}
			case tc.failReason != "":
				if stepErr != nil || res.WaitState != "" {
					t.Fatalf("FAIL decision outcome: wait=%q err=%v", res.WaitState, stepErr)
				}
				d := h.plans.lastDecision()
				if d.Decision != DecisionFail || d.ReasonCode != tc.failReason {
					t.Fatalf("decision = %+v, want FAIL reason %q", d, tc.failReason)
				}
				if tc.step != StepKeyPlan && h.plans.planStatus(1) != PlanFailed {
					t.Fatalf("plan status = %q, want %q", h.plans.planStatus(1), PlanFailed)
				}
			default:
				if stepErr == nil {
					t.Fatal("retryable step failure unexpectedly succeeded")
				}
			}

			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			for _, forbidden := range []string{"REQUEST-SECRET", "PROVIDER-MESSAGE", "additionalProperties", `\"properties\"`} {
				if strings.Contains(logs.String(), forbidden) {
					t.Errorf("log contains forbidden content %q: %s", forbidden, logs.String())
				}
			}
			var failures []map[string]any
			for _, line := range lines {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatalf("log is not JSON: %v: %q", err, line)
				}
				if record["msg"] == "orchestrator llm call failed" {
					failures = append(failures, record)
				}
			}
			if len(failures) != 1 {
				t.Fatalf("failure log records = %d, want 1: %q", len(failures), logs.String())
			}
			record := failures[0]
			if record["msg"] != "orchestrator llm call failed" || record["component"] != "orchestrator" {
				t.Errorf("message fields = %#v", record)
			}
			if record["execution_id"] != float64(shExec) || record["step"] != tc.step || record["attempt"] != float64(1) {
				t.Errorf("execution fields = %#v", record)
			}
			if record["error_code"] != tc.wantCode || record["retryable"] != tc.retryable {
				t.Errorf("classification = %#v", record)
			}
		})
	}
}
