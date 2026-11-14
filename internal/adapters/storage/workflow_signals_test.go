package storage

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// waitingRun builds a run execution whose first step created one blocking
// approval per item and ended its attempt in a wait.
func (e *wfEnv) waitingRun(ws, subject string, def *workflow.Definition, kinds ...behavior.ApprovalKind) (bRun, *workflow.Job, []behavior.Approval) {
	e.t.Helper()
	br := e.runExecution(ws, "approvals", subject, def)
	job := e.claim(e.base)
	if job.ExecutionID != br.ex.ID {
		e.t.Fatalf("claimed execution %d, want %d", job.ExecutionID, br.ex.ID)
	}
	var approvals []behavior.Approval
	for i, k := range kinds {
		it := e.addItem(ws, br.run.ID, fmt.Sprintf("item-%d", i), behavior.ClassAuto)
		approvals = append(approvals, e.waitApproval(job, e.base, k, br.run.ID, it.ID))
	}
	if err := e.waitOn(job, e.base, workflow.StateWaitingForApproval, approvals...); err != nil {
		e.t.Fatalf("wait: %v", err)
	}
	return br, job, approvals
}

// TC-B20 (REQ-10): a durable wait holds no lease and survives a restart
// (fresh stores over the same database); the decision then wakes the same
// step once, resuming its attempt.
func TestApprovalWaitSurvivesRestart(t *testing.T) {
	e := newWFEnv(t)
	br, job, aps := e.waitingRun(workspaceA, "task:restart", nil, behavior.ApprovalPostDraft)
	ex := e.execution(workspaceA, br.ex.ID)
	assertState(t, ex, workflow.StateWaitingForApproval, workflow.ReasonStepWaiting)
	if len(e.activeJobs(ex.ID)) != 0 {
		t.Fatal("TC-B20: waiting execution holds a job")
	}
	if s := e.step(workspaceA, ex.ID, job.StepKey); s.State != workflow.StepWaiting {
		t.Fatalf("TC-B20: step %s", s.State)
	}
	// Restart: new store instances; reconcile must keep the wait.
	restarted, err := NewWorkflowStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	behaviors, _ := NewBehaviorStore(e.pool)
	e.store, e.behaviors = restarted, behaviors
	fp := e.fingerprint(ex.ID)
	e.reconcile(e.base.Add(time.Hour))
	if e.fingerprint(ex.ID) != fp {
		t.Fatal("TC-B20: reconcile changed an open wait")
	}
	if j, err := e.store.ClaimJob(e.ctx, e.base.Add(time.Hour)); err != nil || j != nil {
		t.Fatalf("TC-B20: open wait runnable: %#v %v", j, err)
	}
	waits, err := e.behaviors.ListStepWaits(e.ctx, workspaceA, ex.ID, job.StepKey)
	if err != nil || len(waits) != 1 || waits[0].Approval.ID != aps[0].ID || waits[0].SignalID != 0 {
		t.Fatalf("TC-B20: waits after restart %#v %v", waits, err)
	}
	at := e.base.Add(2 * time.Hour)
	d := e.decide(workspaceA, aps[0], behavior.DecisionApproved, at)
	if d.ExecutionID != ex.ID {
		t.Fatalf("TC-B20: decision not bound to execution: %d", d.ExecutionID)
	}
	assertState(t, e.execution(workspaceA, ex.ID), workflow.StateQueued, ReasonSignalReceived)
	resumed := e.claim(at)
	if resumed.StepKey != job.StepKey || resumed.Attempts != job.Attempts {
		t.Fatalf("TC-B20: resumed %s attempt %d, want %s attempt %d", resumed.StepKey, resumed.Attempts, job.StepKey, job.Attempts)
	}
	waits, _ = e.behaviors.ListStepWaits(e.ctx, workspaceA, ex.ID, job.StepKey)
	if len(waits) != 1 || waits[0].SignalID == 0 || waits[0].Consumed || waits[0].Approval.Decision != behavior.DecisionApproved {
		t.Fatalf("TC-B20: woken step does not see the decision: %#v", waits)
	}
}

