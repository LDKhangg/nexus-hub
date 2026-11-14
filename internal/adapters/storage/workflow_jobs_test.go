package storage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// stubClock is an injected worker clock; its After never fires, so no step
// timeout interferes unless a test moves time itself.
type stubClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stubClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *stubClock) After(time.Duration) <-chan time.Time { return make(chan time.Time) }
func (c *stubClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// TC-17 (REQ-5, REQ-6): after a step succeeds and the hub restarts, the
// next claim continues with the dependent step; the finished step is not
// rerun and keeps its checkpoint and cumulative usage.
func TestJobRetryPreservesUsageAndCompletedSteps(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "resume")
	ex := e.start(workspaceA, v.ID, "resume-1", chainDef("a", "b"))
	job := e.claim(e.base)
	if job.StepKey != "a" || job.Attempts != 1 || job.Execution.State != workflow.StateRunning || job.ClaimToken == "" {
		t.Fatalf("first claim = %#v", job)
	}
	if _, err := e.store.CheckpointJob(e.ctx, checkpointReq(job, e.base, reusable("7"), workflow.Usage{InputTokens: 5, CacheReadTokens: 2}, "mid-a")); err != nil {
		t.Fatal(err)
	}
	e.complete(job, e.base.Add(time.Second), reusable("7", "8"), workflow.Usage{InputTokens: 3, OutputTokens: 9})

	// Restart: a fresh store instance and the startup reconcile.
	restarted, err := NewWorkflowStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	e.store = restarted
	if err := workflow.NewReconciler(restarted).Reconcile(e.ctx, e.base.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	a := e.step(workspaceA, ex.ID, "a")
	if a.State != workflow.StepSucceeded || a.Attempts != 1 || a.Usage != (workflow.Usage{InputTokens: 8, OutputTokens: 9, CacheReadTokens: 2}) ||
		a.Checkpoint == nil || len(a.Checkpoint.References) != 2 || !a.Checkpoint.Reusable {
		t.Fatalf("TC-17: completed step after restart = %#v", a)
	}
	next := e.claim(e.base.Add(2 * time.Second))
	if next.ExecutionID != ex.ID || next.StepKey != "b" || next.Attempts != 1 {
		t.Fatalf("TC-17: restart claimed %s attempt %d", next.StepKey, next.Attempts)
	}
	// Lose step b's lease, reconcile, and claim again: a is still untouched.
	expiry := e.base.Add(2*time.Second + 15*time.Minute + time.Second)
	if err := e.store.ReconcileJobs(e.ctx, expiry); err != nil {
		t.Fatal(err)
	}
	again := e.claim(expiry.Add(5 * time.Second))
	if again.StepKey != "b" || again.Attempts != 2 {
		t.Fatalf("TC-17: after lease loss claimed %s attempt %d", again.StepKey, again.Attempts)
	}
	if got := e.step(workspaceA, ex.ID, "a"); got.Attempts != 1 || got.State != workflow.StepSucceeded || got.Usage != a.Usage {
		t.Fatalf("TC-17: completed step rerun or changed: %#v", got)
	}
	e.complete(again, expiry.Add(6*time.Second), reusable("9"), workflow.Usage{OutputTokens: 1})
	done := e.execution(workspaceA, ex.ID)
	if done.State != workflow.StateCompleted || done.FinishedAt == nil {
		t.Fatalf("TC-17: final state %s", done.State)
	}
	if len(e.activeJobs(ex.ID)) != 0 {
		t.Fatal("TC-17: completed execution kept an active job")
	}
	if j, err := e.store.ClaimJob(e.ctx, expiry.Add(time.Hour)); err != nil || j != nil {
		t.Fatalf("TC-17: work left after completion: %#v %v", j, err)
	}
	var stepRuns int
	for _, ev := range e.events(ex.ID) {
		if ev.Kind == "transition" && ev.From == "QUEUED" && ev.StepKey == "a" {
			stepRuns++
		}
	}
	if stepRuns != 1 {
		t.Fatalf("TC-17: step a started %d times", stepRuns)
	}
}

// TC-20 (REQ-6): two workers racing for one due job get exactly one claim
// with a fresh token; one execution never has two active jobs.
func TestJobConcurrentClaimsAreExclusive(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "race")
	seen := map[string]bool{}
	for round := 0; round < 10; round++ {
		ex := e.start(workspaceA, v.ID, "race-"+string(rune('a'+round)), chainDef("a"))
		jobs := make([]*workflow.Job, 2)
		errs := make([]error, 2)
		parallel(2, func(i int) { jobs[i], errs[i] = e.store.ClaimJob(e.ctx, e.base) })
		won := 0
		for i := range jobs {
			if jobs[i] != nil {
				won++
				if jobs[i].ExecutionID != ex.ID || seen[jobs[i].ClaimToken] {
					t.Fatalf("TC-20: bad claim %#v", jobs[i])
				}
				seen[jobs[i].ClaimToken] = true
			} else if errs[i] != nil && !errors.Is(errs[i], workflow.ErrStaleClaim) {
				t.Fatalf("TC-20: loser error %v", errs[i])
			}
		}
		if won != 1 {
			t.Fatalf("TC-20 round %d: %d workers claimed the job (errs=%v)", round, won, errs)
		}
		if j, err := e.store.ClaimJob(e.ctx, e.base.Add(time.Minute)); err != nil || j != nil {
			t.Fatalf("TC-20: processing job claimed again: %#v %v", j, err)
		}
	}

	t.Run("TC-20/second active job for an execution is refused", func(t *testing.T) {
		ex := e.start(workspaceA, v.ID, "race-dup", chainDef("a"))
		_, err := e.pool.Exec(e.ctx, `INSERT INTO jobs(workspace_id,execution_id,step_key,due_at,max_attempts) VALUES($1,$2,'a',now(),5)`, workspaceA, ex.ID)
		requirePGCode(t, err, "23505")
	})
}

