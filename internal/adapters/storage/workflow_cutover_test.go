package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// P7 fixtures: legacy behavior runs (no execution) in the states a live
// legacy hub leaves behind.

func (e *wfEnv) legacyRun(ws, name, subject string) behavior.Run {
	e.t.Helper()
	v, err := e.behaviors.GetVersion(e.ctx, ws, name, 0)
	if err != nil {
		v = e.enabledVersion(ws, name)
	}
	run, created, err := e.behaviors.CreateRun(e.ctx, ws, behavior.NewRun{
		BehaviorID: v.BehaviorID, VersionID: v.ID, Version: v.Version, TriggerKind: behavior.TriggerCommentAdded,
		Trigger: []byte(`{}`), Subject: subject, Revision: "r1", RunAt: time.Now().Add(-time.Minute),
	})
	if err != nil || !created {
		e.t.Fatalf("legacy run %s: created=%v err=%v", subject, created, err)
	}
	return run
}

func (e *wfEnv) setRunStatus(id int64, status string) {
	e.t.Helper()
	mustExec(e.t, e.ctx, e.pool, `UPDATE behavior_runs SET status=$2, claim_token=NULL, locked_until=NULL, finished_at=CASE WHEN $2 IN ('done','failed','canceled') THEN now() END WHERE id=$1`, id, status)
}