// TC-B21 (REQ-9, REQ-10): a decision is never lost: one recorded while the
// step still runs re-queues the step when it waits; a missing signal or a
// lost wake job is repaired by reconciliation.
func TestApprovalDecisionCrashHasNoLostWakeup(t *testing.T) {
	t.Run("TC-B21/decision while the step runs", func(t *testing.T) {
		e := newWFEnv(t)
		br := e.runExecution(workspaceA, "approvals", "task:during", nil)
		job := e.claim(e.base)
		it := e.addItem(workspaceA, br.run.ID, "i", behavior.ClassAuto)
		a := e.waitApproval(job, e.base, behavior.ApprovalPostDraft, br.run.ID, it.ID)
		e.decide(workspaceA, a, behavior.DecisionApproved, e.base) // before the step ends
		if e.signalCount(br.ex.ID) != 1 {
			t.Fatal("TC-B21: signal not recorded with the decision")
		}
		if err := e.waitOn(job, e.base.Add(time.Second), workflow.StateWaitingForApproval, a); err != nil {
			t.Fatal(err)
		}
		assertState(t, e.execution(workspaceA, br.ex.ID), workflow.StateQueued, "")
		if js := e.activeJobs(br.ex.ID); len(js) != 1 {
			t.Fatalf("TC-B21: decision during the step lost: %s", describeJobs(js))
		}
	})
	t.Run("TC-B21/missing signal is repaired", func(t *testing.T) {
		e := newWFEnv(t)
		br, _, aps := e.waitingRun(workspaceA, "task:nosignal", nil, behavior.ApprovalPostDraft)
		// Simulate a decision written without its signal (crash between them
		// in an older binary or a manual repair).
		mustExec(t, e.ctx, e.pool, `UPDATE approvals SET decision='approved', decided_by='owner', decided_at=now() WHERE id=$1`, aps[0].ID)
		e.reconcile(e.base.Add(time.Minute))
		if e.signalCount(br.ex.ID) != 1 {
			t.Fatal("TC-B21: missing signal not repaired")
		}
		assertState(t, e.execution(workspaceA, br.ex.ID), workflow.StateQueued, "")
		if js := e.activeJobs(br.ex.ID); len(js) != 1 {
			t.Fatalf("TC-B21: repaired signal did not wake: %s", describeJobs(js))
		}
	})
	t.Run("TC-B21/lost wake job is repaired without consuming an attempt", func(t *testing.T) {
		e := newWFEnv(t)
		br, job, aps := e.waitingRun(workspaceA, "task:lostjob", nil, behavior.ApprovalPostDraft)
		e.decide(workspaceA, aps[0], behavior.DecisionApproved, e.base)
		mustExec(t, e.ctx, e.pool, `DELETE FROM jobs WHERE execution_id=$1 AND status='pending'`, br.ex.ID)
		e.reconcile(e.base.Add(time.Minute))
		resumed := e.claim(e.base.Add(time.Minute))
		if resumed.ExecutionID != br.ex.ID || resumed.Attempts != job.Attempts {
			t.Fatalf("TC-B21: repaired wake %d attempt %d, want attempt %d", resumed.ExecutionID, resumed.Attempts, job.Attempts)
		}
	})
}