// TC-20 (REQ-6): a reclaim after lease expiry issues a fresh token.
func TestJobReclaimIssuesFreshToken(t *testing.T) {
	e2 := newWFEnv(t)
	v2 := e2.enabledVersion(workspaceA, "token")
	e2.start(workspaceA, v2.ID, "token-1", chainDef("a"))
	first := e2.claim(e2.base)
	expiry := e2.base.Add(16 * time.Minute)
	if err := e2.store.ReconcileJobs(e2.ctx, expiry); err != nil {
		t.Fatal(err)
	}
	second := e2.claim(expiry.Add(5 * time.Second))
	if second.ID != first.ID || second.ClaimToken == first.ClaimToken || second.Attempts != 2 {
		t.Fatalf("TC-20: reclaim: first=%s second=%s attempts=%d", first.ClaimToken, second.ClaimToken, second.Attempts)
	}
}

// fencedWrites tries every claim-fenced write with job's token at time at
// and requires each to fail with ErrStaleClaim without changing anything.
func (e *wfEnv) fencedWrites(t *testing.T, label string, job *workflow.Job, at time.Time) {
	t.Helper()
	fp := e.fingerprint(job.ExecutionID)
	if err := e.store.AuthorizeAction(e.ctx, job.ID, job.WorkspaceID, job.ClaimToken, at); !errors.Is(err, workflow.ErrStaleClaim) {
		t.Errorf("%s: authorize: %v", label, err)
	}
	if _, err := e.store.CheckpointJob(e.ctx, checkpointReq(job, at, reusable("x"), workflow.Usage{InputTokens: 50}, label+"-cp")); !errors.Is(err, workflow.ErrStaleClaim) {
		t.Errorf("%s: checkpoint: %v", label, err)
	}
	req := resultReq(job, at, reusable("y"), workflow.Usage{OutputTokens: 60})
	req.Reason = workflow.ReasonStepSucceeded
	if err := e.store.CompleteJob(e.ctx, req); !errors.Is(err, workflow.ErrStaleClaim) {
		t.Errorf("%s: complete: %v", label, err)
	}
	req.Reason, req.RetryAt = workflow.ReasonStepRetry, at.Add(5*time.Second)
	if err := e.store.RetryJob(e.ctx, req); !errors.Is(err, workflow.ErrStaleClaim) {
		t.Errorf("%s: retry: %v", label, err)
	}
	req.Reason = workflow.ReasonNonRetryable
	if err := e.store.FailJob(e.ctx, req); !errors.Is(err, workflow.ErrStaleClaim) {
		t.Errorf("%s: fail: %v", label, err)
	}
	req.WaitState, req.WaitReferences, req.Reason = workflow.StateWaitingForApproval, []workflow.CheckpointReference{{Kind: "approval", ReferenceID: "1"}}, workflow.ReasonStepWaiting
	if err := e.store.WaitJob(e.ctx, req); !errors.Is(err, workflow.ErrStaleClaim) {
		t.Errorf("%s: wait: %v", label, err)
	}
	if e.fingerprint(job.ExecutionID) != fp {
		t.Errorf("%s: a fenced write changed the execution", label)
	}
}

