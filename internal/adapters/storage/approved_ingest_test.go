package storage

import (
	"fmt"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/memory"
)

// TC-1 (REQ-1): defaults are independently checked against persisted times.
func TestApproved_DebounceEightSecondsThirtySecondCap(t *testing.T) {
	ctx, pool, r := newAgentDB(t, true)
	for i := 0; i < 8; i++ {
		if _, err := r.Accept(ctx, []domain.NormalizedEvent{noticeEvent(fmt.Sprintf("burst-%d", i), "same-task")}); err != nil {
			t.Fatal(err)
		}
	}
	var delta float64
	if err := pool.QueryRow(ctx, `SELECT extract(epoch from run_at-updated_at)::float8 FROM agent_jobs`).Scan(&delta); err != nil {
		t.Fatal(err)
	}
	if delta < 7.5 || delta > 8.5 {
		t.Errorf("debounce delta=%f want 8 seconds", delta)
	}
	if countRows(t, ctx, pool, "agent_jobs") != 1 || countRows(t, ctx, pool, "notification_outbox") != 0 || countRows(t, ctx, pool, "message_logs") != 8 {
		t.Error("burst did not become one agent job without notices")
	}
	mustExec(t, ctx, pool, `UPDATE agent_jobs SET first_enqueued_at=now()-interval '29 seconds'`)
	if _, err := r.Accept(ctx, []domain.NormalizedEvent{noticeEvent("cap", "same-task")}); err != nil {
		t.Fatal(err)
	}
	if got := scalar[bool](t, ctx, pool, `SELECT run_at=first_enqueued_at+interval '30 seconds' FROM agent_jobs`); !got {
		t.Error("30-second cap not exact")
	}
}

// TC-2 (REQ-1): duplicate/out-of-order callbacks, tasks and workspaces retain
// separate durable message sets even when the external IDs are identical.
func TestApproved_AgentIngestNamespacesAndOutOfOrder(t *testing.T) {
	ctx, pool, r := newAgentDB(t, true)
	mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'B')`, workspaceB)
	mustExec(t, ctx, pool, `INSERT INTO integrations(id,workspace_id,platform,role,config) SELECT $1,$2,platform,role,config FROM integrations WHERE id=$3`, threadC, workspaceB, integrationB)
	mustExec(t, ctx, pool, `INSERT INTO integrations(id,workspace_id,platform,role,config) SELECT $1,$2,platform,role,jsonb_set(config,'{target_integration_id}',to_jsonb($3::text)) FROM integrations WHERE id=$4`, threadB, workspaceB, threadC, integrationA)
	e := noticeEvent("shared-id", "same-task")
	older := noticeEvent("older-id", "same-task")
	older.OccurredAt = e.OccurredAt.Add(-time.Hour)
	otherTask := noticeEvent("shared-id", "other-task")
	otherWorkspace := e
	otherWorkspace.WorkspaceID = workspaceB
	otherWorkspace.IntegrationID = threadB
	for _, ev := range []domain.NormalizedEvent{e, older, e, otherTask, otherWorkspace} {
		if _, err := r.Accept(ctx, []domain.NormalizedEvent{ev}); err != nil {
			t.Fatal(err)
		}
	}
	if countRows(t, ctx, pool, "message_logs") != 4 || countRows(t, ctx, pool, "agent_jobs") != 3 || countRows(t, ctx, pool, "notification_outbox") != 0 {
		t.Error("duplicates or namespace isolation lost messages/jobs")
	}
	if n := scalar[int](t, ctx, pool, `SELECT count(*) FROM agent_jobs WHERE workspace_id=$1`, workspaceB); n != 1 {
		t.Error("workspace B did not retain its own job")
	}
}

// TC-5 (REQ-2): both absent and disabled optional agent configuration retain
// the existing comment and status notice behavior, with no agent jobs.
func TestApproved_Phase1WithoutAgentUnchanged(t *testing.T) {
	for _, mode := range []string{"absent", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, pool, r := newAgentDB(t, false)
			if mode == "absent" {
				mustExec(t, ctx, pool, `UPDATE integrations SET config=config-'agent' WHERE id=$1`, integrationA)
			}
			s := noticeEvent("status", "task")
			s.EventType = domain.EventStatusChanged
			for _, ev := range []domain.NormalizedEvent{noticeEvent("comment", "task"), s} {
				if _, err := r.Accept(ctx, []domain.NormalizedEvent{ev}); err != nil {
					t.Fatal(err)
				}
			}
			if countRows(t, ctx, pool, "notification_outbox") != 2 || countRows(t, ctx, pool, "agent_jobs") != 0 {
				t.Error("Phase 1 routing changed")
			}
		})
	}
}

// TC-29 (REQ-14): the minimum eligible five excludes exactly the newest three.
func TestApproved_SummaryExactlyFiveEligibleExcludesThreeRecent(t *testing.T) {
	e := newAgentEnv(t, enabledCfg)
	var ids []int64
	for i := 0; i < 8; i++ {
		ids = append(ids, e.message(t, fmt.Sprintf("summary-%d", i), "external_user", "message"))
	}
	_, cursor, candidates, err := e.store.SummaryCandidates(e.ctx, workspaceA, threadA)
	if err != nil || cursor != 0 || len(candidates) != 5 {
		t.Fatal("summary candidate threshold or cursor incorrect")
	}
	for i, m := range candidates {
		if m.ID != ids[i] || m.SenderRole != memory.SenderRoleExternalUser {
			t.Error("wrong candidate window")
		}
	}
	if err := e.store.SaveSummary(e.ctx, workspaceA, threadA, 0, ids[4], "Tóm tắt hữu ích 🧠"); err != nil {
		t.Fatal(err)
	}
	_, cursor, candidates, err = e.store.SummaryCandidates(e.ctx, workspaceA, threadA)
	if err != nil || cursor != ids[4] || len(candidates) != 0 {
		t.Error("summary cursor did not advance exactly to fifth eligible message")
	}
}