// TC-B22 (REQ-10): duplicate decisions and signals are no-ops: one signal,
// one wake job, no extra history.
func TestApprovalSignalDuplicateIsNoOp(t *testing.T) {
	e := newWFEnv(t)
	br, job, aps := e.waitingRun(workspaceA, "task:dup", nil, behavior.ApprovalPostDraft)
	e.decide(workspaceA, aps[0], behavior.DecisionApproved, e.base)
	before := e.fingerprint(br.ex.ID)
	// The same tap again (Telegram retries a callback), and a rival decision.
	if again, err := e.behaviors.Decide(e.ctx, workspaceA, aps[0].Nonce, behavior.DecisionApproved, "", "owner", e.base); err != nil && !errors.Is(err, behavior.ErrDecided) {
		t.Fatalf("TC-B22: repeated decide: %v", err)
	} else if err == nil && again.Decision != behavior.DecisionApproved {
		t.Fatalf("TC-B22: repeated decide changed decision: %s", again.Decision)
	}
	if _, err := e.behaviors.Decide(e.ctx, workspaceA, aps[0].Nonce, behavior.DecisionRejected, "", "owner", e.base); err == nil {
		if a, _ := e.behaviors.GetApproval(e.ctx, workspaceA, aps[0].ID); a.Decision != behavior.DecisionApproved {
			t.Fatalf("TC-B22: second decision overwrote the first: %s", a.Decision)
		}
	}
	sig := workflow.Signal{WorkspaceID: workspaceA, ExecutionID: br.ex.ID, SignalID: "approval:" + strconv.FormatInt(aps[0].ID, 10),
		BlockedStepKey: job.StepKey, ReferenceKind: SignalReferenceApproval, ReferenceID: strconv.FormatInt(aps[0].ID, 10), IdempotencyKey: "sig-1"}
	for i := 0; i < 3; i++ {
		if err := e.store.HandleSignal(e.ctx, sig); err != nil {
			t.Fatalf("TC-B22: duplicate HandleSignal %d: %v", i, err)
		}
	}
	other := sig
	other.SignalID, other.IdempotencyKey = "other-id", "sig-2"
	if err := e.store.HandleSignal(e.ctx, other); err != nil {
		t.Fatalf("TC-B22: second signal for the same wait: %v", err)
	}
	if e.fingerprint(br.ex.ID) != before {
		t.Fatal("TC-B22: duplicate signal changed the execution")
	}
	if e.signalCount(br.ex.ID) != 1 || len(e.activeJobs(br.ex.ID)) != 1 {
		t.Fatalf("TC-B22: signals %d jobs %s", e.signalCount(br.ex.ID), describeJobs(e.activeJobs(br.ex.ID)))
	}
	// Concurrent duplicate decisions on a fresh wait: still one signal.
	br2, _, aps2 := e.waitingRun(workspaceA, "task:dup2", nil, behavior.ApprovalPostDraft)
	parallel(3, func(int) {
		_, _ = e.behaviors.Decide(e.ctx, workspaceA, aps2[0].Nonce, behavior.DecisionApproved, "", "owner", e.base)
	})
	if e.signalCount(br2.ex.ID) != 1 || len(e.activeJobs(br2.ex.ID)) != 1 {
		t.Fatalf("TC-B22: concurrent decides: signals %d jobs %s", e.signalCount(br2.ex.ID), describeJobs(e.activeJobs(br2.ex.ID)))
	}
}

// TC-B23 (REQ-11): Resume needs a recorded, unapplied decision of a bound
// wait. It never decides, creates or bypasses an approval, and a repeated
// command is idempotent.
func TestResumeCannotBypassApproval(t *testing.T) {
	e := newWFEnv(t)
	br, job, aps := e.waitingRun(workspaceA, "task:resume", nil, behavior.ApprovalPostDraft)
	cmd := workflow.Command{WorkspaceID: workspaceA, ExecutionID: br.ex.ID, IdempotencyKey: "resume-1"}
	fp := e.fingerprint(br.ex.ID)
	if _, err := e.store.ResumeExecution(e.ctx, cmd); !errors.Is(err, workflow.ErrNotEligible) {
		t.Fatalf("TC-B23: resume without decision: %v", err)
	}
	if e.fingerprint(br.ex.ID) != fp {
		t.Fatal("TC-B23: refused resume changed the execution")
	}
	if a, _ := e.behaviors.GetApproval(e.ctx, workspaceA, aps[0].ID); a.Decision != "" {
		t.Fatalf("TC-B23: resume decided the approval: %s", a.Decision)
	}
	// A rejection recorded without its signal: resume repairs and queues the
	// step, which then sees the rejection (it cannot turn it into approval).
	mustExec(t, e.ctx, e.pool, `UPDATE approvals SET decision='rejected', decided_by='owner', decided_at=now() WHERE id=$1`, aps[0].ID)
	got, err := e.store.ResumeExecution(e.ctx, cmd)
	if err != nil {
		t.Fatalf("TC-B23: resume after recorded decision: %v", err)
	}
	assertState(t, got, workflow.StateQueued, ReasonResumed)
	if _, err := e.store.ResumeExecution(e.ctx, cmd); err != nil {
		t.Fatalf("TC-B23: repeated resume: %v", err)
	}
	if js := e.activeJobs(br.ex.ID); len(js) != 1 {
		t.Fatalf("TC-B23: repeated resume queued twice: %s", describeJobs(js))
	}
	resumed := e.claim(e.base)
	waits, _ := e.behaviors.ListStepWaits(e.ctx, workspaceA, br.ex.ID, job.StepKey)
	if resumed.Attempts != job.Attempts || len(waits) != 1 || waits[0].Approval.Decision != behavior.DecisionRejected {
		t.Fatalf("TC-B23: resumed step sees %#v (attempt %d)", waits, resumed.Attempts)
	}
	// Resume of a running or foreign execution is refused.
	if _, err := e.store.ResumeExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: br.ex.ID, IdempotencyKey: "resume-2"}); !errors.Is(err, workflow.ErrNotEligible) {
		t.Fatalf("TC-B23: resume of a running step: %v", err)
	}
	if _, err := e.store.ResumeExecution(e.ctx, workflow.Command{WorkspaceID: workspaceB, ExecutionID: br.ex.ID, IdempotencyKey: "resume-3"}); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("TC-B23: resume from another workspace: %v", err)
	}
}

