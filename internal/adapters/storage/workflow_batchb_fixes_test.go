package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// TC-B10 (REQ-8, R1): the stored execution deadline is the earlier of the
// explicit request deadline and At + the pinned definition deadline; a retry
// child gets a fresh deadline from its own start, never the parent's past one.
func TestExecutionDeadlineFromDefinition(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "deadline")
	def := chainDef("a")
	def.ExecutionDeadline = time.Hour
	start := func(trigger string, explicit *time.Time) workflow.Execution {
		t.Helper()
		req := e.startReq(workspaceA, v.ID, trigger, def)
		req.At, req.Deadline = e.base, explicit
		ex, err := e.store.StartExecution(e.ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return ex
	}
	want := e.base.Add(time.Hour)
	if ex := start("d-def", nil); ex.Deadline == nil || !ex.Deadline.Equal(want) {
		t.Fatalf("TC-B10: definition deadline %v, want %v", ex.Deadline, want)
	}
	earlier := e.base.Add(30 * time.Minute)
	if ex := start("d-early", &earlier); ex.Deadline == nil || !ex.Deadline.Equal(earlier) {
		t.Fatalf("TC-B10: explicit earlier deadline %v, want %v", ex.Deadline, earlier)
	}
	later := e.base.Add(3 * time.Hour)
	parent := start("d-late", &later)
	if parent.Deadline == nil || !parent.Deadline.Equal(want) {
		t.Fatalf("TC-B10: explicit later deadline must not extend the definition: %v, want %v", parent.Deadline, want)
	}

	// Run d-late past its deadline: the claimant is fenced out and the
	// reconciler fails it with deadline_exceeded and dead-letters its job.
	for {
		j, err := e.store.ClaimJob(e.ctx, e.base)
		if err != nil {
			t.Fatal(err)
		}
		if j == nil {
			t.Fatal("d-late job not claimable")
		}
		if j.ExecutionID == parent.ID {
			if err := e.store.AuthorizeAction(e.ctx, j.ID, workspaceA, j.ClaimToken, want); !errors.Is(err, workflow.ErrStaleClaim) {
				t.Fatalf("TC-B10: claim authorized at the deadline: %v", err)
			}
			break
		}
	}
	e.reconcile(want)
	got := e.execution(workspaceA, parent.ID)
	assertState(t, got, workflow.StateFailed, workflow.ReasonDeadlineExceeded)
	if js := e.jobs(parent.ID); len(js) != 1 || js[0].Status != "dead_letter" || js[0].DeadLetter != "deadline_exceeded" || js[0].Token != "" || js[0].HasLease {
		t.Fatalf("TC-B10: deadline job not dead-lettered: %s", describeJobs(js))
	}

	child, err := e.store.RetryExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: parent.ID, IdempotencyKey: "retry-deadline"})
	if err != nil {
		t.Fatalf("TC-B10: retry after deadline: %v", err)
	}
	now := time.Now()
	if child.Deadline == nil || child.Deadline.Before(now.Add(59*time.Minute)) || child.Deadline.After(now.Add(61*time.Minute)) {
		t.Fatalf("TC-B10: retry child deadline %v, want about now+1h (%v)", child.Deadline, now.Add(time.Hour))
	}
	if j, err := e.store.ClaimJob(e.ctx, now.Add(time.Second)); err != nil || j == nil || j.ExecutionID != child.ID {
		t.Fatalf("TC-B10: retry child not runnable before its fresh deadline: %#v %v", j, err)
	}
}

