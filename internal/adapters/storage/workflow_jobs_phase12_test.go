package storage

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/google/uuid"
)

// TC-10 (REQ-3): a job whose lease expired (crash) goes back to the queue at
// the next reconcile and is claimed again with a fresh token; TC-13: a second
// reconcile at the same time changes nothing.
func TestPhase12ExpiredWorkflowLeaseIsRequeuedOnceAndReconcileIsIdempotent(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "p12-lease")
	ex := e.start(workspaceA, v.ID, "p12-lease-1", chainDef("a"))
	first := e.claim(e.base)
	expiry := e.base.Add(16 * time.Minute)
	rec := workflow.NewReconciler(e.store)
	if err := rec.Reconcile(e.ctx, expiry); err != nil {
		t.Fatal(err)
	}
	jobs := e.jobs(ex.ID)
	if len(jobs) != 1 || jobs[0].Status != "pending" || jobs[0].LastError != string(workflow.ReasonLeaseExpired) || jobs[0].Attempts != 1 {
		t.Fatalf("TC-10: after reconcile %s", describeJobs(jobs))
	}
	assertState(t, e.execution(workspaceA, ex.ID), workflow.StateQueued, workflow.ReasonLeaseExpired)
	before := e.fingerprint(ex.ID)
	if err := rec.Reconcile(e.ctx, expiry); err != nil {
		t.Fatal(err)
	}
	if err := e.store.ReconcileJobs(e.ctx, expiry); err != nil {
		t.Fatal(err)
	}
	if after := e.fingerprint(ex.ID); after != before {
		t.Fatal("TC-13: a second reconcile changed the execution")
	}
	second := e.claim(expiry.Add(2 * time.Minute))
	if second.ID != first.ID || second.ClaimToken == first.ClaimToken || second.Attempts != 2 {
		t.Fatalf("TC-10: reclaim first=%s second=%s attempts=%d", first.ClaimToken, second.ClaimToken, second.Attempts)
	}
	if n := e.count(`SELECT count(*) FROM jobs WHERE execution_id=$1`, ex.ID); n != 1 {
		t.Fatalf("TC-10: %d job rows, want 1", n)
	}
}

// TC-11 (REQ-3): five crashes in a row exhaust the retry limit; the execution
// ends FAILED attempts_exhausted and the job is never queued again.
func TestPhase12ExhaustedLeaseExpiryFailsExecution(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "p12-exhaust")
	ex := e.start(workspaceA, v.ID, "p12-exhaust-1", chainDef("a"))
	at := e.base
	for attempt := 1; attempt <= 5; attempt++ {
		job := e.claim(at)
		if job.Attempts != attempt {
			t.Fatalf("TC-11: attempt %d claimed as %d", attempt, job.Attempts)
		}
		at = at.Add(16 * time.Minute)
		e.reconcile(at)
		at = at.Add(2 * time.Minute)
	}
	got := e.execution(workspaceA, ex.ID)
	if got.State != workflow.StateFailed || got.Reason != workflow.ReasonAttemptsExhausted {
		t.Fatalf("TC-11: execution %s/%s", got.State, got.Reason)
	}
	if j, err := e.store.ClaimJob(e.ctx, at.Add(time.Hour)); err != nil || j != nil {
		t.Fatalf("TC-11: exhausted job claimable again: %#v %v", j, err)
	}
	jobs := e.jobs(ex.ID)
	if len(jobs) != 1 || jobs[0].Status == "pending" || jobs[0].Status == "processing" {
		t.Fatalf("TC-11: job state %s", describeJobs(jobs))
	}
	before := e.fingerprint(ex.ID)
	e.reconcile(at.Add(time.Hour))
	if e.fingerprint(ex.ID) != before {
		t.Fatal("TC-13: reconcile changed a failed execution")
	}
}