// TC-B24 (REQ-8, REQ-10): reject, expiry and cancel racing each other and a
// restart: exactly one decision is kept, one signal at most, and a cancelled
// execution records the decision without ever applying it.
func TestApprovalRejectExpireCancelRaces(t *testing.T) {
	t.Run("TC-B24/cancel then decide", func(t *testing.T) {
		e := newWFEnv(t)
		br, _, aps := e.waitingRun(workspaceA, "task:cancel", nil, behavior.ApprovalPostDraft)
		if _, err := e.store.CancelExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: br.ex.ID, IdempotencyKey: "c"}); err != nil {
			t.Fatal(err)
		}
		if n := e.count(`SELECT count(*) FROM workflow_waits WHERE execution_id=$1 AND closed_at IS NULL`, br.ex.ID); n != 0 {
			t.Fatalf("TC-B24: %d waits open after cancel", n)
		}
		d := e.decide(workspaceA, aps[0], behavior.DecisionApproved, e.base)
		if d.Decision != behavior.DecisionApproved {
			t.Fatal("TC-B24: decision not recorded after cancel")
		}
		assertState(t, e.execution(workspaceA, br.ex.ID), workflow.StateCancelled, workflow.ReasonCancelled)
		e.reconcile(e.base.Add(time.Hour))
		if js := e.activeJobs(br.ex.ID); len(js) != 0 {
			t.Fatalf("TC-B24: cancelled execution woken: %s", describeJobs(js))
		}
		if n := e.count(`SELECT count(*) FROM approvals WHERE id=$1`, aps[0].ID); n != 1 {
			t.Fatal("TC-B24: cancel deleted approval audit history")
		}
	})
	t.Run("TC-B24/decide and expire race", func(t *testing.T) {
		e := newWFEnv(t)
		br, _, aps := e.waitingRun(workspaceA, "task:expire", nil, behavior.ApprovalPostDraft)
		late := aps[0].ExpiresAt.Add(time.Minute)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = e.behaviors.Decide(e.ctx, workspaceA, aps[0].Nonce, behavior.DecisionRejected, "", "owner", late)
		}()
		go func() { defer wg.Done(); _, _ = e.behaviors.ExpireDue(e.ctx, late, 10) }()
		wg.Wait()
		a, err := e.behaviors.GetApproval(e.ctx, workspaceA, aps[0].ID)
		if err != nil || (a.Decision != behavior.DecisionRejected && a.Decision != behavior.DecisionExpired) {
			t.Fatalf("TC-B24: decision after race %q %v", a.Decision, err)
		}
		var sd string
		if err := e.pool.QueryRow(e.ctx, `SELECT decision FROM workflow_signals WHERE execution_id=$1`, br.ex.ID).Scan(&sd); err != nil || sd != string(a.Decision) {
			t.Fatalf("TC-B24: signal %q, approval %q (%v)", sd, a.Decision, err)
		}
		if e.signalCount(br.ex.ID) != 1 || len(e.activeJobs(br.ex.ID)) != 1 {
			t.Fatalf("TC-B24: race produced %d signals, jobs %s", e.signalCount(br.ex.ID), describeJobs(e.activeJobs(br.ex.ID)))
		}
	})
	t.Run("TC-B24/decide and cancel race", func(t *testing.T) {
		e := newWFEnv(t)
		br, _, aps := e.waitingRun(workspaceA, "task:race", nil, behavior.ApprovalPostDraft)
		parallel(2, func(i int) {
			if i == 0 {
				_, _ = e.behaviors.Decide(e.ctx, workspaceA, aps[0].Nonce, behavior.DecisionApproved, "", "owner", e.base)
				return
			}
			_, _ = e.store.CancelExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: br.ex.ID, IdempotencyKey: "c"})
		})
		got := e.execution(workspaceA, br.ex.ID)
		assertState(t, got, workflow.StateCancelled, "")
		if js := e.activeJobs(br.ex.ID); len(js) != 0 {
			t.Fatalf("TC-B24: cancelled execution kept a job: %s", describeJobs(js))
		}
		if j, err := e.store.ClaimJob(e.ctx, e.base.Add(time.Hour)); err != nil || j != nil {
			t.Fatalf("TC-B24: work after cancel: %#v %v", j, err)
		}
	})
}

