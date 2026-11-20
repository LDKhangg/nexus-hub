package metrics

import (
	"strings"
	"testing"
)

// TC-C-15 (REQ-11): retention labels stay closed and non-positive values do not create series.
func TestPhase12cRetentionMetricClosedLabels(t *testing.T) {
	h := testHub(nil)
	h.RetentionDeleted("message_logs", 2)
	h.RetentionDeleted("tool_runs", 1)
	h.RetentionDeleted("workspace-secret", 4)
	h.RetentionDeleted("policy_decisions", 0)
	h.RetentionDeleted("workflow_executions", -1)
	out := scrape(t, h)
	for _, want := range []string{
		`nexushub_retention_deleted_total{table="message_logs"} 2`,
		`nexushub_retention_deleted_total{table="tool_runs"} 1`,
		`nexushub_retention_deleted_total{table="other"} 4`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, "workspace-secret") || strings.Contains(out, `table="policy_decisions"`) || strings.Contains(out, `table="workflow_executions"`) {
		t.Fatal("unsafe or non-positive retention series exposed")
	}
}