// TC-21 (REQ-6, REQ-8): after lease expiry or cancel, the old claimant can
// neither checkpoint, complete nor authorize an action.
func TestJobOldTokenCannotWriteOrAuthorize(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "fence")

	t.Run("TC-21/lease expiry", func(t *testing.T) {
		ex := e.start(workspaceA, v.ID, "fence-expiry", chainDef("a", "b"))
		job := e.claim(e.base)
		if err := e.store.AuthorizeAction(e.ctx, job.ID, workspaceA, job.ClaimToken, e.base.Add(time.Minute)); err != nil {
			t.Fatalf("live claim refused: %v", err)
		}
		expiry := job.LeaseExpiresAt
		if !expiry.Equal(e.base.Add(15 * time.Minute)) {
			t.Fatalf("lease = %v", expiry)
		}
		e.fencedWrites(t, "at expiry", job, expiry)
		e.fencedWrites(t, "after expiry", job, expiry.Add(time.Second))
		if err := e.store.ReconcileJobs(e.ctx, expiry.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		fresh := e.claim(expiry.Add(6 * time.Second))
		if fresh.ExecutionID != ex.ID {
			t.Fatalf("claimed %d", fresh.ExecutionID)
		}
		// Old token with a time inside the new lease is still fenced.
		e.fencedWrites(t, "superseded", job, expiry.Add(7*time.Second))
		if err := e.store.AuthorizeAction(e.ctx, fresh.ID, workspaceA, fresh.ClaimToken, expiry.Add(7*time.Second)); err != nil {
			t.Fatalf("new claimant refused: %v", err)
		}
		e.complete(fresh, expiry.Add(8*time.Second), reusable("1"), workflow.Usage{})
		if e.step(workspaceA, ex.ID, "a").Usage != (workflow.Usage{}) {
			t.Fatal("old claimant usage leaked into the step")
		}
		// Finish b so no due job is left for later subtests.
		e.complete(e.claim(expiry.Add(9*time.Second)), expiry.Add(9*time.Second), nil, workflow.Usage{})
	})

	t.Run("TC-21/cancel while running", func(t *testing.T) {
		ex := e.start(workspaceA, v.ID, "fence-cancel", chainDef("a", "b"))
		job := e.claim(e.base)
		if _, err := e.store.CancelExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: ex.ID, IdempotencyKey: "stop"}); err != nil {
			t.Fatal(err)
		}
		e.fencedWrites(t, "cancelled", job, e.base.Add(time.Minute))
		if st := e.execution(workspaceA, ex.ID).State; st != workflow.StateCancelled {
			t.Fatalf("state %s", st)
		}
		if err := e.store.ReconcileJobs(e.ctx, e.base.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if st := e.execution(workspaceA, ex.ID).State; st != workflow.StateCancelled {
			t.Fatalf("reconcile revived a cancelled execution: %s", st)
		}
		if j, err := e.store.ClaimJob(e.ctx, e.base.Add(time.Hour)); err != nil || j != nil {
			t.Fatalf("cancelled work claimable: %#v %v", j, err)
		}
	})

	t.Run("TC-21/wrong attempt, step or token", func(t *testing.T) {
		ex := e.start(workspaceA, v.ID, "fence-shape", chainDef("a"))
		job := e.claim(e.base)
		for name, mutate := range map[string]func(*workflow.Job){
			"attempt": func(j *workflow.Job) { j.Attempts = 2 },
			"step":    func(j *workflow.Job) { j.StepKey = "b" },
			"token":   func(j *workflow.Job) { j.ClaimToken = "019a6690-abcd-7000-8000-0000000000ff" },
			"exec":    func(j *workflow.Job) { j.ExecutionID = ex.ID + 1000 },
		} {
			bad := *job
			mutate(&bad)
			fp := e.fingerprint(ex.ID)
			if err := e.store.CompleteJob(e.ctx, resultReq(&bad, e.base.Add(time.Second), nil, workflow.Usage{})); !errors.Is(err, workflow.ErrStaleClaim) {
				t.Errorf("%s: %v", name, err)
			}
			if e.fingerprint(ex.ID) != fp {
				t.Errorf("%s wrote", name)
			}
		}
		bad := *job
		bad.ClaimToken = "not-a-token"
		if err := e.store.AuthorizeAction(e.ctx, bad.ID, workspaceA, bad.ClaimToken, e.base); !errors.Is(err, workflow.ErrInvalidInput) {
			t.Errorf("malformed token: %v", err)
		}
		e.complete(job, e.base.Add(time.Second), nil, workflow.Usage{})
	})
}