// TC-B25 (REQ-10, REQ-12): several items on one step join: each decision
// wakes the step once and is recorded only for its own approval; the others
// stay open, and a rejection does not touch other items.
func TestApprovalMultipleItemsPreserveLegacyOutcomes(t *testing.T) {
	e := newWFEnv(t)
	br, job, aps := e.waitingRun(workspaceA, "task:multi", nil, behavior.ApprovalPostDraft, behavior.ApprovalPostDraft, behavior.ApprovalItemAnswer)
	e.decide(workspaceA, aps[1], behavior.DecisionRejected, e.base)
	resumed := e.claim(e.base)
	waits, err := e.behaviors.ListStepWaits(e.ctx, workspaceA, br.ex.ID, job.StepKey)
	if err != nil || len(waits) != 3 {
		t.Fatalf("TC-B25: waits %#v %v", waits, err)
	}
	for i, w := range waits {
		decided := w.SignalID != 0
		if decided != (i == 1) {
			t.Fatalf("TC-B25: wait %d decided=%v (decision %q)", i, decided, w.Approval.Decision)
		}
	}
	if !waits[2].Input || waits[0].Input {
		t.Fatal("TC-B25: input wait kind not preserved")
	}
	// The continuation applies decision 1, then waits on the rest again.
	if err := e.behaviors.ConsumeWaitSignal(e.ctx, fenceOf(resumed, e.base), waits[1].SignalID); err != nil {
		t.Fatal(err)
	}
	if err := e.waitOn(resumed, e.base, workflow.StateWaitingForApproval, aps[0], aps[2]); err != nil {
		t.Fatal(err)
	}
	assertState(t, e.execution(workspaceA, br.ex.ID), workflow.StateWaitingForApproval, "")
	// A consumed signal never wakes the step again.
	e.reconcile(e.base.Add(time.Minute))
	if len(e.activeJobs(br.ex.ID)) != 0 {
		t.Fatal("TC-B25: consumed decision woke the step again")
	}
	e.decide(workspaceA, aps[2], behavior.DecisionAnswered, e.base.Add(time.Minute))
	if len(e.activeJobs(br.ex.ID)) != 1 {
		t.Fatal("TC-B25: second item decision did not wake the step")
	}
	if a, _ := e.behaviors.GetApproval(e.ctx, workspaceA, aps[0].ID); a.Decision != "" {
		t.Fatalf("TC-B25: undecided item changed: %q", a.Decision)
	}
}

