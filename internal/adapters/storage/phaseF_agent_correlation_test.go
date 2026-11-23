package storage

import (
	"database/sql"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/google/uuid"
)

// TC-F-19 (REQ-2): ClickUp correlation survives durable ingestion and workflow
// adoption. Legacy nullable jobs receive a fresh execution correlation.
func TestPhaseFAgentWorkflowCorrelationContinuity(t *testing.T) {
	const webhookCorrelation = "019a6690-abcd-7000-8000-0000000000f2"
	for _, tc := range []struct {
		name   string
		legacy bool
	}{
		{name: "webhook_correlation", legacy: false},
		{name: "legacy_null_gets_fresh_id", legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, pool, notices := newAgentDB(t, true)
			event := noticeEvent("phase-f-correlation", "task-1")
			event.CorrelationID = webhookCorrelation
			if result, err := notices.Accept(ctx, []domain.NormalizedEvent{event}); err != nil || result.Accepted != 1 {
				t.Fatalf("accept=%+v err=%v", result, err)
			}
			jobID := scalar[int64](t, ctx, pool, `SELECT id FROM agent_jobs`)
			if tc.legacy {
				mustExec(t, ctx, pool, `UPDATE agent_jobs SET correlation_id=NULL, run_at=$2 WHERE id=$1`, jobID, time.Now().Add(-time.Minute))
			} else {
				mustExec(t, ctx, pool, `UPDATE agent_jobs SET run_at=$2 WHERE id=$1`, jobID, time.Now().Add(-time.Minute))
			}
			var stored sql.NullString
			if err := pool.QueryRow(ctx, `SELECT correlation_id FROM agent_jobs WHERE id=$1`, jobID).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if !tc.legacy && (!stored.Valid || stored.String != webhookCorrelation) {
				t.Fatalf("job correlation=%+v", stored)
			}
			if tc.legacy && stored.Valid {
				t.Fatalf("legacy job correlation=%+v", stored)
			}
			agents, err := NewAgentStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			workflows, err := NewWorkflowStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			starter, err := NewAgentWorkflowStore(agents, workflows)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := workflows.Cutover(ctx, workspaceA, true); err != nil {
				t.Fatal(err)
			}
			if started, err := starter.StartDueBatches(ctx, workspaceA, time.Now(), testPipelineSettings, 1); err != nil || started != 1 {
				t.Fatalf("started=%d err=%v", started, err)
			}
			correlation := scalar[string](t, ctx, pool, `SELECT e.correlation_id FROM workflow_executions e JOIN agent_jobs j ON j.workflow_execution_id=e.id WHERE j.id=$1`, jobID)
			if tc.legacy {
				if parsed, err := uuid.Parse(correlation); err != nil || correlation == webhookCorrelation || parsed == uuid.Nil {
					t.Fatalf("fresh legacy correlation=%q err=%v", correlation, err)
				}
			} else if correlation != webhookCorrelation {
				t.Fatalf("execution correlation=%q want=%q", correlation, webhookCorrelation)
			}
		})
	}
}