// TC-12 (REQ-3): after a crash in step b, replay re-runs only step b; the
// finished step a keeps its single attempt and result.
func TestPhase12ReplayDoesNotRepeatFinishedStep(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "p12-replay")
	ex := e.start(workspaceA, v.ID, "p12-replay-1", chainDef("a", "b"))
	a := e.claim(e.base)
	if a.StepKey != "a" {
		t.Fatalf("first step %s", a.StepKey)
	}
	e.complete(a, e.base.Add(time.Second), reusable("1"), workflow.Usage{InputTokens: 4})
	e.reconcile(e.base.Add(2 * time.Second))
	b1 := e.claim(e.base.Add(2 * time.Second))
	if b1.StepKey != "b" {
		t.Fatalf("second step %s", b1.StepKey)
	}
	crash := e.base.Add(2*time.Second + 15*time.Minute + time.Second)
	e.reconcile(crash)
	b2 := e.claim(crash.Add(5 * time.Second))
	if b2.StepKey != "b" || b2.Attempts != 2 {
		t.Fatalf("TC-12: replay claimed %s attempt %d, want b attempt 2", b2.StepKey, b2.Attempts)
	}
	stepA := e.step(workspaceA, ex.ID, "a")
	if stepA.State != workflow.StepSucceeded || stepA.Attempts != 1 || stepA.Usage.InputTokens != 4 {
		t.Fatalf("TC-12: finished step a was touched by replay: %#v", stepA)
	}
}

// TC-17 (REQ-4): ReleaseJobLease ends only the caller's own live lease. A
// wrong or stale token, a repeat, and a job that is not leased are no-ops;
// bad input is an error; the released job is resumed at once by reconcile.
func TestPhase12ReleaseJobLeaseIsClaimFenced(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "p12-release")
	ex := e.start(workspaceA, v.ID, "p12-release-1", chainDef("a"))
	job := e.claim(e.base)
	at := e.base.Add(time.Second)
	fp := e.fingerprint(ex.ID)

	if err := e.store.ReleaseJobLease(e.ctx, job.ID, job.WorkspaceID, uuid.NewString(), at); err != nil {
		t.Fatalf("TC-17: wrong token: %v", err)
	}
	if err := e.store.ReleaseJobLease(e.ctx, job.ID, workspaceB, job.ClaimToken, at); err != nil {
		t.Fatalf("TC-17: wrong workspace: %v", err)
	}
	if e.fingerprint(ex.ID) != fp {
		t.Fatal("TC-17: a foreign release changed the job")
	}
	for name, call := range map[string]func() error{
		"zero job":      func() error { return e.store.ReleaseJobLease(e.ctx, 0, job.WorkspaceID, job.ClaimToken, at) },
		"bad token":     func() error { return e.store.ReleaseJobLease(e.ctx, job.ID, job.WorkspaceID, "not-a-uuid", at) },
		"bad workspace": func() error { return e.store.ReleaseJobLease(e.ctx, job.ID, "nope", job.ClaimToken, at) },
		"zero time": func() error {
			return e.store.ReleaseJobLease(e.ctx, job.ID, job.WorkspaceID, job.ClaimToken, time.Time{})
		},
	} {
		if call() == nil {
			t.Errorf("TC-17: %s accepted", name)
		}
	}
	if err := e.store.ReleaseJobLease(e.ctx, job.ID, job.WorkspaceID, job.ClaimToken, at); err != nil {
		t.Fatalf("TC-17: own release: %v", err)
	}
	var live bool
	if err := e.pool.QueryRow(e.ctx, `SELECT lease_expires_at > $2 FROM jobs WHERE id=$1`, job.ID, at).Scan(&live); err != nil || live {
		t.Fatalf("TC-17: lease still live after release: %v %v", live, err)
	}
	if jobs := e.jobs(ex.ID); len(jobs) != 1 || jobs[0].Status != "processing" || jobs[0].Token != job.ClaimToken {
		t.Fatalf("TC-17: release changed more than the lease: %s", describeJobs(jobs))
	}
	if err := e.store.ReleaseJobLease(e.ctx, job.ID, job.WorkspaceID, job.ClaimToken, at); err != nil {
		t.Fatalf("TC-17: repeat release: %v", err)
	}
	// Resumed at once: reconcile one second later, no 15-minute wait.
	e.reconcile(at.Add(time.Second))
	if jobs := e.jobs(ex.ID); len(jobs) != 1 || jobs[0].Status != "pending" {
		t.Fatalf("TC-4/TC-17: released job not requeued: %s", describeJobs(jobs))
	}
	again := e.claim(at.Add(2 * time.Minute))
	if again.ID != job.ID || again.ClaimToken == job.ClaimToken {
		t.Fatalf("TC-17: resumed claim %#v", again)
	}
	// The old holder must not end the new claimant's lease.
	if err := e.store.ReleaseJobLease(e.ctx, job.ID, job.WorkspaceID, job.ClaimToken, at.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(e.ctx, `SELECT lease_expires_at > $2 FROM jobs WHERE id=$1`, job.ID, at.Add(3*time.Minute)).Scan(&live); err != nil || !live {
		t.Fatalf("TC-17: stale release ended the new claimant's lease (live=%v err=%v)", live, err)
	}
}

