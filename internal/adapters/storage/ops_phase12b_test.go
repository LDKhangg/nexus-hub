package storage

import (
	"math"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// Phase 12 batch B (REQ-7 budget, REQ-8, REQ-9, REQ-10) against PostgreSQL.
// Each test func owns ONE test database (the schema lock is held per env)
// and uses fixed timestamps relative to a base, no sleeps.

// p12bExec makes a started execution of workspace ws in the given state.
func p12bExec(e *wfEnv, ws string, versionID int64, trigger string) int64 {
	e.t.Helper()
	return e.start(ws, versionID, trigger, chainDef("s1", "s2")).ID
}

func p12bSetState(e *wfEnv, id int64, state string) {
	e.t.Helper()
	switch state {
	case "COMPLETED", "FAILED":
		mustExec(e.t, e.ctx, e.pool, `UPDATE workflow_executions SET state=$2, finished_at=now()-interval '1 minute', reason_code=CASE WHEN $2='FAILED' THEN 'failed' END WHERE id=$1`, id, state)
	case "CANCELLED":
		mustExec(e.t, e.ctx, e.pool, `UPDATE workflow_executions SET state='CANCELLED', cancelled_at=now(), finished_at=now() WHERE id=$1`, id)
	default:
		mustExec(e.t, e.ctx, e.pool, `UPDATE workflow_executions SET state=$2 WHERE id=$1`, id, state)
	}
}

func p12bJob(e *wfEnv, id int64, status string, due, updated time.Time) {
	e.t.Helper()
	if status == "processing" {
		mustExec(e.t, e.ctx, e.pool, `UPDATE jobs SET status='processing', claim_token=gen_random_uuid(), lease_expires_at=now()+interval '1 hour', due_at=$2, updated_at=$3 WHERE execution_id=$1`, id, due, updated)
		return
	}
	mustExec(e.t, e.ctx, e.pool, `UPDATE jobs SET status=$2, due_at=$3, updated_at=$4 WHERE execution_id=$1`, id, status, due, updated)
}

// TC-B-28, TC-B-29, TC-B-30 (REQ-7, REQ-8, REQ-9)
func TestPhase12bOpsLedgerAndSnapshot(t *testing.T) {
	e := newWFEnv(t)
	ctx := e.ctx
	ops, err := NewOpsStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	t.Run("empty database reports zeros", func(t *testing.T) {
		snap, err := ops.Snapshot(ctx, "", day)
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.ExecutionsByState) != 0 || snap.QueuePending != 0 || snap.OldestDueAge != 0 ||
			snap.WorkerDevices != 0 || snap.WorkersOnline != 0 || snap.NewestHeartbeatAge != -1 || len(snap.LLMUsage) != 0 {
			t.Fatalf("empty snapshot = %+v", snap)
		}
	})

	t.Run("ledger upsert sums per workspace day provider model", func(t *testing.T) {
		add := func(ws string, at time.Time, provider, model string, in, out int64, cost float64) {
			t.Helper()
			if err := ops.AddLLMUsage(ctx, domain.LLMUsageRecord{WorkspaceID: ws, Day: at, Provider: provider, Model: model, InputTokens: in, OutputTokens: out, CostUSD: cost}); err != nil {
				t.Fatal(err)
			}
		}
		add(workspaceA, day, "anthropic", "claude-sonnet-5-5", 100, 10, 0.5)
		add(workspaceA, day.Add(5*time.Hour), "anthropic", "claude-sonnet-5-5", 100, 10, 0.5) // same UTC day: summed
		add(workspaceA, day, "anthropic", "claude-haiku-5-5", 7, 3, 0.25)
		add(workspaceA, day, "claude_cli", "claude-sonnet-5-5", 50, 5, 0)
		add(workspaceA, day.Add(24*time.Hour), "anthropic", "claude-sonnet-5-5", 1, 1, 9) // next day: own row
		add(workspaceB, day, "anthropic", "claude-sonnet-5-5", 1000, 100, 3)
		// 06:30 on the 21st in +07 is 23:30 UTC on the 20th.
		ict := time.FixedZone("ict", 7*3600)
		add(workspaceA, time.Date(2026, 11, 21, 6, 30, 0, 0, ict), "anthropic", "claude-haiku-5-5", 1, 1, 0.25)

		cost, err := ops.LLMCostForDay(ctx, workspaceA, day)
		if err != nil || math.Abs(cost-1.5) > 1e-9 {
			t.Fatalf("workspace A cost = %v %v, want 1.5 (0.5+0.5+0.25+0+0.25)", cost, err)
		}
		if cost, _ := ops.LLMCostForDay(ctx, workspaceB, day); math.Abs(cost-3) > 1e-9 {
			t.Fatalf("workspace B cost = %v", cost)
		}
		if cost, _ := ops.LLMCostForDay(ctx, workspaceA, day.Add(24*time.Hour)); math.Abs(cost-9) > 1e-9 {
			t.Fatalf("next-day cost = %v", cost)
		}
		if cost, _ := ops.LLMCostForDay(ctx, workspaceA, day.Add(48*time.Hour)); cost != 0 {
			t.Fatalf("empty day cost = %v", cost)
		}
		usage, err := ops.LLMUsageForDay(ctx, workspaceA, day)
		if err != nil || len(usage) != 3 {
			t.Fatalf("workspace A usage = %+v %v", usage, err)
		}
		// Ordered by provider, model; the sonnet API row holds the sums.
		if usage[0].Provider != "anthropic" || usage[0].Model != "claude-haiku-5-5" || usage[0].InputTokens != 8 || usage[0].OutputTokens != 4 {
			t.Fatalf("haiku row = %+v", usage[0])
		}
		if usage[1].Model != "claude-sonnet-5-5" || usage[1].InputTokens != 200 || usage[1].OutputTokens != 20 || math.Abs(usage[1].CostUSD-1) > 1e-9 {
			t.Fatalf("sonnet row = %+v", usage[1])
		}
		if usage[2].Provider != "claude_cli" || usage[2].CostUSD != 0 {
			t.Fatalf("cli row = %+v", usage[2])
		}
		hub, _ := ops.LLMUsageForDay(ctx, "", day)
		var hubIn int64
		for _, u := range hub {
			if u.Provider == "anthropic" && u.Model == "claude-sonnet-5-5" {
				hubIn = u.InputTokens
			}
		}
		if hubIn != 1200 {
			t.Fatalf("hub-wide sonnet input = %d, want 1200", hubIn)
		}
		// Rejected input.
		bad := []domain.LLMUsageRecord{
			{WorkspaceID: workspaceA, Day: day, Provider: "anthropic", Model: "m", InputTokens: -1},
			{WorkspaceID: workspaceA, Day: day, Provider: "anthropic", Model: "m", CostUSD: -0.01},
			{WorkspaceID: "not-a-uuid", Day: day, Provider: "anthropic", Model: "m", InputTokens: 1},
		}
		for _, r := range bad {
			if err := ops.AddLLMUsage(ctx, r); err == nil {
				t.Errorf("AddLLMUsage accepted %+v", r)
			}
		}
		if _, err := ops.LLMCostForDay(ctx, "not-a-uuid", day); err == nil {
			t.Error("LLMCostForDay accepted a bad workspace id")
		}
	})

	t.Run("migration 00038 constraints", func(t *testing.T) {
		ins := func(provider, model string, in, out int64, cost float64, ws string) error {
			_, err := e.pool.Exec(ctx, `INSERT INTO llm_usage_daily(workspace_id,day,provider,model,input_tokens,output_tokens,cost_usd) VALUES($1,'2030-01-01',$2,$3,$4,$5,$6)`, ws, provider, model, in, out, cost)
			return err
		}
		requirePGCode(t, ins("anthropic", "bad model!", 1, 1, 0, workspaceA), "23514")
		requirePGCode(t, ins("anthropic", "", 1, 1, 0, workspaceA), "23514")
		requirePGCode(t, ins("Anthropic", "m", 1, 1, 0, workspaceA), "23514")
		requirePGCode(t, ins("anthropic", "m", -1, 1, 0, workspaceA), "23514")
		requirePGCode(t, ins("anthropic", "m", 1, -1, 0, workspaceA), "23514")
		requirePGCode(t, ins("anthropic", "m", 1, 1, -0.5, workspaceA), "23514")
		requirePGCode(t, ins("anthropic", "m", 1, 1, 0, missingID), "23503")
		if err := ins("anthropic", "ok-model_1.0", 1, 1, 0, workspaceA); err != nil {
			t.Fatal(err)
		}
		var rls bool
		if err := e.pool.QueryRow(ctx, `SELECT relrowsecurity FROM pg_class WHERE oid='public.llm_usage_daily'::regclass`).Scan(&rls); err != nil || !rls {
			t.Fatalf("RLS on llm_usage_daily: %v %v", rls, err)
		}
	})

	t.Run("snapshot counts states queue and workers per scope", func(t *testing.T) {
		now := time.Now()
		va := e.enabledVersion(workspaceA, "ops-a")
		vb := e.enabledVersion(workspaceB, "ops-b")
		a1 := p12bExec(e, workspaceA, va.ID, "ops:a1")
		a2 := p12bExec(e, workspaceA, va.ID, "ops:a2")
		a3 := p12bExec(e, workspaceA, va.ID, "ops:a3")
		b1 := p12bExec(e, workspaceB, vb.ID, "ops:b1")
		p12bSetState(e, a1, "QUEUED")
		p12bSetState(e, a2, "QUEUED")
		p12bSetState(e, a3, "FAILED")
		p12bSetState(e, b1, "RUNNING")
		p12bJob(e, a1, "pending", now.Add(-10*time.Minute), now)
		p12bJob(e, a2, "pending", now.Add(time.Hour), now) // not due: queued but not waiting
		p12bJob(e, a3, "completed", now.Add(-time.Hour), now)
		p12bJob(e, b1, "pending", now.Add(-5*time.Minute), now)
		mustExec(t, ctx, e.pool, `INSERT INTO worker_devices(workspace_id,name,token_hash,state,last_seen_at,revoked_at) VALUES
			($1,'laptop-a',sha256('a1'),'online',now()-interval '30 seconds',NULL),
			($1,'old-a',sha256('a2'),'offline',now()-interval '1 hour',NULL),
			($1,'revoked-a',sha256('a3'),'online',now()-interval '1 second',now()),
			($2,'pc-b',sha256('b1'),'busy',now()-interval '5 seconds',NULL)`, workspaceA, workspaceB)

		hub, err := ops.Snapshot(ctx, "", day)
		if err != nil {
			t.Fatal(err)
		}
		if hub.ExecutionsByState["QUEUED"] != 2 || hub.ExecutionsByState["FAILED"] != 1 || hub.ExecutionsByState["RUNNING"] != 1 {
			t.Fatalf("hub states = %v", hub.ExecutionsByState)
		}
		if hub.QueuePending != 3 || hub.OldestDueAge < 590*time.Second || hub.OldestDueAge > 700*time.Second {
			t.Fatalf("hub queue = %d oldest %v", hub.QueuePending, hub.OldestDueAge)
		}
		if hub.WorkerDevices != 3 || hub.WorkersOnline != 2 || hub.NewestHeartbeatAge < 3*time.Second || hub.NewestHeartbeatAge > 30*time.Second {
			t.Fatalf("hub workers = %d/%d heartbeat %v (revoked device must not count)", hub.WorkerDevices, hub.WorkersOnline, hub.NewestHeartbeatAge)
		}
		if len(hub.LLMUsage) == 0 || !hub.Day.Equal(time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("hub llm = %+v day %v", hub.LLMUsage, hub.Day)
		}
		a, err := ops.Snapshot(ctx, workspaceA, day)
		if err != nil {
			t.Fatal(err)
		}
		if a.ExecutionsByState["RUNNING"] != 0 || a.ExecutionsByState["QUEUED"] != 2 || a.QueuePending != 2 ||
			a.OldestDueAge < 590*time.Second || a.OldestDueAge > 700*time.Second ||
			a.WorkerDevices != 2 || a.WorkersOnline != 1 || a.NewestHeartbeatAge < 25*time.Second || a.NewestHeartbeatAge > 60*time.Second {
			t.Fatalf("workspace A snapshot = %+v", a)
		}
		b, err := ops.Snapshot(ctx, workspaceB, day)
		if err != nil {
			t.Fatal(err)
		}
		if b.ExecutionsByState["RUNNING"] != 1 || b.QueuePending != 1 || b.OldestDueAge < 290*time.Second || b.OldestDueAge > 400*time.Second ||
			b.WorkerDevices != 1 || b.WorkersOnline != 1 {
			t.Fatalf("workspace B snapshot = %+v", b)
		}
		if _, err := ops.Snapshot(ctx, "not-a-uuid", day); err == nil {
			t.Error("Snapshot accepted a bad workspace id")
		}
	})

	t.Run("migration 00038 down and up", func(t *testing.T) {
		mustExec(t, ctx, e.pool, downSection(t, "00038_llm_usage_daily.sql"))
		if _, err := e.pool.Exec(ctx, `SELECT 1 FROM llm_usage_daily`); err == nil {
			t.Fatal("table still exists after Down")
		}
		mustExec(t, ctx, e.pool, upSection(t, "00038_llm_usage_daily.sql"))
		var n int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM llm_usage_daily`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("after Up: %d %v", n, err)
		}
	})
}
