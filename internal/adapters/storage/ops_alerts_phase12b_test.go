package storage

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

type p12bKey struct {
	exec  int64
	state behavior.AlertState
}

// TC-B-33, TC-B-34, TC-B-35, TC-B-42, TC-B-44 (REQ-10)
func TestPhase12bAlertStore(t *testing.T) {
	e := newWFEnv(t)
	ctx := e.ctx
	ops, err := NewOpsStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Second)
	ago := func(d time.Duration) time.Time { return base.Add(-d) }
	va := e.enabledVersion(workspaceA, "alert-a")
	vb := e.enabledVersion(workspaceB, "alert-b")
	ex := func(ws string, v int64, name string) int64 { return p12bExec(e, ws, v, "alert:"+name) }
	gate := func(execID int64, expires time.Time, decided bool, n string) {
		t.Helper()
		if decided {
			mustExec(t, ctx, e.pool, `INSERT INTO approvals(workspace_id,kind,nonce,expires_at,policy_action,resource_key,input_sha256,execution_id,decision,decided_at)
				VALUES($1,'policy_gate',$2,$3,'github.comment',$5,'',$4,'approved',now())`, workspaceA, "gate-nonce-"+n+"-0123456", expires, execID, "k-"+n)
			return
		}
		mustExec(t, ctx, e.pool, `INSERT INTO approvals(workspace_id,kind,nonce,expires_at,policy_action,resource_key,input_sha256,execution_id)
			VALUES($1,'policy_gate',$2,$3,'github.comment',$5,'',$4)`, workspaceA, "gate-nonce-"+n+"-0123456", expires, execID, "k-"+n)
	}

	// stuck_running: a processing job unchanged for the threshold.
	run15 := ex(workspaceA, va.ID, "run-over") // 16 min: alerts
	run14 := ex(workspaceA, va.ID, "run-under")
	runDone := ex(workspaceA, va.ID, "run-ended")
	p12bSetState(e, run15, "RUNNING")
	p12bSetState(e, run14, "RUNNING")
	p12bJob(e, run15, "processing", ago(time.Hour), ago(16*time.Minute))
	mustExec(t, ctx, e.pool, `UPDATE jobs SET last_error_code='lease_expired' WHERE execution_id=$1`, run15)
	p12bJob(e, run14, "processing", ago(time.Hour), ago(14*time.Minute))
	p12bSetState(e, runDone, "COMPLETED")
	p12bJob(e, runDone, "processing", ago(time.Hour), ago(time.Hour))

	// stuck_queued: a pending job due for the threshold.
	q31 := ex(workspaceA, va.ID, "queue-over")
	q29 := ex(workspaceA, va.ID, "queue-under")
	qFuture := ex(workspaceA, va.ID, "queue-future")
	qDone := ex(workspaceA, va.ID, "queue-ended")
	p12bJob(e, q31, "pending", ago(31*time.Minute), ago(31*time.Minute))
	p12bJob(e, q29, "pending", ago(29*time.Minute), ago(29*time.Minute))
	p12bJob(e, qFuture, "pending", base.Add(time.Hour), base)
	p12bSetState(e, qDone, "CANCELLED")
	p12bJob(e, qDone, "pending", ago(3*time.Hour), ago(3*time.Hour))

	// approval_overdue: WAITING_FOR_APPROVAL with an open bound approval
	// that expired before the 5-minute grace.
	ap6 := ex(workspaceA, va.ID, "appr-over")
	ap4 := ex(workspaceA, va.ID, "appr-under")
	apDecided := ex(workspaceA, va.ID, "appr-decided")
	apWait := ex(workspaceA, va.ID, "appr-wait")
	apNotWaiting := ex(workspaceA, va.ID, "appr-notwaiting")
	apTwo := ex(workspaceA, va.ID, "appr-two")
	for _, id := range []int64{ap6, ap4, apDecided, apWait, apTwo} {
		p12bSetState(e, id, "WAITING_FOR_APPROVAL")
	}
	gate(ap6, ago(6*time.Minute), false, "a6")
	gate(ap4, ago(4*time.Minute), false, "a4")
	gate(apDecided, ago(time.Hour), true, "ad")
	gate(apNotWaiting, ago(time.Hour), false, "an") // execution not waiting
	gate(apTwo, ago(20*time.Minute), false, "t1")
	gate(apTwo, ago(40*time.Minute), false, "t2")
	var waitApproval int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO approvals(workspace_id,kind,nonce,expires_at) VALUES($1,'confirm_action','plain-nonce-wait-000001',$2) RETURNING id`, workspaceA, ago(10*time.Minute)).Scan(&waitApproval); err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, e.pool, `INSERT INTO workflow_waits(workspace_id,execution_id,step_key,approval_id,wait_kind) VALUES($1,$2,'s1',$3,'approval')`, workspaceA, apWait, waitApproval)

	// failed: finished FAILED within the 24-hour lookback.
	f1h := ex(workspaceA, va.ID, "failed-recent")
	f25h := ex(workspaceA, va.ID, "failed-old")
	completed := ex(workspaceA, va.ID, "completed")
	cancelled := ex(workspaceA, va.ID, "cancelled")
	p12bSetState(e, f1h, "FAILED")
	mustExec(t, ctx, e.pool, `UPDATE workflow_executions SET finished_at=$2, reason_code='attempts_exhausted' WHERE id=$1`, f1h, ago(time.Hour))
	p12bSetState(e, f25h, "FAILED")
	mustExec(t, ctx, e.pool, `UPDATE workflow_executions SET finished_at=$2 WHERE id=$1`, f25h, ago(25*time.Hour))
	p12bSetState(e, completed, "COMPLETED")
	p12bSetState(e, cancelled, "CANCELLED")
	// Another workspace is listed hub-wide.
	bRun := ex(workspaceB, vb.ID, "b-run")
	p12bSetState(e, bRun, "RUNNING")
	p12bJob(e, bRun, "processing", ago(time.Hour), ago(2*time.Hour))

	q := behavior.AlertQuery{
		RunningBefore: ago(15 * time.Minute), QueuedBefore: ago(30 * time.Minute),
		ApprovalBefore: ago(5 * time.Minute), FailedSince: ago(24 * time.Hour), Limit: 100,
	}
	list, err := ops.ListAlertCandidates(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	got := map[p12bKey]behavior.WorkflowAlert{}
	for _, a := range list {
		k := p12bKey{a.ExecutionID, a.State}
		if _, dup := got[k]; dup {
			t.Fatalf("duplicate candidate %v", k)
		}
		got[k] = a
	}
	want := []p12bKey{
		{run15, behavior.AlertStuckRunning}, {q31, behavior.AlertStuckQueued},
		{ap6, behavior.AlertApprovalOverdue}, {apWait, behavior.AlertApprovalOverdue}, {apTwo, behavior.AlertApprovalOverdue},
		{f1h, behavior.AlertFailed}, {bRun, behavior.AlertStuckRunning},
	}
	if len(got) != len(want) {
		t.Fatalf("candidates = %d, want %d: %+v", len(got), len(want), list)
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("missing candidate %v", k)
		}
	}
	// TC-B-33 thresholds, TC-B-34 grace/binding, TC-B-35 lookback: the
	// "under" and non-matching rows above are absent (len check), and the
	// values of the present ones are right.
	if a := got[p12bKey{run15, behavior.AlertStuckRunning}]; a.ErrorCode != "lease_expired" || a.BehaviorName != "alert-a" || a.WorkspaceID != workspaceA || !a.Since.Equal(ago(16*time.Minute)) {
		t.Errorf("stuck_running = %+v", a)
	}
	if a := got[p12bKey{q31, behavior.AlertStuckQueued}]; !a.Since.Equal(ago(31 * time.Minute)) {
		t.Errorf("stuck_queued since = %v", a.Since)
	}
	if a := got[p12bKey{apTwo, behavior.AlertApprovalOverdue}]; !a.Since.Equal(ago(40 * time.Minute)) {
		t.Errorf("approval_overdue must use the earliest expiry: %v", a.Since)
	}
	if a := got[p12bKey{f1h, behavior.AlertFailed}]; a.ErrorCode != "attempts_exhausted" || !a.Since.Equal(ago(time.Hour)) {
		t.Errorf("failed = %+v", a)
	}
	if a := got[p12bKey{bRun, behavior.AlertStuckRunning}]; a.WorkspaceID != workspaceB || a.BehaviorName != "alert-b" {
		t.Errorf("workspace B candidate = %+v", a)
	}
	// Oldest first.
	for i := 1; i < len(list); i++ {
		if list[i].Since.Before(list[i-1].Since) {
			t.Fatalf("not oldest first at %d: %v then %v", i, list[i-1].Since, list[i].Since)
		}
	}
	// Custom thresholds move the boundary; tighter ones include the "under" rows.
	tight := q
	tight.RunningBefore, tight.QueuedBefore, tight.ApprovalBefore = ago(10*time.Minute), ago(20*time.Minute), ago(3*time.Minute)
	more, err := ops.ListAlertCandidates(ctx, tight)
	if err != nil {
		t.Fatal(err)
	}
	if len(more) != len(want)+3 {
		t.Fatalf("tight thresholds: %d candidates, want %d", len(more), len(want)+3)
	}
	// The limit keeps the oldest rows.
	lim := q
	lim.Limit = 2
	two, err := ops.ListAlertCandidates(ctx, lim)
	if err != nil || len(two) != 2 || two[0].ExecutionID != list[0].ExecutionID || two[1].ExecutionID != list[1].ExecutionID {
		t.Fatalf("limit 2: %+v %v", two, err)
	}

	// TC-B-42 dedup: claim once, rescan lists it no more, release re-lists it.
	a := got[p12bKey{run15, behavior.AlertStuckRunning}]
	if ok, err := ops.ClaimAlert(ctx, a, base); err != nil || !ok {
		t.Fatalf("first claim: %v %v", ok, err)
	}
	if ok, err := ops.ClaimAlert(ctx, a, base); err != nil || ok {
		t.Fatalf("second claim: %v %v", ok, err)
	}
	after, _ := ops.ListAlertCandidates(ctx, q)
	if len(after) != len(want)-1 {
		t.Fatalf("claimed row still listed: %d", len(after))
	}
	if err := ops.ReleaseAlert(ctx, a); err != nil {
		t.Fatal(err)
	}
	if again, _ := ops.ListAlertCandidates(ctx, q); len(again) != len(want) {
		t.Fatalf("released row not listed again: %d", len(again))
	}
	if ok, err := ops.ClaimAlert(ctx, a, base); err != nil || !ok {
		t.Fatalf("claim after release: %v %v", ok, err)
	}
	// Another state of the same execution is its own alert.
	other := a
	other.State = behavior.AlertFailed
	if ok, err := ops.ClaimAlert(ctx, other, base); err != nil || !ok {
		t.Fatalf("other state claim: %v %v", ok, err)
	}
	// Ten racing claims: exactly one wins.
	racing := got[p12bKey{q31, behavior.AlertStuckQueued}]
	var wins atomic.Int32
	parallel(10, func(int) {
		if ok, err := ops.ClaimAlert(ctx, racing, base); err == nil && ok {
			wins.Add(1)
		}
	})
	if wins.Load() != 1 {
		t.Fatalf("racing claims won %d times", wins.Load())
	}
	// Bad input.
	if _, err := ops.ClaimAlert(ctx, behavior.WorkflowAlert{WorkspaceID: "x", ExecutionID: 1, State: behavior.AlertFailed}, base); err == nil {
		t.Error("claim accepted a bad workspace id")
	}
	if _, err := ops.ClaimAlert(ctx, behavior.WorkflowAlert{WorkspaceID: workspaceA, ExecutionID: 999999, State: behavior.AlertFailed}, base); err == nil {
		t.Error("claim accepted an unknown execution (foreign key)")
	}
	if _, err := ops.ClaimAlert(ctx, behavior.WorkflowAlert{WorkspaceID: workspaceA, ExecutionID: run15, State: "bogus"}, base); err == nil {
		t.Error("claim accepted an unknown state (check)")
	}

	// TC-B-42 open-alert count. Claimed so far: run15 stuck_running (live),
	// run15 failed (sent now), q31 stuck_queued (live). Add more.
	claim := func(ws string, id int64, st behavior.AlertState, at time.Time) {
		t.Helper()
		if ok, err := ops.ClaimAlert(ctx, behavior.WorkflowAlert{WorkspaceID: ws, ExecutionID: id, State: st}, at); err != nil || !ok {
			t.Fatalf("claim %d %s: %v %v", id, st, ok, err)
		}
	}
	claim(workspaceA, runDone, behavior.AlertStuckRunning, base) // execution ended: not open
	claim(workspaceA, f1h, behavior.AlertFailed, base)           // within lookback: open
	claim(workspaceA, f25h, behavior.AlertFailed, ago(48*time.Hour))
	claim(workspaceB, bRun, behavior.AlertStuckRunning, base) // other workspace
	since := ago(24 * time.Hour)
	hub, err := ops.OpenAlertCount(ctx, "", since)
	if err != nil || hub != 5 {
		t.Fatalf("hub open alerts = %d %v, want 5 (run15 stuck, run15 failed, q31, f1h failed, bRun)", hub, err)
	}
	if n, _ := ops.OpenAlertCount(ctx, workspaceA, since); n != 4 {
		t.Fatalf("workspace A open alerts = %d, want 4", n)
	}
	if n, _ := ops.OpenAlertCount(ctx, workspaceB, since); n != 1 {
		t.Fatalf("workspace B open alerts = %d, want 1", n)
	}
	if _, err := ops.OpenAlertCount(ctx, "x", since); err == nil {
		t.Error("OpenAlertCount accepted a bad workspace id")
	}

	// TC-B-44 migration 00039: foreign key without cascade, RLS, down and up.
	var delType string
	if err := e.pool.QueryRow(ctx, `SELECT confdeltype::text FROM pg_constraint WHERE conrelid='public.workflow_alerts'::regclass AND contype='f' AND confrelid='public.workflow_executions'::regclass`).Scan(&delType); err != nil || delType != "a" {
		t.Fatalf("workflow_alerts execution FK delete rule = %q %v, want no action (retention must delete alert rows first)", delType, err)
	}
	var rls bool
	if err := e.pool.QueryRow(ctx, `SELECT relrowsecurity FROM pg_class WHERE oid='public.workflow_alerts'::regclass`).Scan(&rls); err != nil || !rls {
		t.Fatalf("RLS on workflow_alerts: %v %v", rls, err)
	}
	mustExec(t, ctx, e.pool, downSection(t, "00039_workflow_alerts.sql"))
	if _, err := e.pool.Exec(ctx, `SELECT 1 FROM workflow_alerts`); err == nil {
		t.Fatal("table still exists after Down")
	}
	mustExec(t, ctx, e.pool, upSection(t, "00039_workflow_alerts.sql"))
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM workflow_alerts`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after Up: %d %v", n, err)
	}
}
