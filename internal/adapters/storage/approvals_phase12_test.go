package storage

import (
	"errors"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// restartStores models a hub restart: fresh store instances over the same DB.
func (e *wfEnv) restartStores() {
	e.t.Helper()
	ws, err := NewWorkflowStore(e.pool)
	if err != nil {
		e.t.Fatal(err)
	}
	bs, err := NewBehaviorStore(e.pool)
	if err != nil {
		e.t.Fatal(err)
	}
	e.store, e.behaviors = ws, bs
}

// TC-28 (REQ-6): an approval decided before the crash is applied once after
// it: every reconcile (also repeated) leaves one signal and one wake job, the
// step resumes once in the same attempt, and nothing is queued again after.
func TestPhase12DecidedBeforeCrashResumesOnce(t *testing.T) {
	e := newWFEnv(t)
	br, job, aps := e.waitingRun(workspaceA, "task:p12-decided", nil, behavior.ApprovalPostDraft)
	e.decide(workspaceA, aps[0], behavior.DecisionApproved, e.base.Add(time.Minute))
	// The hub dies here; it restarts three times in a row.
	for i := 0; i < 3; i++ {
		e.restartStores()
		e.reconcile(e.base.Add(time.Duration(i+2) * time.Minute))
		if e.signalCount(br.ex.ID) != 1 {
			t.Fatalf("TC-28: restart %d: %d signals, want 1", i, e.signalCount(br.ex.ID))
		}
		if js := e.activeJobs(br.ex.ID); len(js) != 1 {
			t.Fatalf("TC-28: restart %d: jobs %s, want one wake job", i, describeJobs(js))
		}
	}
	at := e.base.Add(10 * time.Minute)
	resumed := e.claim(at)
	if resumed.ExecutionID != br.ex.ID || resumed.StepKey != job.StepKey || resumed.Attempts != job.Attempts {
		t.Fatalf("TC-28: resumed %d/%s attempt %d, want %d/%s attempt %d", resumed.ExecutionID, resumed.StepKey, resumed.Attempts, br.ex.ID, job.StepKey, job.Attempts)
	}
	if j, err := e.store.ClaimJob(e.ctx, at); err != nil || j != nil {
		t.Fatalf("TC-28: the decision woke the step twice: %#v %v", j, err)
	}
}

// TC-28 (REQ-6, human): an open approval stays open and undecided through
// several restarts and reconciles, and the owner can still decide it.
func TestPhase12OpenApprovalSurvivesRepeatedRestarts(t *testing.T) {
	e := newWFEnv(t)
	br, _, aps := e.waitingRun(workspaceA, "task:p12-open", nil, behavior.ApprovalPostDraft)
	for i := 1; i <= 3; i++ {
		e.restartStores()
		e.reconcile(e.base.Add(time.Duration(i) * time.Hour))
		a, err := e.behaviors.GetApproval(e.ctx, workspaceA, aps[0].ID)
		if err != nil || a.Decision != "" {
			t.Fatalf("TC-28: restart %d: approval %+v %v", i, a, err)
		}
		if j, err := e.store.ClaimJob(e.ctx, e.base.Add(time.Duration(i)*time.Hour)); err != nil || j != nil {
			t.Fatalf("TC-28: open approval is runnable: %#v %v", j, err)
		}
	}
	assertState(t, e.execution(workspaceA, br.ex.ID), workflow.StateWaitingForApproval, "")
	late := e.base.Add(4 * time.Hour)
	d, err := e.behaviors.Decide(e.ctx, workspaceA, aps[0].Nonce, behavior.DecisionApproved, "", "owner", late)
	if err != nil || d.Decision != behavior.DecisionApproved {
		t.Fatalf("TC-28: decision after restarts: %+v %v", d, err)
	}
}

// TC-29 (REQ-6, abuse): an approval pressed after its expiry never counts. The
// press is refused with ErrExpired, the sweep expires it, the woken step sees
// "expired", and later presses (also approvals) change nothing.
func TestPhase12ExpiredApprovalNeverApproves(t *testing.T) {
	e := newWFEnv(t)
	br, job, aps := e.waitingRun(workspaceA, "task:p12-expired", nil, behavior.ApprovalPostDraft)
	late := aps[0].ExpiresAt.Add(time.Minute)
	if _, err := e.behaviors.Decide(e.ctx, workspaceA, aps[0].Nonce, behavior.DecisionApproved, "", "owner", late); !errors.Is(err, behavior.ErrExpired) {
		t.Fatalf("TC-29: late approve = %v, want ErrExpired", err)
	}
	if a, _ := e.behaviors.GetApproval(e.ctx, workspaceA, aps[0].ID); a.Decision == behavior.DecisionApproved {
		t.Fatal("TC-29: a late press approved")
	}
	if e.signalCount(br.ex.ID) != 0 {
		t.Fatal("TC-29: a refused press recorded a signal")
	}
	expired, err := e.behaviors.ExpireDue(e.ctx, late, 10)
	if err != nil || len(expired) != 1 || expired[0].ID != aps[0].ID {
		t.Fatalf("TC-29: ExpireDue = %+v %v", expired, err)
	}
	var sd string
	if err := e.pool.QueryRow(e.ctx, `SELECT decision FROM workflow_signals WHERE execution_id=$1`, br.ex.ID).Scan(&sd); err != nil || sd != string(behavior.DecisionExpired) {
		t.Fatalf("TC-29: signal decision %q %v", sd, err)
	}
	for _, d := range []behavior.Decision{behavior.DecisionApproved, behavior.DecisionRejected} {
		if _, err := e.behaviors.Decide(e.ctx, workspaceA, aps[0].Nonce, d, "", "owner", late); err == nil {
			t.Fatalf("TC-29: %s accepted after expiry", d)
		}
	}
	if n, err := e.behaviors.ExpireDue(e.ctx, late.Add(time.Hour), 10); err != nil || len(n) != 0 {
		t.Fatalf("TC-29: second sweep expired %d (%v)", len(n), err)
	}
	if e.signalCount(br.ex.ID) != 1 {
		t.Fatalf("TC-29: %d signals after repeated presses", e.signalCount(br.ex.ID))
	}
	resumed := e.claim(late)
	if resumed.StepKey != job.StepKey {
		t.Fatalf("TC-29: woke %s", resumed.StepKey)
	}
	waits, err := e.behaviors.ListStepWaits(e.ctx, workspaceA, br.ex.ID, job.StepKey)
	if err != nil || len(waits) != 1 || waits[0].Approval.Decision != behavior.DecisionExpired {
		t.Fatalf("TC-29: the woken step sees %#v %v, want expired", waits, err)
	}
}

// TC-29 (REQ-6, human): a double tap on Approve applies once: one signal, one
// wake job, one resumed claim.
func TestPhase12DoubleApproveAppliesOnce(t *testing.T) {
	e := newWFEnv(t)
	br, _, aps := e.waitingRun(workspaceA, "task:p12-double", nil, behavior.ApprovalPostDraft)
	at := e.base.Add(time.Minute)
	parallel(5, func(int) {
		_, _ = e.behaviors.Decide(e.ctx, workspaceA, aps[0].Nonce, behavior.DecisionApproved, "", "owner", at)
	})
	if e.signalCount(br.ex.ID) != 1 || len(e.activeJobs(br.ex.ID)) != 1 {
		t.Fatalf("TC-29: signals %d jobs %s", e.signalCount(br.ex.ID), describeJobs(e.activeJobs(br.ex.ID)))
	}
	e.claim(at)
	if j, err := e.store.ClaimJob(e.ctx, at); err != nil || j != nil {
		t.Fatalf("TC-29: applied twice: %#v %v", j, err)
	}
}

// TC-30 (REQ-6): a decision recorded while the hub was down without its wake
// (the signal write was lost) is repaired by the periodic sweep: the step is
// woken once, repeated sweeps add nothing.
func TestPhase12LostWakeIsRepairedBySweepOnce(t *testing.T) {
	e := newWFEnv(t)
	br, _, aps := e.waitingRun(workspaceA, "task:p12-sweep", nil, behavior.ApprovalPostDraft)
	mustExec(t, e.ctx, e.pool, `UPDATE approvals SET decision='approved', decided_by='owner', decided_at=now() WHERE id=$1`, aps[0].ID)
	if len(e.activeJobs(br.ex.ID)) != 0 {
		t.Fatal("TC-30: setup: step already queued")
	}
	e.restartStores()
	for i := 1; i <= 3; i++ {
		e.reconcile(e.base.Add(time.Duration(i) * time.Minute))
	}
	if e.signalCount(br.ex.ID) != 1 || len(e.activeJobs(br.ex.ID)) != 1 {
		t.Fatalf("TC-30: signals %d jobs %s", e.signalCount(br.ex.ID), describeJobs(e.activeJobs(br.ex.ID)))
	}
}

// TC-30 (REQ-6): a policy gate approved before the crash merges once after the
// restart (replayed any number of times); the gate sweep never touches a
// decided gate.
func TestPhase12ApprovedGateMergesOnceAfterRestart(t *testing.T) {
	m := newMGEnv(t, mgOpts{})
	rec, ref := m.approved(t)
	if rec.Decision != behavior.DecisionApproved {
		t.Fatalf("TC-30: gate %+v", rec)
	}
	m.build(mgOpts{})
	for i := 0; i < 3; i++ {
		out, err := m.actions.ResumeTask(m.ctx, m.task(), ref)
		if err != nil || !out.Succeeded {
			t.Fatalf("TC-30: resume %d: %+v %v", i, out, err)
		}
	}
	if _, merges := m.gh.state(); merges != 1 {
		t.Fatalf("TC-30: %d merges, want 1", merges)
	}
	if n, err := m.gates.Sweep(m.ctx); err != nil || n != 0 {
		t.Fatalf("TC-30: the gate sweep touched a decided gate: %d %v", n, err)
	}
}