// TC-21 (REQ-5): many claimers race over many jobs; every job is claimed
// exactly once, with its own token, and none is lost.
func TestPhase12ParallelClaimsAreDistinctAndComplete(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "p12-race")
	const n = 24
	want := map[int64]bool{}
	for i := 0; i < n; i++ {
		ex := e.start(workspaceA, v.ID, "p12-race-"+uuid.NewString(), chainDef("a"))
		want[ex.ID] = true
	}
	type got struct {
		exec  int64
		token string
		job   int64
	}
	results := make(chan got, n*2)
	parallel(8, func(int) {
		for k := 0; k < n; k++ {
			j, err := e.store.ClaimJob(e.ctx, e.base)
			if err != nil {
				if errors.Is(err, workflow.ErrStaleClaim) {
					continue
				}
				t.Errorf("TC-21: claim: %v", err)
				return
			}
			if j == nil {
				return
			}
			results <- got{j.ExecutionID, j.ClaimToken, j.ID}
		}
	})
	for {
		j, err := e.store.ClaimJob(e.ctx, e.base)
		if err != nil {
			t.Fatalf("TC-21: drain: %v", err)
		}
		if j == nil {
			break
		}
		results <- got{j.ExecutionID, j.ClaimToken, j.ID}
	}
	close(results)
	execs, tokens, jobs := map[int64]bool{}, map[string]bool{}, map[int64]bool{}
	for r := range results {
		if execs[r.exec] || tokens[r.token] || jobs[r.job] {
			t.Fatalf("TC-21: duplicate claim %#v", r)
		}
		execs[r.exec], tokens[r.token], jobs[r.job] = true, true, true
	}
	if len(execs) != n {
		t.Fatalf("TC-21: %d of %d executions claimed", len(execs), n)
	}
	for id := range want {
		if !execs[id] {
			t.Fatalf("TC-21: execution %d never claimed", id)
		}
	}
	if c := e.count(`SELECT count(*) FROM jobs WHERE status='processing'`); c != n {
		t.Fatalf("TC-21: %d processing rows, want %d", c, n)
	}
}

// TC-23 (REQ-5): after a lease was reclaimed, the old claim token can write
// nothing (all fenced writes fail with ErrStaleClaim, no row changes) and the
// new claimant finishes normally.
func TestPhase12StaleClaimAfterReclaimCannotWrite(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "p12-stale")
	ex := e.start(workspaceA, v.ID, "p12-stale-1", chainDef("a"))
	old := e.claim(e.base)
	expiry := e.base.Add(16 * time.Minute)
	e.reconcile(expiry)
	fresh := e.claim(expiry.Add(2 * time.Minute))
	if fresh.ClaimToken == old.ClaimToken {
		t.Fatal("TC-23: token not renewed")
	}
	e.fencedWrites(t, "TC-23 old token after reclaim", old, expiry.Add(3*time.Minute))
	e.complete(fresh, expiry.Add(3*time.Minute), reusable("1"), workflow.Usage{})
	if got := e.execution(workspaceA, ex.ID); got.State != workflow.StateCompleted {
		t.Fatalf("TC-23: execution %s", got.State)
	}
}

func claimAll(t *testing.T, e *wfEnv, at time.Time) (perWorkspace map[string]int, executions []int64) {
	t.Helper()
	perWorkspace = map[string]int{}
	for i := 0; i < 50; i++ {
		j, err := e.store.ClaimJob(e.ctx, at)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if j == nil {
			return
		}
		perWorkspace[j.WorkspaceID]++
		executions = append(executions, j.ExecutionID)
	}
	t.Fatal("claim loop did not end")
	return
}