// TC-22 (REQ-7), storage side: the real queue applies the 5s/15s/30s/60s
// backoff, keeps usage and dead-letters the fifth attempt with its history.
func TestJobTimeoutAndExhaustionDeadLetter(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "exhaust")
	ex := e.start(workspaceA, v.ID, "exhaust-1", chainDef("a"))
	clock := &stubClock{now: e.base}
	registry := workflow.NewRegistry()
	calls := 0
	if err := registry.Register(workflow.StepKindModel, execFunc(func(context.Context, workflow.StepContext) (workflow.StepResult, error) {
		calls++
		return workflow.StepResult{Usage: workflow.Usage{InputTokens: 10}}, &workflow.StepError{Code: "provider_busy", Retryable: true}
	})); err != nil {
		t.Fatal(err)
	}
	worker := workflow.NewWorker(e.store, registry, clock, workflow.WorkerSettings{})
	delays := []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}
	for attempt := 1; attempt <= 5; attempt++ {
		worked, err := worker.ProcessNext(e.ctx)
		if !worked || err != nil {
			t.Fatalf("TC-22 attempt %d: worked=%v err=%v", attempt, worked, err)
		}
		jobs := e.jobs(ex.ID)
		if len(jobs) != 1 || jobs[0].Attempts != attempt {
			t.Fatalf("TC-22 attempt %d: jobs=%s", attempt, describeJobs(jobs))
		}
		if attempt == 5 {
			break
		}
		if jobs[0].Status != "pending" || jobs[0].LastError != "provider_busy" || !jobs[0].DueAt.Equal(clock.Now().Add(delays[attempt-1])) {
			t.Fatalf("TC-22 attempt %d: retry row %+v (want due %v)", attempt, jobs[0], clock.Now().Add(delays[attempt-1]))
		}
		clock.Advance(delays[attempt-1] - time.Second)
		if worked, _ := worker.ProcessNext(e.ctx); worked {
			t.Fatalf("TC-22 attempt %d: job ran before its backoff", attempt)
		}
		clock.Advance(time.Second)
	}
	if calls != 5 {
		t.Fatalf("executor calls %d", calls)
	}
	job := e.jobs(ex.ID)[0]
	if job.Status != "dead_letter" || job.DeadLetter != string(workflow.ReasonAttemptsExhausted) || job.HasLease || job.Token != "" {
		t.Fatalf("TC-22: dead letter row %+v", job)
	}
	got := e.execution(workspaceA, ex.ID)
	if got.State != workflow.StateFailed || got.Reason != workflow.ReasonAttemptsExhausted {
		t.Fatalf("TC-22: execution %s %s", got.State, got.Reason)
	}
	step := e.step(workspaceA, ex.ID, "a")
	if step.State != workflow.StepFailed || step.Attempts != 5 || step.Usage.InputTokens != 50 {
		t.Fatalf("TC-22: step %#v", step)
	}
	starts, retries, fails := 0, 0, 0
	for _, ev := range e.events(ex.ID) {
		switch {
		case ev.From == "QUEUED" && ev.To == "RUNNING":
			starts++
			if ev.Attempt != starts {
				t.Fatalf("attempt history out of order: %#v", ev)
			}
		case ev.From == "RUNNING" && ev.To == "QUEUED" && ev.Reason == string(workflow.ReasonStepRetry):
			retries++
		case ev.To == "FAILED":
			fails++
			if ev.Reason != string(workflow.ReasonAttemptsExhausted) || ev.Attempt != 5 {
				t.Fatalf("failure event %#v", ev)
			}
		}
	}
	if starts != 5 || retries != 4 || fails != 1 {
		t.Fatalf("TC-22: history starts=%d retries=%d fails=%d", starts, retries, fails)
	}
	rows, err := sqlc.New(e.pool).ListWorkflowDeadLetters(e.ctx, sqlc.ListWorkflowDeadLettersParams{WorkspaceID: mustDBID(t, workspaceA), RowLimit: 10})
	if err != nil || len(rows) != 1 || rows[0].DeadLetterReason.String != "attempts_exhausted" {
		t.Fatalf("TC-22: dead letters not visible: %#v %v", rows, err)
	}
}

