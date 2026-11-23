package workflow_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/obs"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// TC-F-6 (REQ-2): a claimed durable execution restores both identifiers into
// every step lifecycle record rather than using worker-process identity.
func TestPhaseFWorkflowStepLogsPersistedCorrelationAndExecution(t *testing.T) {
	oldDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(oldDefault) })
	var logs bytes.Buffer
	obs.Setup("info", &logs)
	clock := newFakeClock(time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC))
	definition := defaultDefinition(t)
	job := testJob(6, 1, definition)
	job.Execution.CorrelationID = "019a6690-abcd-7000-8000-0000000000f6"
	store := newFakeJobStore(job)
	worker := newTestWorker(t, store, clock, workflow.StepKindModel, execFunc(func(context.Context, workflow.StepContext) (workflow.StepResult, error) {
		return workflow.StepResult{}, nil
	}))
	processed, err := worker.ProcessNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("processed=%v err=%v", processed, err)
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("workflow lifecycle logs=%q", logs.String())
	}
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("non-JSON workflow log %q: %v", line, err)
		}
		if record["correlation_id"] != job.Execution.CorrelationID || record["execution_id"] != float64(job.ExecutionID) {
			t.Errorf("step log lost durable identifiers: %#v", record)
		}
	}
}