// TC-B11 (REQ-8, R3): cancel and admin terminal transitions dead-letter the
// active job with the reason, clear its claim and lease, and fence out the
// old claimant.
func TestTerminalCommandsDeadLetterActiveJob(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "term")

	running := e.start(workspaceA, v.ID, "t-run", chainDef("a", "b"))
	job := e.claim(e.base)
	if _, err := e.store.CancelExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: running.ID, IdempotencyKey: "cancel-1"}); err != nil {
		t.Fatal(err)
	}
	js := e.jobs(running.ID)
	if len(js) != 1 || js[0].Status != "dead_letter" || js[0].DeadLetter != "cancelled" || js[0].Token != "" || js[0].HasLease {
		t.Fatalf("TC-B11: cancelled job %s", describeJobs(js))
	}
	at := e.base.Add(time.Second)
	if err := e.store.AuthorizeAction(e.ctx, job.ID, workspaceA, job.ClaimToken, at); !errors.Is(err, workflow.ErrStaleClaim) {
		t.Fatalf("TC-B11: old claimant authorized after cancel: %v", err)
	}
	if _, err := e.store.CheckpointJob(e.ctx, checkpointReq(job, at, reusable("x"), workflow.Usage{InputTokens: 1}, "late")); !errors.Is(err, workflow.ErrStaleClaim) {
		t.Fatalf("TC-B11: old claimant checkpointed after cancel: %v", err)
	}
	creq := resultReq(job, at, nil, workflow.Usage{})
	creq.Reason = workflow.ReasonStepSucceeded
	if err := e.store.CompleteJob(e.ctx, creq); !errors.Is(err, workflow.ErrStaleClaim) {
		t.Fatalf("TC-B11: old claimant completed after cancel: %v", err)
	}
	for _, s := range e.steps(workspaceA, running.ID) {
		if s.State != workflow.StepCancelled {
			t.Fatalf("TC-B11: step %s left %s after cancel", s.Key, s.State)
		}
	}
	// Cancel is idempotent: a repeat neither errors nor adds history.
	n := len(e.events(running.ID))
	if _, err := e.store.CancelExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: running.ID, IdempotencyKey: "cancel-2"}); err != nil {
		t.Fatalf("TC-B11: repeated cancel: %v", err)
	}
	if len(e.events(running.ID)) != n {
		t.Fatal("TC-B11: repeated cancel appended history")
	}

	pending := e.start(workspaceA, v.ID, "t-pending", chainDef("a"))
	if _, err := e.store.TransitionExecution(e.ctx, workflow.TransitionRequest{WorkspaceID: workspaceA, ExecutionID: pending.ID,
		From: workflow.StateQueued, To: workflow.StateFailed, Reason: workflow.ReasonNonRetryable}); err != nil {
		t.Fatal(err)
	}
	if js := e.jobs(pending.ID); len(js) != 1 || js[0].Status != "dead_letter" || js[0].DeadLetter != "non_retryable" {
		t.Fatalf("TC-B11: admin failure left job %s", describeJobs(js))
	}
	if j, err := e.store.ClaimJob(e.ctx, e.base.Add(time.Hour)); err != nil || j != nil {
		t.Fatalf("TC-B11: terminal execution still runnable: %#v %v", j, err)
	}
}

// TC-B12 (REQ-9, R6): one bad record does not roll back the reconcile pass:
// the good record is repaired and committed, the bad one is left untouched
// and reported in the joined error by execution ID.
func TestReconcileSavepointPartialCommit(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "partial")
	good := e.start(workspaceA, v.ID, "p-good", chainDef("a"))
	bad := e.start(workspaceA, v.ID, "p-bad", chainDef("a"))
	mustExec(t, e.ctx, e.pool, `DELETE FROM jobs WHERE execution_id IN ($1,$2)`, good.ID, bad.ID)
	// A corrupt pinned definition makes this record's repair fail.
	mustExec(t, e.ctx, e.pool, `UPDATE workflow_executions SET definition = jsonb_set(definition,'{Retry}','"corrupt"') WHERE id=$1`, bad.ID)
	badFP := e.fingerprint(bad.ID)

	err := e.store.ReconcileJobs(e.ctx, e.base)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("execution %d", bad.ID)) {
		t.Fatalf("TC-B12: bad record not reported: %v", err)
	}
	if strings.Contains(err.Error(), fmt.Sprintf("execution %d:", good.ID)) {
		t.Fatalf("TC-B12: good record reported as failed: %v", err)
	}
	if js := e.activeJobs(good.ID); len(js) != 1 || js[0].StepKey != "a" {
		t.Fatalf("TC-B12: good record not repaired (pass rolled back?): %s", describeJobs(js))
	}
	if e.fingerprint(bad.ID) != badFP {
		t.Fatal("TC-B12: bad record partially written")
	}
	// The repaired execution runs to completion.
	job := e.claim(e.base)
	if job.ExecutionID != good.ID {
		t.Fatalf("TC-B12: claimed %d, want %d", job.ExecutionID, good.ID)
	}
	e.complete(job, e.base, nil, workflow.Usage{})
	assertState(t, e.execution(workspaceA, good.ID), workflow.StateCompleted, "")
}