// TC-B26 (REQ-10 abuse): alias, preference and feedback approvals of a run
// never bind to a step, never block and never wake an execution.
func TestAliasPrefsFeedbackDoNotBlockExecution(t *testing.T) {
	e := newWFEnv(t)
	br := e.runExecution(workspaceA, "approvals", "task:nonblocking", nil)
	job := e.claim(e.base)
	it := e.addItem(workspaceA, br.run.ID, "i", behavior.ClassNotify)
	for _, kind := range []behavior.ApprovalKind{behavior.ApprovalFeedback, behavior.ApprovalFeedbackReason, behavior.ApprovalContactAlias, behavior.ApprovalAssistantAction} {
		if _, err := e.behaviors.CreateWaitApproval(e.ctx, fenceOf(job, e.base), behavior.NewApproval{
			Kind: kind, RunID: br.run.ID, ItemID: it.ID, Nonce: nonce("nb"), ExpiresAt: e.base.Add(time.Hour),
		}); err == nil {
			t.Fatalf("TC-B26: %s bound as a blocking wait", kind)
		}
	}
	fb, err := e.behaviors.CreateApproval(e.ctx, workspaceA, behavior.NewApproval{Kind: behavior.ApprovalFeedback, RunID: br.run.ID, ItemID: it.ID,
		Nonce: nonce("fb"), ExpiresAt: e.base.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	alias, err := e.behaviors.CreateApproval(e.ctx, workspaceA, behavior.NewApproval{Kind: behavior.ApprovalContactAlias,
		Nonce: nonce("al"), ExpiresAt: e.base.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	creq := resultReq(job, e.base, nil, workflow.Usage{})
	creq.Reason = workflow.ReasonStepSucceeded
	if err := e.store.CompleteJob(e.ctx, creq); err != nil {
		t.Fatalf("TC-B26: open feedback/alias approvals blocked the step: %v", err)
	}
	fp := e.fingerprint(br.ex.ID)
	for _, a := range []behavior.Approval{fb, alias} {
		d := e.decide(workspaceA, a, behavior.DecisionApproved, e.base)
		if d.ExecutionID != 0 {
			t.Fatalf("TC-B26: %s decision bound to execution %d", a.Kind, d.ExecutionID)
		}
	}
	if e.fingerprint(br.ex.ID) != fp || e.signalCount(br.ex.ID) != 0 {
		t.Fatal("TC-B26: non-blocking decision changed the execution")
	}
	assertState(t, e.execution(workspaceA, br.ex.ID), workflow.StateCompleted, "")
	// A signal naming a non-blocking approval is refused.
	err = e.store.HandleSignal(e.ctx, workflow.Signal{WorkspaceID: workspaceA, ExecutionID: br.ex.ID, SignalID: "x", BlockedStepKey: job.StepKey,
		ReferenceKind: SignalReferenceApproval, ReferenceID: strconv.FormatInt(fb.ID, 10)})
	if !errors.Is(err, workflow.ErrNotEligible) {
		t.Fatalf("TC-B26: signal for a feedback approval: %v", err)
	}
}

// TC-B27 (REQ-10 abuse): a signal from another workspace, for another
// execution or step, for an undecided approval, or with a malformed
// reference is rejected and changes nothing.
func TestForeignWorkspaceSignalRejected(t *testing.T) {
	e := newWFEnv(t)
	brA, jobA, apsA := e.waitingRun(workspaceA, "task:a", nil, behavior.ApprovalPostDraft)
	brB, jobB, apsB := e.waitingRun(workspaceB, "task:b", nil, behavior.ApprovalPostDraft)
	e.decide(workspaceB, apsB[0], behavior.DecisionApproved, e.base)
	fpA := e.fingerprint(brA.ex.ID)
	id := func(a behavior.Approval) string { return strconv.FormatInt(a.ID, 10) }
	cases := []struct {
		name string
		sig  workflow.Signal
		want error
	}{
		{"workspace B names execution A", workflow.Signal{WorkspaceID: workspaceB, ExecutionID: brA.ex.ID, SignalID: "s1", BlockedStepKey: jobA.StepKey, ReferenceKind: SignalReferenceApproval, ReferenceID: id(apsA[0])}, workflow.ErrNotFound},
		{"approval of B on execution A", workflow.Signal{WorkspaceID: workspaceA, ExecutionID: brA.ex.ID, SignalID: "s2", BlockedStepKey: jobA.StepKey, ReferenceKind: SignalReferenceApproval, ReferenceID: id(apsB[0])}, workflow.ErrNotEligible},
		{"undecided approval", workflow.Signal{WorkspaceID: workspaceA, ExecutionID: brA.ex.ID, SignalID: "s3", BlockedStepKey: jobA.StepKey, ReferenceKind: SignalReferenceApproval, ReferenceID: id(apsA[0])}, workflow.ErrNotEligible},
		{"wrong step", workflow.Signal{WorkspaceID: workspaceB, ExecutionID: brB.ex.ID, SignalID: "s4", BlockedStepKey: "other", ReferenceKind: SignalReferenceApproval, ReferenceID: id(apsB[0])}, workflow.ErrNotEligible},
		{"wrong execution", workflow.Signal{WorkspaceID: workspaceB, ExecutionID: brB.ex.ID + 1000, SignalID: "s5", BlockedStepKey: jobB.StepKey, ReferenceKind: SignalReferenceApproval, ReferenceID: id(apsB[0])}, workflow.ErrNotFound},
		{"unknown reference kind", workflow.Signal{WorkspaceID: workspaceA, ExecutionID: brA.ex.ID, SignalID: "s6", BlockedStepKey: jobA.StepKey, ReferenceKind: "input", ReferenceID: id(apsA[0])}, workflow.ErrInvalidInput},
		{"injected reference id", workflow.Signal{WorkspaceID: workspaceA, ExecutionID: brA.ex.ID, SignalID: "s7", BlockedStepKey: jobA.StepKey, ReferenceKind: SignalReferenceApproval, ReferenceID: "1 OR 1=1"}, workflow.ErrInvalidInput},
	}
	for _, tc := range cases {
		t.Run("TC-B27/"+tc.name, func(t *testing.T) {
			if err := e.store.HandleSignal(e.ctx, tc.sig); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	if e.fingerprint(brA.ex.ID) != fpA || e.signalCount(brA.ex.ID) != 0 {
		t.Fatal("TC-B27: a rejected signal changed execution A")
	}
	// Workspace A's owner cannot decide B's approval by nonce.
	if _, err := e.behaviors.Decide(e.ctx, workspaceA, apsB[0].Nonce, behavior.DecisionRejected, "", "owner", e.base); err == nil {
		t.Fatal("TC-B27: cross-workspace decide accepted")
	}
}

// TC-B28 (REQ-7, REQ-10, 61902f5): approval rounds never consume attempts;
// a step that waits on its last attempt can still be resumed; a resumed
// claim that then fails over counts as a real attempt.
func TestApprovalRoundsKeepAttemptBudget(t *testing.T) {
	e := newWFEnv(t)
	def := chainDef("s1")
	def.Retry = workflow.RetryPolicy{MaxAttempts: 1}
	br := e.runExecution(workspaceA, "approvals", "task:rounds", &def)
	job := e.claim(e.base)
	at := e.base
	for round := 0; round < 6; round++ {
		if job.Attempts != 1 || job.MaxAttempts != 1 {
			t.Fatalf("TC-B28 round %d: attempt %d/%d", round, job.Attempts, job.MaxAttempts)
		}
		it := e.addItem(workspaceA, br.run.ID, fmt.Sprintf("r%d", round), behavior.ClassAuto)
		a := e.waitApproval(job, at, behavior.ApprovalPostDraft, br.run.ID, it.ID)
		if err := e.waitOn(job, at, workflow.StateWaitingForApproval, a); err != nil {
			t.Fatalf("round %d wait: %v", round, err)
		}
		at = at.Add(time.Minute)
		e.decide(workspaceA, a, behavior.DecisionApproved, at)
		next, err := e.store.ClaimJob(e.ctx, at)
		if err != nil || next == nil {
			t.Fatalf("TC-B28 round %d: step on its last attempt not resumable: %#v %v", round, next, err)
		}
		waits, _ := e.behaviors.ListStepWaits(e.ctx, workspaceA, br.ex.ID, next.StepKey)
		for _, w := range waits {
			if w.SignalID != 0 && !w.Consumed {
				if err := e.behaviors.ConsumeWaitSignal(e.ctx, fenceOf(next, at), w.SignalID); err != nil {
					t.Fatal(err)
				}
			}
		}
		job = next
	}
	if s := e.step(workspaceA, br.ex.ID, "s1"); s.Attempts != 1 {
		t.Fatalf("TC-B28: step attempts %d after 6 approval rounds", s.Attempts)
	}
	// The resumed claim loses its lease: with no attempts left it fails.
	e.reconcile(job.LeaseExpiresAt.Add(time.Second))
	assertState(t, e.execution(workspaceA, br.ex.ID), workflow.StateFailed, workflow.ReasonAttemptsExhausted)
}

// TC-B29 (REQ-8, REQ-10, R5): a waiting execution past its deadline is
// failed with deadline_exceeded by the reconciler: the waiting step fails,
// waits close, no job remains; a later decision is recorded but never
// applied, and a decision after the deadline never wakes the step.
func TestWaitingExecutionExpiresAtDeadline(t *testing.T) {
	e := newWFEnv(t)
	def := chainDef("s1", "s2")
	def.ExecutionDeadline = 10 * time.Minute
	br, job, aps := e.waitingRun(workspaceA, "task:deadline", &def, behavior.ApprovalPostDraft, behavior.ApprovalPostDraft)
	deadline := *e.execution(workspaceA, br.ex.ID).Deadline
	// A decision after the deadline, before the reconciler ran, does not wake.
	e.decide(workspaceA, aps[0], behavior.DecisionApproved, deadline.Add(time.Second))
	if js := e.activeJobs(br.ex.ID); len(js) != 0 {
		t.Fatalf("TC-B29: wake past the deadline: %s", describeJobs(js))
	}
	e.reconcile(deadline.Add(2 * time.Second))
	got := e.execution(workspaceA, br.ex.ID)
	assertState(t, got, workflow.StateFailed, workflow.ReasonDeadlineExceeded)
	if s := e.step(workspaceA, br.ex.ID, job.StepKey); s.State != workflow.StepFailed {
		t.Fatalf("TC-B29: waiting step %s", s.State)
	}
	if s := e.step(workspaceA, br.ex.ID, "s2"); s.State != workflow.StepCancelled {
		t.Fatalf("TC-B29: unfinished step %s", s.State)
	}
	if n := e.count(`SELECT count(*) FROM workflow_waits WHERE execution_id=$1 AND closed_at IS NULL`, br.ex.ID); n != 0 {
		t.Fatalf("TC-B29: %d waits still open", n)
	}
	d := e.decide(workspaceA, aps[1], behavior.DecisionRejected, deadline.Add(time.Minute))
	if d.Decision != behavior.DecisionRejected {
		t.Fatal("TC-B29: late decision not recorded")
	}
	e.reconcile(deadline.Add(time.Hour))
	if js := e.activeJobs(br.ex.ID); len(js) != 0 {
		t.Fatalf("TC-B29: late decision applied: %s", describeJobs(js))
	}
	assertState(t, e.execution(workspaceA, br.ex.ID), workflow.StateFailed, workflow.ReasonDeadlineExceeded)
	if _, err := e.store.ResumeExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: br.ex.ID, IdempotencyKey: "late"}); !errors.Is(err, workflow.ErrNotEligible) {
		t.Fatalf("TC-B29: resume after deadline: %v", err)
	}
	err := e.store.HandleSignal(e.ctx, workflow.Signal{WorkspaceID: workspaceA, ExecutionID: br.ex.ID, SignalID: "late", BlockedStepKey: job.StepKey,
		ReferenceKind: SignalReferenceApproval, ReferenceID: strconv.FormatInt(aps[1].ID, 10)})
	if !errors.Is(err, workflow.ErrNotEligible) {
		t.Fatalf("TC-B29: signal after deadline: %v", err)
	}
}
