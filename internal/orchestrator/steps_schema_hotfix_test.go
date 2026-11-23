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
		name      string
		step      string
		err       error
		wantCode  string
		retryable bool
	}{
		{"plan status", StepKeyPlan, &hotfixHTTPError{status: 400}, "llm_status_400", false},
		{"evaluate status", "eval_01", &hotfixHTTPError{status: 503, retryable: true}, "llm_status_503", true},
		{"plan invalid output", StepKeyPlan, fmt.Errorf("PROVIDER-MESSAGE: %w", ErrInvalidOutput), "llm_invalid_output", false},
		{"evaluate invalid output", "eval_01", fmt.Errorf("PROVIDER-MESSAGE: %w", ErrInvalidOutput), "llm_invalid_output", false},
		{"plan budget", StepKeyPlan, fmt.Errorf("PROVIDER-MESSAGE: %w", &BudgetExceededError{Kind: BudgetTokens}), "llm_budget", false},
		{"evaluate budget", "eval_01", fmt.Errorf("PROVIDER-MESSAGE: %w", &domain.LLMError{Code: domain.LLMCodeBudgetExceeded}), "llm_budget", false},
		{"plan timeout", StepKeyPlan, fmt.Errorf("PROVIDER-MESSAGE: %w", context.DeadlineExceeded), "llm_timeout", true},
		{"evaluate timeout", "eval_01", fmt.Errorf("PROVIDER-MESSAGE: %w", &domain.LLMError{Code: domain.LLMCodeTimeout, Retryable: false}), "llm_timeout", false},
		{"plan unknown", StepKeyPlan, errors.New("PROVIDER-MESSAGE"), "llm_unknown", true},
		{"evaluate unknown", "eval_01", fmt.Errorf("PROVIDER-MESSAGE: %w", &domain.LLMError{Code: domain.LLMCodeError, Retryable: false}), "llm_unknown", false},
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
			if _, err := h.step(tc.step); err == nil {
				t.Fatal("step unexpectedly succeeded")
			}

			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			if len(lines) != 1 {
				t.Fatalf("log lines = %d, want 1: %q", len(lines), logs.String())
			}
			for _, forbidden := range []string{"REQUEST-SECRET", "PROVIDER-MESSAGE", "additionalProperties", `\"properties\"`} {
				if strings.Contains(logs.String(), forbidden) {
					t.Errorf("log contains forbidden content %q: %s", forbidden, logs.String())
				}
			}
			var record map[string]any
			if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
				t.Fatalf("log is not JSON: %v: %q", err, lines[0])
			}
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