// TC-33 (REQ-7): the cap counts live claims per workspace; the other
// workspace is not blocked; a freed slot (finished job or expired lease)
// makes the next job claimable; 0 and negative values turn the cap off.
func TestPhase12WorkspaceCapIsPerWorkspace(t *testing.T) {
	e := newWFEnv(t)
	va := e.enabledVersion(workspaceA, "p12-cap")
	vb := e.enabledVersion(workspaceB, "p12-cap")
	for i := 0; i < 4; i++ {
		e.start(workspaceA, va.ID, "p12-cap-a-"+uuid.NewString(), chainDef("a"))
	}
	for i := 0; i < 2; i++ {
		e.start(workspaceB, vb.ID, "p12-cap-b-"+uuid.NewString(), chainDef("a"))
	}
	e.store.SetWorkspaceMaxConcurrent(2)
	per, _ := claimAll(t, e, e.base)
	if per[workspaceA] != 2 || per[workspaceB] != 2 {
		t.Fatalf("TC-33: claims per workspace = %v, want A=2 B=2", per)
	}
	// An expired lease frees the slot (the cap counts live claims only).
	per, _ = claimAll(t, e, e.base.Add(16*time.Minute))
	if per[workspaceA] != 2 {
		t.Fatalf("TC-33: expired leases did not free slots: A claimed %d, want 2", per[workspaceA])
	}
}

// TC-33 (REQ-7): 0 and negative values turn the cap off. One env per case:
// each env holds the test schema lock until cleanup.
func TestPhase12WorkspaceCapOff(t *testing.T) {
	for _, n := range []int{0, -3} {
		n := n
		t.Run(fmt.Sprintf("cap_%d", n), func(t *testing.T) {
			e := newWFEnv(t)
			v := e.enabledVersion(workspaceA, "p12-capoff")
			for i := 0; i < 6; i++ {
				e.start(workspaceA, v.ID, "p12-capoff-"+uuid.NewString(), chainDef("a"))
			}
			e.store.SetWorkspaceMaxConcurrent(n)
			per, _ := claimAll(t, e, e.base)
			if per[workspaceA] != 6 {
				t.Fatalf("TC-33: cap %d claimed %d of 6", n, per[workspaceA])
			}
		})
	}
}

// TC-33 (REQ-7): resume jobs and the owner's own executions are claimed
// first and never held back by a full workspace. They still count as live
// claims, so with cap 1 no normal job starts while their leases are live;
// once they finish, normal jobs follow in FIFO order.
func TestPhase12WorkspaceCapExemptions(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "p12-exempt")
	normal1 := e.start(workspaceA, v.ID, "p12-exempt-1", chainDef("a"))
	normal2 := e.start(workspaceA, v.ID, "p12-exempt-2", chainDef("a"))
	local := e.start(workspaceA, v.ID, "p12-exempt-3", chainDef("a"))
	resume := e.start(workspaceA, v.ID, "p12-exempt-4", chainDef("a"))
	orch := e.start(workspaceA, v.ID, "p12-exempt-5", chainDef("a"))
	mustExec(t, e.ctx, e.pool, `UPDATE workflow_executions SET source_kind='local_task' WHERE id=$1`, local.ID)
	mustExec(t, e.ctx, e.pool, `UPDATE workflow_executions SET source_kind='orchestrated_task' WHERE id=$1`, orch.ID)
	// A real resume job continues an attempt already counted (>= 1) on both
	// the job and the step, which the result fence requires.
	mustExec(t, e.ctx, e.pool, `UPDATE jobs SET continues_attempt=true, attempts=1 WHERE execution_id=$1`, resume.ID)
	mustExec(t, e.ctx, e.pool, `UPDATE execution_steps SET attempts=1 WHERE execution_id=$1`, resume.ID)
	e.store.SetWorkspaceMaxConcurrent(1)
	var urgent []*workflow.Job
	got := map[int64]bool{}
	for i := 0; i < 3; i++ {
		j := e.claim(e.base)
		urgent = append(urgent, j)
		got[j.ExecutionID] = true
	}
	for name, id := range map[string]int64{"local_task": local.ID, "resume": resume.ID, "orchestrated_task": orch.ID} {
		if !got[id] {
			t.Fatalf("TC-33: exempt %s was held back by the cap (claimed %v)", name, got)
		}
	}
	if j, err := e.store.ClaimJob(e.ctx, e.base); err != nil || j != nil {
		t.Fatalf("TC-33: normal job claimed while urgent leases are live: %#v %v", j, err)
	}
	for _, j := range urgent {
		e.complete(j, e.base, nil, workflow.Usage{})
	}
	first := e.claim(e.base)
	if first.ExecutionID != normal1.ID {
		t.Fatalf("TC-33: first normal claim is execution %d, want %d (FIFO)", first.ExecutionID, normal1.ID)
	}
	if j, err := e.store.ClaimJob(e.ctx, e.base); err != nil || j != nil {
		t.Fatalf("TC-33: second normal job claimed beyond cap 1: %#v %v", j, err)
	}
	e.complete(first, e.base, nil, workflow.Usage{})
	if second := e.claim(e.base); second.ExecutionID != normal2.ID {
		t.Fatalf("TC-33: second normal claim is execution %d, want %d", second.ExecutionID, normal2.ID)
	}
}