type execFunc func(context.Context, workflow.StepContext) (workflow.StepResult, error)

func (f execFunc) Execute(ctx context.Context, sc workflow.StepContext) (workflow.StepResult, error) {
	return f(ctx, sc)
}

// TC-24 (REQ-8), storage side: a claimant that finishes after the execution
// deadline cannot commit; the deadline failure is recorded.
func TestJobDeadlineFencesLateClaimant(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "deadline")
	req := e.startReq(workspaceA, v.ID, "deadline-1", chainDef("a", "b"))
	deadline := e.base.Add(30 * time.Second)
	req.Deadline = &deadline
	ex, err := e.store.StartExecution(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	job := e.claim(e.base)
	e.fencedWrites(t, "after deadline", job, deadline.Add(time.Second))
	if err := e.store.ReconcileJobs(e.ctx, deadline.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	got := e.execution(workspaceA, ex.ID)
	if got.State != workflow.StateFailed || got.Reason != workflow.ReasonDeadlineExceeded {
		t.Fatalf("TC-24: %s %s", got.State, got.Reason)
	}
	jobs := e.jobs(ex.ID)
	if len(jobs) != 1 || jobs[0].Status != "dead_letter" || jobs[0].DeadLetter != "deadline_exceeded" {
		t.Fatalf("TC-24: jobs %s", describeJobs(jobs))
	}

	t.Run("TC-24/due job past its deadline is not claimed", func(t *testing.T) {
		req := e.startReq(workspaceA, v.ID, "deadline-2", chainDef("a"))
		d := e.base.Add(10 * time.Second)
		req.Deadline = &d
		ex, err := e.store.StartExecution(e.ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if j, err := e.store.ClaimJob(e.ctx, d); err != nil || j != nil {
			t.Fatalf("claimed at deadline: %#v %v", j, err)
		}
		if err := e.store.ReconcileJobs(e.ctx, d); err != nil {
			t.Fatal(err)
		}
		if got := e.execution(workspaceA, ex.ID); got.State != workflow.StateFailed || got.Reason != workflow.ReasonDeadlineExceeded {
			t.Fatalf("deadline not recorded: %s %s", got.State, got.Reason)
		}
	})
}

// TC-25 (REQ-5, REQ-9), storage side: a wait commits atomically and holds no
// lease or runnable job; a malformed wait changes nothing.
func TestJobWaitReleasesLease(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "wait")
	ex := e.start(workspaceA, v.ID, "wait-1", chainDef("a", "b"))
	job := e.claim(e.base)
	refs := []workflow.CheckpointReference{{Kind: "approval", ReferenceID: "501"}, {Kind: "approval", ReferenceID: "502"}}

	fp := e.fingerprint(ex.ID)
	for name, mutate := range map[string]func(*workflow.JobResultRequest){
		"not a wait state": func(r *workflow.JobResultRequest) { r.WaitState = workflow.StateRunning },
		"no references":    func(r *workflow.JobResultRequest) { r.WaitReferences = nil },
	} {
		req := resultReq(job, e.base.Add(time.Second), nil, workflow.Usage{})
		req.WaitState, req.WaitReferences, req.Reason = workflow.StateWaitingForApproval, refs, workflow.ReasonStepWaiting
		mutate(&req)
		if err := e.store.WaitJob(e.ctx, req); !errors.Is(err, workflow.ErrInvalidInput) {
			t.Fatalf("TC-25: malformed wait (%s): %v", name, err)
		}
	}
	if e.fingerprint(ex.ID) != fp {
		t.Fatal("TC-25: malformed wait wrote")
	}

	req := resultReq(job, e.base.Add(time.Second), &workflow.Checkpoint{Reusable: false}, workflow.Usage{OutputTokens: 6})
	req.WaitState, req.WaitReferences, req.Reason = workflow.StateWaitingForApproval, refs, workflow.ReasonStepWaiting
	if err := e.store.WaitJob(e.ctx, req); err != nil {
		t.Fatal(err)
	}
	got := e.execution(workspaceA, ex.ID)
	if got.State != workflow.StateWaitingForApproval || got.Reason != workflow.ReasonStepWaiting {
		t.Fatalf("TC-25: execution %s %s", got.State, got.Reason)
	}
	a := e.step(workspaceA, ex.ID, "a")
	if a.State != workflow.StepWaiting || a.Checkpoint == nil || len(a.Checkpoint.References) != 2 || a.Checkpoint.References[1].ReferenceID != "502" || a.Usage.OutputTokens != 6 {
		t.Fatalf("TC-25: waiting step %#v", a)
	}
	jobs := e.jobs(ex.ID)
	if len(jobs) != 1 || jobs[0].Status != "completed" || jobs[0].HasLease || jobs[0].Token != "" {
		t.Fatalf("TC-25: waiting execution holds %s", describeJobs(jobs))
	}
	if j, err := e.store.ClaimJob(e.ctx, e.base.Add(time.Hour)); err != nil || j != nil {
		t.Fatalf("TC-25: waiting execution runnable: %#v %v", j, err)
	}
	e.fencedWrites(t, "after wait", job, e.base.Add(2*time.Second))
}

// TC-26 (REQ-9): reconciliation recreates missing wakeups and requeues
// expired leases; the old token loses its power.
func TestReconcileRepairsMissingWakeup(t *testing.T) {
	t.Run("TC-26/missing job for a pending step", func(t *testing.T) {
		e := newWFEnv(t)
		v := e.enabledVersion(workspaceA, "wake")
		ex := e.start(workspaceA, v.ID, "wake-1", chainDef("a", "b"))
		mustExec(t, e.ctx, e.pool, `DELETE FROM jobs WHERE execution_id=$1`, ex.ID)
		if err := workflow.NewReconciler(e.store).Reconcile(e.ctx, e.base); err != nil {
			t.Fatal(err)
		}
		jobs := e.activeJobs(ex.ID)
		if len(jobs) != 1 || jobs[0].StepKey != "a" || jobs[0].Status != "pending" || jobs[0].DueAt.After(e.base) {
			t.Fatalf("missing wakeup not repaired: %s", describeJobs(jobs))
		}
		job := e.claim(e.base)
		e.complete(job, e.base, nil, workflow.Usage{})
		if e.step(workspaceA, ex.ID, "a").State != workflow.StepSucceeded {
			t.Fatal("repaired job cannot complete")
		}
	})

	t.Run("TC-26/missing job for a running step can still finish", func(t *testing.T) {
		e := newWFEnv(t)
		v := e.enabledVersion(workspaceA, "wake")
		ex := e.start(workspaceA, v.ID, "wake-2", chainDef("a", "b"))
		e.claim(e.base)
		mustExec(t, e.ctx, e.pool, `DELETE FROM jobs WHERE execution_id=$1`, ex.ID)
		at := e.base.Add(time.Second)
		if err := e.store.ReconcileJobs(e.ctx, at); err != nil {
			t.Fatal(err)
		}
		got := e.execution(workspaceA, ex.ID)
		jobs := e.activeJobs(ex.ID)
		if got.State != workflow.StateQueued || len(jobs) != 1 || jobs[0].StepKey != "a" || !jobs[0].DueAt.Equal(at.Add(5*time.Second)) {
			t.Fatalf("running step not requeued: %s %s", got.State, describeJobs(jobs))
		}
		job := e.claim(at.Add(5 * time.Second))
		req := resultReq(job, at.Add(6*time.Second), nil, workflow.Usage{})
		req.Reason = workflow.ReasonStepSucceeded
		if err := e.store.CompleteJob(e.ctx, req); err != nil {
			t.Fatalf("TC-26: repaired step cannot complete (job attempt %d, step attempts %d): %v",
				job.Attempts, e.step(workspaceA, ex.ID, "a").Attempts, err)
		}
	})

	t.Run("TC-26/missing job after a retry keeps the attempt budget", func(t *testing.T) {
		e := newWFEnv(t)
		v := e.enabledVersion(workspaceA, "wake")
		ex := e.start(workspaceA, v.ID, "wake-3", chainDef("a"))
		job := e.claim(e.base)
		req := resultReq(job, e.base, nil, workflow.Usage{})
		req.Reason, req.RetryAt = "provider_busy", e.base.Add(5*time.Second)
		if err := e.store.RetryJob(e.ctx, req); err != nil {
			t.Fatal(err)
		}
		mustExec(t, e.ctx, e.pool, `DELETE FROM jobs WHERE execution_id=$1`, ex.ID)
		if err := e.store.ReconcileJobs(e.ctx, e.base.Add(5*time.Second)); err != nil {
			t.Fatal(err)
		}
		job = e.claim(e.base.Add(5 * time.Second))
		if job.Attempts != 2 {
			t.Errorf("TC-26: repaired job restarted the attempt count at %d (step attempts %d)", job.Attempts, e.step(workspaceA, ex.ID, "a").Attempts)
		}
		req = resultReq(job, e.base.Add(6*time.Second), nil, workflow.Usage{})
		req.Reason = workflow.ReasonStepSucceeded
		if err := e.store.CompleteJob(e.ctx, req); err != nil {
			t.Fatalf("TC-26: repaired retry cannot complete: %v", err)
		}
	})

	t.Run("TC-26/expired lease is requeued with backoff", func(t *testing.T) {
		e := newWFEnv(t)
		v := e.enabledVersion(workspaceA, "wake")
		ex := e.start(workspaceA, v.ID, "wake-4", chainDef("a"))
		old := e.claim(e.base)
		at := old.LeaseExpiresAt.Add(time.Second)
		if err := e.store.ReconcileJobs(e.ctx, at); err != nil {
			t.Fatal(err)
		}
		jobs := e.jobs(ex.ID)
		if len(jobs) != 1 || jobs[0].Status != "pending" || jobs[0].Token != "" || jobs[0].HasLease ||
			jobs[0].LastError != "lease_expired" || !jobs[0].DueAt.Equal(at.Add(5*time.Second)) {
			t.Fatalf("expired lease row %s", describeJobs(jobs))
		}
		got := e.execution(workspaceA, ex.ID)
		if got.State != workflow.StateQueued || got.Reason != workflow.ReasonLeaseExpired || e.step(workspaceA, ex.ID, "a").State != workflow.StepPending {
			t.Fatalf("execution after requeue %s %s", got.State, got.Reason)
		}
		e.fencedWrites(t, "expired token", old, at)
		if j, err := e.store.ClaimJob(e.ctx, at.Add(4*time.Second)); err != nil || j != nil {
			t.Fatalf("claimed before backoff: %#v %v", j, err)
		}
		fresh := e.claim(at.Add(5 * time.Second))
		if fresh.ClaimToken == old.ClaimToken || fresh.Attempts != 2 {
			t.Fatalf("fresh claim %#v", fresh)
		}
		e.complete(fresh, at.Add(6*time.Second), nil, workflow.Usage{})
	})

	t.Run("TC-26/expired lease on the last attempt dead-letters", func(t *testing.T) {
		e := newWFEnv(t)
		v := e.enabledVersion(workspaceA, "wake")
		def := chainDef("a")
		def.Retry = workflow.RetryPolicy{MaxAttempts: 1}
		ex := e.start(workspaceA, v.ID, "wake-5", def)
		old := e.claim(e.base)
		if err := e.store.ReconcileJobs(e.ctx, old.LeaseExpiresAt); err != nil {
			t.Fatal(err)
		}
		got := e.execution(workspaceA, ex.ID)
		jobs := e.jobs(ex.ID)
		if got.State != workflow.StateFailed || got.Reason != workflow.ReasonAttemptsExhausted || jobs[0].Status != "dead_letter" {
			t.Fatalf("exhausted lease: %s %s %s", got.State, got.Reason, describeJobs(jobs))
		}
	})
}

// TC-27 (REQ-9): reconciliation never steals a live lease, never reruns a
// waiting or terminal execution and keeps uncertainty visible.
func TestReconcilePreservesLiveLeaseAndTerminalState(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "keep")
	reconcile := func(at time.Time) {
		t.Helper()
		if err := workflow.NewReconciler(e.store).Reconcile(e.ctx, at); err != nil {
			t.Fatal(err)
		}
	}

	live := e.start(workspaceA, v.ID, "keep-live", chainDef("a"))
	liveJob := e.claim(e.base)
	liveFP := e.fingerprint(live.ID)
	reconcile(e.base.Add(14 * time.Minute))
	reconcile(e.base.Add(14 * time.Minute))
	if e.fingerprint(live.ID) != liveFP {
		t.Fatal("TC-27: reconcile changed a live lease")
	}
	if err := e.store.AuthorizeAction(e.ctx, liveJob.ID, workspaceA, liveJob.ClaimToken, e.base.Add(14*time.Minute)); err != nil {
		t.Fatalf("TC-27: live claimant lost its lease: %v", err)
	}
	e.complete(liveJob, e.base.Add(14*time.Minute), nil, workflow.Usage{}) // now COMPLETED

	waiting := e.start(workspaceA, v.ID, "keep-wait", chainDef("a", "b"))
	wj := e.claim(e.base)
	wreq := resultReq(wj, e.base, nil, workflow.Usage{})
	wreq.WaitState, wreq.WaitReferences, wreq.Reason = workflow.StateWaitingForUser, []workflow.CheckpointReference{{Kind: "user_input", ReferenceID: "q1"}}, workflow.ReasonStepWaiting
	if err := e.store.WaitJob(e.ctx, wreq); err != nil {
		t.Fatal(err)
	}

	cancelled := e.start(workspaceA, v.ID, "keep-cancel", chainDef("a"))
	e.claim(e.base) // the lease of a cancelled execution later expires
	if _, err := e.store.CancelExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: cancelled.ID, IdempotencyKey: "c"}); err != nil {
		t.Fatal(err)
	}

	uncertain := e.start(workspaceA, v.ID, "keep-uncertain", chainDef("a", "b"))
	e.failJob(e.claim(e.base), e.base, workflow.ReasonSideEffectUncertain)

	ids := []int64{live.ID, waiting.ID, cancelled.ID, uncertain.ID}
	before := map[int64]string{}
	for _, id := range ids {
		before[id] = e.fingerprint(id)
	}
	reconcile(e.base.Add(2 * time.Hour))
	reconcile(e.base.Add(48 * time.Hour))
	for _, id := range ids {
		if e.fingerprint(id) != before[id] {
			t.Errorf("TC-27: reconcile changed execution %d (%s)", id, e.execution(workspaceA, id).State)
		}
	}
	if j, err := e.store.ClaimJob(e.ctx, e.base.Add(48*time.Hour)); err != nil || j != nil {
		t.Fatalf("TC-27: reconcile produced runnable work: %#v %v", j, err)
	}
	u := e.execution(workspaceA, uncertain.ID)
	if u.State != workflow.StateFailed || u.Reason != workflow.ReasonSideEffectUncertain {
		t.Fatalf("TC-27: uncertainty hidden: %s %s", u.State, u.Reason)
	}
	rows, err := sqlc.New(e.pool).ListWorkflowDeadLetters(e.ctx, sqlc.ListWorkflowDeadLettersParams{WorkspaceID: mustDBID(t, workspaceA), RowLimit: 10})
	found := false
	for _, r := range rows {
		if r.ExecutionID == uncertain.ID && r.DeadLetterReason.String == "side_effect_uncertain" {
			found = true
		}
	}
	if err != nil || !found {
		t.Fatalf("TC-27: uncertain dead letter not visible: %#v %v", rows, err)
	}
	if _, err := e.store.RetryExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: uncertain.ID, IdempotencyKey: "r"}); !errors.Is(err, workflow.ErrProviderUncertainty) {
		t.Fatalf("TC-27: uncertain retry: %v", err)
	}
	if w := e.execution(workspaceA, waiting.ID); w.State != workflow.StateWaitingForUser || len(e.activeJobs(waiting.ID)) != 0 {
		t.Fatalf("TC-27: wait not preserved: %s", w.State)
	}
	if err := workflow.NewReconciler(e.store).Reconcile(e.ctx, time.Time{}); !errors.Is(err, workflow.ErrInvalidInput) {
		t.Fatalf("zero reconcile time: %v", err)
	}
}