// TC-B13 (REQ-4, REQ-9, R7): reconciliation only takes legal transitions: a
// QUEUED execution whose steps all succeeded is failed with
// inconsistent_state, never completed; a RUNNING one is completed.
func TestReconcileUsesLegalTransitions(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "legal")

	queued := e.start(workspaceA, v.ID, "l-queued", chainDef("a"))
	mustExec(t, e.ctx, e.pool, `DELETE FROM jobs WHERE execution_id=$1`, queued.ID)
	mustExec(t, e.ctx, e.pool, `UPDATE execution_steps SET state='SUCCEEDED' WHERE execution_id=$1`, queued.ID)

	running := e.start(workspaceA, v.ID, "l-running", chainDef("a"))
	job := e.claim(e.base)
	if job.ExecutionID != running.ID {
		if job.ExecutionID != queued.ID {
			t.Fatalf("unexpected claim %d", job.ExecutionID)
		}
		t.Fatal("claimed the inconsistent execution")
	}
	mustExec(t, e.ctx, e.pool, `DELETE FROM jobs WHERE execution_id=$1`, running.ID)
	mustExec(t, e.ctx, e.pool, `UPDATE execution_steps SET state='SUCCEEDED' WHERE execution_id=$1`, running.ID)

	e.reconcile(e.base.Add(time.Second))
	q := e.execution(workspaceA, queued.ID)
	assertState(t, q, workflow.StateFailed, workflow.ReasonInconsistentState)
	for _, ev := range e.events(queued.ID) {
		if ev.To == string(workflow.StateCompleted) {
			t.Fatal("TC-B13: QUEUED execution recorded COMPLETED")
		}
		if ev.Kind == string(workflow.EventTransition) && ev.To == string(workflow.StateFailed) && ev.From != string(workflow.StateQueued) {
			t.Fatalf("TC-B13: faked intermediate state: %+v", ev)
		}
	}
	assertState(t, e.execution(workspaceA, running.ID), workflow.StateCompleted, "")
	// The repaired executions stay terminal across later passes.
	qfp, rfp := e.fingerprint(queued.ID), e.fingerprint(running.ID)
	e.reconcile(e.base.Add(time.Hour))
	if e.fingerprint(queued.ID) != qfp || e.fingerprint(running.ID) != rfp {
		t.Fatal("TC-B13: terminal executions changed by a later pass")
	}
}

// TC-B14 (REQ-6, R6): a job result, a checkpoint, a cancel and a reconcile
// racing on the same execution never deadlock (lock order execution, job):
// every call either wins or is fenced out, and the end state is consistent.
func TestLockOrderNoDeadlockUnderRaces(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "race")
	for round := 0; round < 12; round++ {
		ex := e.start(workspaceA, v.ID, fmt.Sprintf("race-%d", round), chainDef("a", "b"))
		job := e.claim(e.base)
		if job.ExecutionID != ex.ID {
			t.Fatalf("round %d claimed %d", round, job.ExecutionID)
		}
		expiry := job.LeaseExpiresAt
		var mu sync.Mutex
		var errs []error
		record := func(err error) {
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}
		ctx, cancel := context.WithTimeout(e.ctx, 20*time.Second)
		parallel(4, func(i int) {
			switch i {
			case 0:
				req := resultReq(job, expiry.Add(-time.Second), nil, workflow.Usage{InputTokens: 1})
				req.Reason = workflow.ReasonStepSucceeded
				if round%2 == 1 {
					req.Reason, req.RetryAt = "provider_busy", expiry
					record(e.store.RetryJob(ctx, req))
					return
				}
				record(e.store.CompleteJob(ctx, req))
			case 1:
				_, err := e.store.CheckpointJob(ctx, checkpointReq(job, expiry.Add(-time.Second), nil, workflow.Usage{OutputTokens: 1}, fmt.Sprintf("cp-%d", round)))
				record(err)
			case 2:
				record(e.store.ReconcileJobs(ctx, expiry.Add(time.Second)))
			case 3:
				if round%3 == 0 {
					_, err := e.store.CancelExecution(ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: ex.ID, IdempotencyKey: fmt.Sprintf("rc-%d", round)})
					record(err)
					return
				}
				record(e.store.AuthorizeAction(ctx, job.ID, workspaceA, job.ClaimToken, expiry.Add(-time.Second)))
			}
		})
		cancel()
		for _, err := range errs {
			if isDeadlock(err) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("TC-B14 round %d: lock-order failure: %v", round, err)
			}
			// A cancel that loses to completion is not eligible; a fenced
			// write that loses is a stale claim. Both are correct outcomes.
			if err != nil && !errors.Is(err, workflow.ErrStaleClaim) && !errors.Is(err, workflow.ErrNotEligible) {
				t.Fatalf("TC-B14 round %d: unexpected error: %v", round, err)
			}
		}
		// Consistency: a terminal execution has no active job; a nonterminal
		// one has exactly one.
		got := e.execution(workspaceA, ex.ID)
		active := e.activeJobs(ex.ID)
		if got.State.IsTerminal() != (len(active) == 0) || len(active) > 1 {
			t.Fatalf("TC-B14 round %d: %s with active jobs %s", round, got.State, describeJobs(active))
		}
		// Clean up so the next round claims its own job.
		if !got.State.IsTerminal() {
			if _, err := e.store.CancelExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: ex.ID, IdempotencyKey: fmt.Sprintf("end-%d", round)}); err != nil {
				t.Fatal(err)
			}
		}
	}
}