// TC-31 (REQ-7): CountPendingWork counts only jobs waiting for a claim,
// hub-wide.
func TestPhase12CountPendingWork(t *testing.T) {
	e := newWFEnv(t)
	va := e.enabledVersion(workspaceA, "p12-count")
	vb := e.enabledVersion(workspaceB, "p12-count")
	n, err := e.store.CountPendingWork(e.ctx)
	if err != nil || n != 0 {
		t.Fatalf("empty: %d %v", n, err)
	}
	e.start(workspaceA, va.ID, "p12-count-a1", chainDef("a"))
	e.start(workspaceA, va.ID, "p12-count-a2", chainDef("a"))
	e.start(workspaceB, vb.ID, "p12-count-b1", chainDef("a"))
	if n, err := e.store.CountPendingWork(e.ctx); err != nil || n != 3 {
		t.Fatalf("TC-31: pending = %d %v, want 3 across workspaces", n, err)
	}
	e.claim(e.base)
	if n, err := e.store.CountPendingWork(e.ctx); err != nil || n != 2 {
		t.Fatalf("TC-31: pending after one claim = %d %v, want 2", n, err)
	}
}

// TC-31 (REQ-7): under backpressure only the owner's Telegram command runs
// start; the others stay pending and start after the queue drains.
func TestPhase12UrgentStartOnlyStartsTelegramCommandRuns(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "p12-urgent")
	if _, err := e.store.Cutover(e.ctx, workspaceA, true); err != nil {
		t.Fatal(err)
	}
	mk := func(kind, subject string) behavior.Run {
		run, created, err := e.behaviors.CreateRun(e.ctx, workspaceA, behavior.NewRun{
			BehaviorID: v.BehaviorID, VersionID: v.ID, Version: v.Version, TriggerKind: kind,
			Trigger: []byte(`{}`), Subject: subject, Revision: "r1", RunAt: time.Now().Add(-time.Minute),
		})
		if err != nil || !created {
			t.Fatalf("create run %s: created=%v err=%v", subject, created, err)
		}
		return run
	}
	webhook := mk(behavior.TriggerCommentAdded, "p12:webhook")
	command := mk(behavior.TriggerTelegramCommand, "p12:command")
	started := func(id int64) bool {
		return e.count(`SELECT count(*) FROM workflow_executions WHERE legacy_run_id=$1`, id) == 1
	}
	n, err := e.store.StartPendingUrgentBehaviorRuns(e.ctx, workspaceA, time.Now(), 10)
	if err != nil || n != 1 {
		t.Fatalf("TC-31: urgent start = %d %v, want 1", n, err)
	}
	if !started(command.ID) || started(webhook.ID) {
		t.Fatalf("TC-31: command started=%v webhook started=%v, want true/false", started(command.ID), started(webhook.ID))
	}
	if n, err := e.store.StartPendingUrgentBehaviorRuns(e.ctx, workspaceA, time.Now(), 10); err != nil || n != 0 {
		t.Fatalf("TC-31: second urgent start = %d %v, want 0", n, err)
	}
	if st := e.count(`SELECT count(*) FROM behavior_runs WHERE id=$1 AND status='pending'`, webhook.ID); st != 1 {
		t.Fatal("TC-31: deferred run is not pending any more")
	}
	// Drain: the normal starter picks the deferred run up.
	if n, err := e.store.StartPendingBehaviorRuns(e.ctx, workspaceA, time.Now(), 10); err != nil || n != 1 || !started(webhook.ID) {
		t.Fatalf("TC-31: drain start = %d %v started=%v", n, err, started(webhook.ID))
	}
}