func (e *wfEnv) legacyApproval(ws string, runID int64, key string) behavior.Approval {
	e.t.Helper()
	it := e.addItem(ws, runID, key, behavior.ClassAsk)
	a, err := e.behaviors.CreateApproval(e.ctx, ws, behavior.NewApproval{
		Kind: behavior.ApprovalPostDraft, RunID: runID, ItemID: it.ID, Action: "auto", Resource: "task", Nonce: nonce("c"),
		ExpiresAt: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		e.t.Fatalf("legacy approval: %v", err)
	}
	return a
}

// legacyClaim claims the oldest due legacy run with a lease.
func (e *wfEnv) legacyClaim(lease time.Duration) *behavior.Run {
	e.t.Helper()
	r, err := e.behaviors.ClaimRun(e.ctx, time.Now(), lease, behavior.MaxRunAttempts)
	if err != nil {
		e.t.Fatalf("legacy claim: %v", err)
	}
	return r
}

// dbState is every row the cutover may write, as one comparable string.
func (e *wfEnv) dbState() string {
	e.t.Helper()
	var s string
	if err := e.pool.QueryRow(e.ctx, `SELECT concat_ws('|',
		(SELECT count(*) FROM workflow_executions), (SELECT count(*) FROM execution_steps), (SELECT count(*) FROM jobs),
		(SELECT count(*) FROM workflow_waits), (SELECT count(*) FROM workflow_signals),
		(SELECT COALESCE(string_agg(workspace_id::text||mode, ',' ORDER BY workspace_id),'') FROM workflow_runtime_modes),
		(SELECT COALESCE(md5(string_agg(r::text, ',' ORDER BY id)),'') FROM behavior_runs r),
		(SELECT COALESCE(md5(string_agg(a::text, ',' ORDER BY id)),'') FROM approvals a),
		(SELECT COALESCE(md5(string_agg(t::text, ',' ORDER BY id)),'') FROM tool_runs t))`).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *wfEnv) mode(ws string) string {
	e.t.Helper()
	m, err := e.store.RuntimeMode(e.ctx, ws)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *wfEnv) executionOfRun(runID int64) (int64, bool) {
	var id int64
	err := e.pool.QueryRow(e.ctx, `SELECT id FROM workflow_executions WHERE legacy_run_id=$1 ORDER BY id LIMIT 1`, runID).Scan(&id)
	return id, err == nil
}

func (e *wfEnv) runStatus(id int64) (status, code string) {
	_ = e.pool.QueryRow(e.ctx, `SELECT status, COALESCE(error_code,'') FROM behavior_runs WHERE id=$1`, id).Scan(&status, &code)
	return
}

func (e *wfEnv) startedToolRun(ws string, runID int64, key string) int64 {
	e.t.Helper()
	var id int64
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO tool_runs(workspace_id,behavior_run_id,correlation_id,step,tool,tier,idempotency_key,status)
		VALUES($1,$2,gen_random_uuid(),1,'clickup_post_comment','write',$3,'started') RETURNING id`, ws, runID, key).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// TC-P701 (REQ-12, REQ-14): the preflight writes nothing; an apply
// imports and fences; a second apply is a no-op.
func TestWorkflowCutoverIdempotent(t *testing.T) {
	e := newWFEnv(t)
	pending := e.legacyRun(workspaceA, "cut", "task:p1")
	done := e.legacyRun(workspaceA, "cut", "task:d1")
	e.setRunStatus(done.ID, "done")
	e.legacyApproval(workspaceA, done.ID, "ask")
	before := e.dbState()
	pre, err := e.store.Cutover(e.ctx, workspaceA, false)
	if err != nil || pre.Applied || pre.Mode != RuntimeModeLegacy || pre.BehaviorRunsPending != 1 || pre.BehaviorRunsWaiting != 1 || pre.ApprovalWaits != 1 {
		t.Fatalf("TC-P701: preflight %+v %v", pre, err)
	}
	if e.dbState() != before {
		t.Fatal("TC-P701: the preflight wrote rows")
	}
	r1, err := e.store.Cutover(e.ctx, workspaceA, true)
	if err != nil || !r1.Applied || r1.ImportedExecutions != 2 || e.mode(workspaceA) != RuntimeModeWorkflow {
		t.Fatalf("TC-P701: apply %+v %v", r1, err)
	}
	after := e.dbState()
	r2, err := e.store.Cutover(e.ctx, workspaceA, true)
	if err != nil || r2.Applied || !r2.AlreadyCutOver || r2.ImportedExecutions != 0 {
		t.Fatalf("TC-P701: second apply %+v %v", r2, err)
	}
	if e.dbState() != after {
		t.Fatal("TC-P701: the second apply wrote rows")
	}
	for _, id := range []int64{pending.ID, done.ID} {
		if n := e.count(`SELECT count(*) FROM workflow_executions WHERE legacy_run_id=$1`, id); n != 1 {
			t.Fatalf("TC-P701: run %d has %d executions", id, n)
		}
	}
	if n := e.count(`SELECT count(*) FROM workflow_waits`); n != 1 {
		t.Fatalf("TC-P701: %d waits", n)
	}
}

// TC-P702 (REQ-14): every legacy state is mapped or left alone, counted,
// and an unmappable record blocks the whole cutover by ID and code with
// nothing written; once it is gone the cutover applies.
func TestWorkflowCutoverMixedLegacyStates(t *testing.T) {
	e := newWFEnv(t)
	pending := e.legacyRun(workspaceA, "mix", "task:pending")
	runningLive := e.legacyRun(workspaceA, "mix", "task:running-live")
	_ = e.legacyClaim(time.Hour) // pending is older: claims it first
	e.setRunStatus(pending.ID, "pending")
	mustExec(t, e.ctx, e.pool, `UPDATE behavior_runs SET run_at=now()+interval '1 hour' WHERE id=$1`, pending.ID)
	live := e.legacyClaim(time.Hour)
	if live == nil || live.ID != runningLive.ID {
		t.Fatalf("claim live: %+v", live)
	}
	runningExpired := e.legacyRun(workspaceA, "mix", "task:running-expired")
	if c := e.legacyClaim(time.Hour); c == nil || c.ID != runningExpired.ID {
		t.Fatalf("claim expired: %+v", c)
	}
	mustExec(t, e.ctx, e.pool, `UPDATE behavior_runs SET locked_until=now()-interval '1 minute' WHERE id=$1`, runningExpired.ID)
	doneNo := e.legacyRun(workspaceA, "mix", "task:done")
	e.setRunStatus(doneNo.ID, "done")
	doneAsk := e.legacyRun(workspaceA, "mix", "task:done-ask")
	e.setRunStatus(doneAsk.ID, "done")
	e.legacyApproval(workspaceA, doneAsk.ID, "a1")
	e.legacyApproval(workspaceA, doneAsk.ID, "a2")
	failedAsk := e.legacyRun(workspaceA, "mix", "task:failed-ask")
	e.setRunStatus(failedAsk.ID, "failed")
	legacyRoute := e.legacyApproval(workspaceA, failedAsk.ID, "f1")
	canceled := e.legacyRun(workspaceA, "mix", "task:canceled")
	e.setRunStatus(canceled.ID, "canceled")
	tool := e.startedToolRun(workspaceA, runningExpired.ID, "behavior_run_action:x:0")

	// An unmappable record: a pinned version whose document cannot resolve.
	var badVersion int64
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO behavior_versions(workspace_id,behavior_id,version,document) SELECT workspace_id,behavior_id,99,'{"schema":"garbage"}' FROM behavior_versions WHERE id=$1 RETURNING id`, pending.VersionID).Scan(&badVersion); err != nil {
		t.Fatal(err)
	}
	bad := e.legacyRun(workspaceA, "mix", "task:bad")
	mustExec(t, e.ctx, e.pool, `UPDATE behavior_runs SET behavior_version_id=$2, version=99 WHERE id=$1`, bad.ID, badVersion)
	before := e.dbState()
	rep, err := e.store.Cutover(e.ctx, workspaceA, true)
	if !errors.Is(err, ErrCutoverBlocked) || rep.BlockedTotal != 1 || len(rep.Blocked) != 1 ||
		rep.Blocked[0] != (CutoverIssue{Kind: CutoverKindBehaviorRun, ID: bad.ID, Code: CutoverCodeVersionInvalid}) {
		t.Fatalf("TC-P702: blocked cutover %+v %v", rep, err)
	}
	if e.dbState() != before || e.mode(workspaceA) != RuntimeModeLegacy {
		t.Fatal("TC-P702: a blocked cutover wrote rows or set the fence")
	}
	e.setRunStatus(bad.ID, "canceled") // the owner resolves it; nothing is deleted
	rep, err = e.store.Cutover(e.ctx, workspaceA, true)
	if err != nil || !rep.Applied {
		t.Fatalf("TC-P702: apply %+v %v", rep, err)
	}
	if rep.BehaviorRunsPending != 1 || rep.BehaviorRunsRunning != 2 || rep.BehaviorRunsLeaseLive != 1 || rep.BehaviorRunsWaiting != 1 ||
		rep.ApprovalWaits != 2 || rep.ImportedExecutions != 4 || rep.LegacyRouteApprovals != 1 || rep.InFlightToolRuns != 1 {
		t.Fatalf("TC-P702: report %+v", rep)
	}
	for _, id := range []int64{pending.ID, runningLive.ID, runningExpired.ID, doneAsk.ID} {
		if _, ok := e.executionOfRun(id); !ok {
			t.Fatalf("TC-P702: run %d not imported", id)
		}
	}
	for _, id := range []int64{doneNo.ID, failedAsk.ID, canceled.ID, bad.ID} {
		if _, ok := e.executionOfRun(id); ok {
			t.Fatalf("TC-P702: terminal run %d imported", id)
		}
	}
	if n := e.count(`SELECT count(*) FROM workflow_waits WHERE approval_id=$1`, legacyRoute.ID); n != 0 {
		t.Fatal("TC-P702: approval of a failed run bound as a wait")
	}
	// The in-flight tool run stays an unknown outcome: untouched, not resent.
	var status string
	_ = e.pool.QueryRow(e.ctx, `SELECT status FROM tool_runs WHERE id=$1`, tool).Scan(&status)
	if status != "started" || e.count(`SELECT count(*) FROM tool_runs`) != 1 {
		t.Fatalf("TC-P702: in-flight tool run %q (rows %d)", status, e.count(`SELECT count(*) FROM tool_runs`))
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO tool_runs(workspace_id,behavior_run_id,correlation_id,step,tool,tier,idempotency_key,status) VALUES($1,$2,gen_random_uuid(),1,'clickup_post_comment','write','behavior_run_action:x:0','started')`, workspaceA, runningExpired.ID); err == nil {
		t.Fatal("TC-P702: a second send under the in-flight receipt key was accepted")
	}
}

// TC-P703 (REQ-9, REQ-12): legacy claimers and the durable starters never
// both schedule a workspace, also when they race the cutover.
func TestWorkflowCutoverNoDualClaimers(t *testing.T) {
	e := newWFEnv(t)
	for i := 0; i < 12; i++ {
		e.legacyRun(workspaceA, "dual", fmt.Sprintf("task:%d", i))
	}
	if n, err := e.store.StartPendingBehaviorRuns(e.ctx, workspaceA, time.Now(), 10); err != nil || n != 0 {
		t.Fatalf("TC-P703: durable starter ran in a legacy workspace: %d %v", n, err)
	}
	var cutoverAt atomic.Int64
	var lateClaims atomic.Int32
	var cutErr error
	parallel(4, func(i int) {
		if i == 0 {
			time.Sleep(5 * time.Millisecond)
			_, cutErr = e.store.Cutover(e.ctx, workspaceA, true)
			cutoverAt.Store(time.Now().UnixNano())
			return
		}
		for k := 0; k < 8; k++ {
			r, err := e.behaviors.ClaimRun(e.ctx, time.Now(), time.Minute, behavior.MaxRunAttempts)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if r != nil && cutoverAt.Load() != 0 {
				lateClaims.Add(1)
			}
			if i == 3 {
				if _, err := e.store.StartPendingBehaviorRuns(e.ctx, workspaceA, time.Now(), 2); err != nil {
					t.Errorf("start: %v", err)
				}
			}
		}
	})
	if cutErr != nil {
		t.Fatalf("cutover: %v", cutErr)
	}
	if lateClaims.Load() != 0 {
		t.Fatalf("TC-P703: %d legacy claims after the cutover committed", lateClaims.Load())
	}
	if r := e.legacyClaim(time.Minute); r != nil {
		t.Fatalf("TC-P703: legacy claim in a workflow workspace: %+v", r)
	}
	if n, err := e.store.StartPendingBehaviorRuns(e.ctx, workspaceA, time.Now(), 50); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Logf("TC-P703: starter picked %d runs created during the race", n)
	}
	// One execution per run; a run claimed by legacy is due only after its lease.
	if n := e.count(`SELECT count(*) FROM (SELECT legacy_run_id FROM workflow_executions GROUP BY legacy_run_id HAVING count(*) > 1) d`); n != 0 {
		t.Fatalf("TC-P703: %d runs with two executions", n)
	}
	if n := e.count(`SELECT count(*) FROM behavior_runs r WHERE r.status IN ('pending','running') AND NOT EXISTS (SELECT 1 FROM workflow_executions x WHERE x.legacy_run_id=r.id)`); n != 0 {
		t.Fatalf("TC-P703: %d open runs no scheduler owns", n)
	}
	if n := e.count(`SELECT count(*) FROM behavior_runs r JOIN workflow_executions x ON x.legacy_run_id=r.id JOIN jobs j ON j.execution_id=x.id
		WHERE r.claim_token IS NOT NULL AND j.status='pending' AND j.due_at < r.locked_until - interval '1 second'`); n != 0 {
		t.Fatalf("TC-P703: %d imported runs due before their legacy lease ends", n)
	}
}

// TC-P704 (REQ-14): imports keep IDs, receipts and references: the
// execution's operation key is behavior_run:<id>, the legacy run link is
// kept, historical tool runs still resolve their run, and receipt keys are
// unchanged.
func TestWorkflowCutoverPreservesReceiptKeys(t *testing.T) {
	e := newWFEnv(t)
	run := e.legacyRun(workspaceA, "keys", "task:keys")
	old := e.legacyRun(workspaceA, "keys", "task:old")
	e.setRunStatus(old.ID, "done")
	mustExec(t, e.ctx, e.pool, `INSERT INTO tool_runs(workspace_id,behavior_run_id,correlation_id,step,tool,tier,idempotency_key,status) VALUES($1,$2,gen_random_uuid(),1,'clickup_post_comment','write','receipt-old','ok'),($1,$3,gen_random_uuid(),1,'clickup_post_comment','write','receipt-live','ok')`, workspaceA, old.ID, run.ID)
	var beforeKeys string
	_ = e.pool.QueryRow(e.ctx, `SELECT string_agg(id||':'||idempotency_key||':'||behavior_run_id, ',' ORDER BY id) FROM tool_runs`).Scan(&beforeKeys)
	if _, err := e.store.Cutover(e.ctx, workspaceA, true); err != nil {
		t.Fatal(err)
	}
	id, ok := e.executionOfRun(run.ID)
	if !ok {
		t.Fatal("not imported")
	}
	ex := e.execution(workspaceA, id)
	if ex.References.LegacyRunID != run.ID || ex.TriggerKey != behavior.RunOperationKey(run.ID) || ex.VersionID != run.VersionID {
		t.Fatalf("TC-P704: references %+v op %q version %d", ex.References, ex.TriggerKey, ex.VersionID)
	}
	var afterKeys string
	_ = e.pool.QueryRow(e.ctx, `SELECT string_agg(t.id||':'||t.idempotency_key||':'||r.id, ',' ORDER BY t.id) FROM tool_runs t JOIN behavior_runs r ON r.id=t.behavior_run_id`).Scan(&afterKeys)
	if afterKeys != beforeKeys {
		t.Fatalf("TC-P704: receipts changed:\n%s\n%s", beforeKeys, afterKeys)
	}
	// The same logical run cannot get a second execution from a starter.
	if n, err := e.store.StartPendingBehaviorRuns(e.ctx, workspaceA, time.Now(), 10); err != nil || n != 0 {
		t.Fatalf("TC-P704: starter re-imported %d (%v)", n, err)
	}
}

// TC-P705 (REQ-9, REQ-10, REQ-14): a run running under a live legacy lease
// is imported and taken over only after the lease, then the late legacy
// claimant's writes fail; an open approval becomes a wait that survives a
// restart and its decision takes exactly one route, also when the decision
// races the cutover.
func TestWorkflowRestartRunningAndWaitingApproval(t *testing.T) {
	t.Run("TC-P705/running with a live legacy lease", func(t *testing.T) {
		e := newWFEnv(t)
		run := e.legacyRun(workspaceA, "lease", "task:lease")
		legacy := e.legacyClaim(30 * time.Second)
		if legacy == nil || legacy.ID != run.ID {
			t.Fatalf("legacy claim %+v", legacy)
		}
		if _, err := e.store.Cutover(e.ctx, workspaceA, true); err != nil {
			t.Fatal(err)
		}
		// Restart: fresh stores.
		e.store, _ = NewWorkflowStore(e.pool)
		e.behaviors, _ = NewBehaviorStore(e.pool)
		e.reconcile(time.Now())
		if j, err := e.store.ClaimJob(e.ctx, time.Now().Add(10*time.Second)); err != nil || j != nil {
			t.Fatalf("TC-P705: job claimable during the legacy lease: %+v %v", j, err)
		}
		// The legacy claimant still owns the run until its lease ends.
		if err := e.behaviors.SetRunActions(e.ctx, run.ID, legacy.ClaimToken, json.RawMessage(`{"actions":[]}`)); err != nil {
			t.Fatalf("TC-P705: legacy write within its lease: %v", err)
		}
		at := time.Now().Add(31 * time.Second)
		job := e.claim(at)
		if _, err := e.behaviors.BeginDurableRunAttempt(e.ctx, fenceOf(job, at), run.ID, at.Add(10*time.Minute)); err != nil {
			t.Fatalf("TC-P705: durable takeover after the lease: %v", err)
		}
		if err := e.behaviors.FinishRun(e.ctx, run.ID, legacy.ClaimToken, behavior.RunDone, ""); !errors.Is(err, behavior.ErrStaleClaim) {
			t.Fatalf("TC-P705: late legacy finish: %v", err)
		}
		if err := e.behaviors.SetRunActions(e.ctx, run.ID, legacy.ClaimToken, json.RawMessage(`{"actions":[]}`)); err == nil {
			t.Fatal("TC-P705: late legacy write accepted")
		}
		if s, _ := e.runStatus(run.ID); s != "running" {
			t.Fatalf("TC-P705: run %s", s)
		}
	})
	t.Run("TC-P705/blocking approval becomes a wait", func(t *testing.T) {
		e := newWFEnv(t)
		run := e.legacyRun(workspaceA, "wait", "task:wait")
		e.setRunStatus(run.ID, "done")
		a := e.legacyApproval(workspaceA, run.ID, "ask")
		if _, err := e.store.Cutover(e.ctx, workspaceA, true); err != nil {
			t.Fatal(err)
		}
		e.store, _ = NewWorkflowStore(e.pool)
		e.behaviors, _ = NewBehaviorStore(e.pool)
		e.reconcile(time.Now())
		if n := e.count(`SELECT count(*) FROM workflow_waits WHERE approval_id=$1 AND closed_at IS NULL`, a.ID); n != 1 {
			t.Fatalf("TC-P705: open waits %d", n)
		}
		e.decide(workspaceA, a, behavior.DecisionApproved, time.Now())
		if n := e.count(`SELECT count(*) FROM workflow_signals WHERE reference_id=$1`, a.ID); n != 1 {
			t.Fatalf("TC-P705: %d signals for the decision", n)
		}
	})
	t.Run("TC-P705/decision races the cutover", func(t *testing.T) {
		for round := 0; round < 6; round++ {
			t.Run(strconv.Itoa(round), func(t *testing.T) {
				e := newWFEnv(t)
				run := e.legacyRun(workspaceA, "race", "task:race")
				e.setRunStatus(run.ID, "done")
				a := e.legacyApproval(workspaceA, run.ID, "ask")
				var cutErr, decErr error
				parallel(2, func(i int) {
					if i == 0 {
						_, cutErr = e.store.Cutover(e.ctx, workspaceA, true)
					} else {
						_, decErr = e.behaviors.Decide(e.ctx, workspaceA, a.Nonce, behavior.DecisionApproved, "", "owner", time.Now())
					}
				})
				if cutErr != nil || decErr != nil {
					t.Fatalf("round %d: cutover %v decide %v", round, cutErr, decErr)
				}
				waits := e.count(`SELECT count(*) FROM workflow_waits WHERE approval_id=$1`, a.ID)
				signals := e.count(`SELECT count(*) FROM workflow_signals WHERE reference_id=$1`, a.ID)
				// Bound before the decision: the signal route. Decided first: the
				// legacy route, no wait and no signal.
				if !(waits == 1 && signals == 1 || waits == 0 && signals == 0) {
					t.Fatalf("TC-P705 round %d: waits %d signals %d (two routes or none)", round, waits, signals)
				}
			})
		}
	})
}

// TC-P706 (REQ-12): the fence is per workspace: B stays legacy while A is
// cut over; each scheduler only serves its own workspaces.
func TestWorkflowCutoverWorkspaceIsolation(t *testing.T) {
	e := newWFEnv(t)
	runA := e.legacyRun(workspaceA, "iso", "task:a")
	if _, err := e.store.Cutover(e.ctx, workspaceA, true); err != nil {
		t.Fatal(err)
	}
	runB := e.legacyRun(workspaceB, "iso", "task:b")
	if e.mode(workspaceB) != RuntimeModeLegacy {
		t.Fatal("TC-P706: workspace B left legacy mode")
	}
	if n, err := e.store.StartPendingBehaviorRuns(e.ctx, workspaceB, time.Now(), 10); err != nil || n != 0 {
		t.Fatalf("TC-P706: durable starter took legacy workspace B: %d %v", n, err)
	}
	if _, ok := e.executionOfRun(runB.ID); ok {
		t.Fatal("TC-P706: B's run has an execution")
	}
	r := e.legacyClaim(time.Minute)
	if r == nil || r.ID != runB.ID || r.WorkspaceID != workspaceB {
		t.Fatalf("TC-P706: legacy claim %+v, want B's run %d", r, runB.ID)
	}
	if again := e.legacyClaim(time.Minute); again != nil {
		t.Fatalf("TC-P706: legacy claimer took A's run: %+v", again)
	}
	if _, ok := e.executionOfRun(runA.ID); !ok {
		t.Fatal("TC-P706: A's run not imported")
	}
	ws, err := e.store.WorkflowWorkspaces(e.ctx)
	if err != nil || len(ws) != 1 || ws[0] != workspaceA {
		t.Fatalf("TC-P706: workflow workspaces %v %v", ws, err)
	}
	if _, err := e.store.Cutover(e.ctx, "00000000-0000-0000-0000-0000000000ff", true); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("TC-P706: cutover of an unknown workspace: %v", err)
	}
}

// TC-P707 (REQ-8, REQ-11, P4 Q1/Q2): an execution that fails or is
// cancelled ends its run projection in the same transaction; a retry of a
// failed execution reopens the run as pending with a fresh attempt budget;
// a retry after an uncertain side effect is refused and changes nothing.
func TestWorkflowRunProjectionAndRetry(t *testing.T) {
	cases := []struct {
		name   string
		end    func(e *wfEnv, br bRun, job *workflow.Job)
		status string
		retry  error
	}{
		{"failed", func(e *wfEnv, br bRun, job *workflow.Job) {
			e.failJob(job, e.base, workflow.ReasonCode("model_invalid"))
		}, "failed", nil},
		{"uncertain", func(e *wfEnv, br bRun, job *workflow.Job) { e.failJob(job, e.base, workflow.ReasonSideEffectUncertain) }, "failed", workflow.ErrProviderUncertainty},
		{"cancelled", func(e *wfEnv, br bRun, job *workflow.Job) {
			if _, err := e.store.CancelExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: br.ex.ID, IdempotencyKey: "op-cancel-p707"}); err != nil {
				e.t.Fatal(err)
			}
		}, "canceled", workflow.ErrNotEligible},
	}
	for _, c := range cases {
		t.Run("TC-P707/"+c.name, func(t *testing.T) {
			e := newWFEnv(t)
			br := e.runExecution(workspaceA, "proj", "task:proj", nil)
			job := e.claim(e.base)
			if _, err := e.behaviors.BeginDurableRunAttempt(e.ctx, fenceOf(job, e.base), br.run.ID, e.base.Add(10*time.Minute)); err != nil {
				t.Fatal(err)
			}
			c.end(e, br, job)
			status, code := e.runStatus(br.run.ID)
			if status != c.status || code == "" {
				t.Fatalf("TC-P707: run projection %s/%q, want %s", status, code, c.status)
			}
			before := e.dbState()
			child, err := e.store.RetryExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: br.ex.ID, IdempotencyKey: "op-retry-" + strconv.FormatInt(br.ex.ID, 10)})
			if c.retry != nil {
				if !errors.Is(err, c.retry) {
					t.Fatalf("TC-P707: retry %v, want %v", err, c.retry)
				}
				if e.dbState() != before {
					t.Fatal("TC-P707: refused retry wrote rows")
				}
				return
			}
			if err != nil {
				t.Fatalf("TC-P707: retry: %v", err)
			}
			var attempts int
			_ = e.pool.QueryRow(e.ctx, `SELECT attempts FROM behavior_runs WHERE id=$1`, br.run.ID).Scan(&attempts)
			if s, _ := e.runStatus(br.run.ID); s != "pending" || attempts != 0 || child.References.LegacyRunID != br.run.ID {
				t.Fatalf("TC-P707: after retry run %s attempts %d child refs %+v", s, attempts, child.References)
			}
		})
	}
}
