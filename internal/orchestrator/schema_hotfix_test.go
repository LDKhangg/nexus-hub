package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TC-HS-5 (REQ-4): removing provider-side maxLength never weakens local
// validation of a generated plan title.
func TestGenerateValidated_RejectsPlanTitleOverSchemaLimit(t *testing.T) {
	bad := planJSON(DecisionExecute, okTask("a"))
	bad["tasks"].([]map[string]any)[0]["title"] = strings.Repeat("x", MaxTitleRunes+1)
	f := NewFakeRuntime().Script("plan", reply(t, bad, 1, 1))
	_, err := GenerateValidated(context.Background(), f, StructuredRequest{Prompt: "plan"}, SchemaPlan, 0)
	if !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("error = %v, want ErrInvalidOutput", err)
	}
}

// TC-HS-6 (REQ-4): removing provider-side maxItems never lets a 51-task plan
// through local GenerateValidated validation.
func TestGenerateValidated_RejectsPlanOverTaskLimit(t *testing.T) {
	tasks := make([]map[string]any, 0, MaxPlanTasks+1)
	for i := 0; i <= MaxPlanTasks; i++ {
		tasks = append(tasks, okTask(fmt.Sprintf("t%02d", i)))
	}
	f := NewFakeRuntime().Script("plan", reply(t, planJSON(DecisionExecute, tasks...), 1, 1))
	_, err := GenerateValidated(context.Background(), f, StructuredRequest{Prompt: "plan"}, SchemaPlan, 0)
	if !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("error = %v, want ErrInvalidOutput", err)
	}
}
