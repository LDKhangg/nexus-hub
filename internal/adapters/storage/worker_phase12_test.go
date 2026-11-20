package storage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// phase12Observer records the worker job changes the store announces.
type phase12Observer struct {
	mu      sync.Mutex
	changes []struct {
		kind string
		job  behavior.WorkerJob
	}
}

func (o *phase12Observer) WorkerJobChanged(_ context.Context, kind string, job behavior.WorkerJob) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.changes = append(o.changes, struct {
		kind string
		job  behavior.WorkerJob
	}{kind, job})
}

func (o *phase12Observer) finished(state string) map[string]int {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := map[string]int{}
	for _, c := range o.changes {
		if c.kind == behavior.WorkerChangeFinished && c.job.State == state {
			out[c.job.ID]++
		}
	}
	return out
}

func (e *w7Env) countEvents(t *testing.T, kindReason string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM worker_events WHERE kind || ':' || reason_code = $1`, kindReason).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TC-24 (REQ-5): a worker that dies mid-job has its lease expire; the job is
// kept as INTERRUPTED (not lost, not failed), with an audit event, and the
// device is free again.
func TestPhase12DeadWorkerLeaseExpiresToInterrupted(t *testing.T) {
	e := newW7(t)
	d, j, _ := interrupted(t, e)
	if st := e.jobState(t, j.ID); st != behavior.WorkerJobInterrupted {
		t.Fatalf("TC-24: state %s", st)
	}
	if e.countEvents(t, "lease_lost:lease_expired") != 1 {
		t.Fatalf("TC-24: events %v", e.events(t))
	}
	var attempt int
	if err := e.pool.QueryRow(e.ctx, `SELECT attempt FROM worker_jobs WHERE id=$1`, j.ID).Scan(&attempt); err != nil || attempt != 1 {
		t.Fatalf("TC-24: attempt %d %v", attempt, err)
	}
	e.job(t, workspaceA, nil, "")
	e.mustLease(t, d)
}

// TC-25 (REQ-5): the owner may retry an interrupted job until it has used
// WorkerMaxAttempts leases; then Retry is refused with ErrWorkerRetryLimit,
// audited, double taps change nothing, and a stale attempt number is a plain
// conflict.
func TestPhase12RetryStopsAtAttemptLimit(t *testing.T) {
	b := newB3Env(t)
	d, _ := b.enroll(t, workspaceA, "laptop", "repo.write")
	ex, j := b.endedJob(t, d, behavior.WorkerJobInterrupted)
	for attempt := 1; attempt < behavior.WorkerMaxAttempts; attempt++ {
		retried, err := b.s.RetryJob(b.ctx, workspaceA, j.ID, attempt)
		if err != nil || retried.State != behavior.WorkerJobWaiting {
			t.Fatalf("TC-25: retry at attempt %d: %+v %v", attempt, retried.State, err)
		}
		l := b.mustLease(t, d)
		if l.Job.Attempt != attempt+1 {
			t.Fatalf("TC-25: lease is attempt %d, want %d", l.Job.Attempt, attempt+1)
		}
		b.advance(11 * time.Minute)
		if n, err := b.s.ExpireLeases(b.ctx); err != nil || n < 1 {
			t.Fatalf("TC-25: ExpireLeases = %d %v", n, err)
		}
	}
	at := b.jobOf(t, ex)
	if at.State != behavior.WorkerJobInterrupted || at.Attempt != behavior.WorkerMaxAttempts {
		t.Fatalf("TC-25: job %s attempt %d, want INTERRUPTED at the limit", at.State, at.Attempt)
	}
	execBefore := b.execState(ex)
	for i := 0; i < 2; i++ {
		_, err := b.s.RetryJob(b.ctx, workspaceA, j.ID, at.Attempt)
		if !errors.Is(err, behavior.ErrWorkerRetryLimit) {
			t.Fatalf("TC-25: retry %d at the limit = %v, want ErrWorkerRetryLimit", i+1, err)
		}
	}
	if !errors.Is(behavior.ErrWorkerRetryLimit, behavior.ErrConflict) {
		t.Fatal("TC-25: ErrWorkerRetryLimit must stay an ErrConflict for older callers")
	}
	if b.countEvents(t, behavior.WorkerEventRejected+":"+behavior.WorkerReasonAttemptsExhausted) < 1 {
		t.Fatalf("TC-25: refusal not audited: %v", b.events(t))
	}
	after := b.jobOf(t, ex)
	if after.State != behavior.WorkerJobInterrupted || after.Attempt != at.Attempt || after.DeviceID != at.DeviceID {
		t.Fatalf("TC-25: refused retry changed the job: %+v", after)
	}
	if st := b.execState(ex); st.State != execBefore.State {
		t.Fatalf("TC-25: refused retry changed the execution %s -> %s", execBefore.State, st.State)
	}
	if _, err := b.s.RetryJob(b.ctx, workspaceA, j.ID, at.Attempt-1); errors.Is(err, behavior.ErrWorkerRetryLimit) || !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("TC-25: stale attempt = %v, want a plain conflict", err)
	}
	if _, err := b.s.RetryJob(b.ctx, workspaceB, j.ID, at.Attempt); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("TC-25: other workspace = %v, want not found", err)
	}
	// Cancel still works at the limit.
	if got, err := b.s.CancelJob(b.ctx, workspaceA, j.ID); err != nil || got.State != behavior.WorkerJobCancelled {
		t.Fatalf("TC-25: cancel at the limit: %+v %v", got.State, err)
	}
}

// TC-26 (REQ-5): an interrupted job older than 24 h becomes FAILED with
// lease_expired in the next sweep; younger ones are untouched; the sweep is
// idempotent, audits once and announces one finished change per job.
func TestPhase12InterruptedOver24hFailsOnce(t *testing.T) {
	e := newW7(t)
	obs := &phase12Observer{}
	e.s.SetObserver(obs)
	_, j, _ := interrupted(t, e)
	e.advance(23*time.Hour + 59*time.Minute)
	if _, err := e.s.ExpireLeases(e.ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.jobState(t, j.ID); st != behavior.WorkerJobInterrupted {
		t.Fatalf("TC-26: at 23h59 state %s, want INTERRUPTED", st)
	}
	e.advance(2 * time.Minute)
	if n, err := e.s.ExpireLeases(e.ctx); err != nil || n != 0 {
		t.Fatalf("TC-26: ExpireLeases = %d %v (the count is expired leases only)", n, err)
	}
	if st := e.jobState(t, j.ID); st != behavior.WorkerJobFailed {
		t.Fatalf("TC-26: after 24h state %s, want FAILED", st)
	}
	var reason string
	var finished bool
	if err := e.pool.QueryRow(e.ctx, `SELECT result->>'reason_code', finished_at IS NOT NULL FROM worker_jobs WHERE id=$1`, j.ID).Scan(&reason, &finished); err != nil ||
		reason != behavior.WorkerReasonLeaseExpired || !finished {
		t.Fatalf("TC-26: result reason %q finished %v %v", reason, finished, err)
	}
	for i := 0; i < 3; i++ {
		if _, err := e.s.ExpireLeases(e.ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.countEvents(t, behavior.WorkerEventResultPrefix+behavior.WorkerStatusFailed+":"+behavior.WorkerReasonLeaseExpired); n != 1 {
		t.Fatalf("TC-26: %d audit events, want 1: %v", n, e.events(t))
	}
	if got := obs.finished(behavior.WorkerJobFailed); len(got) != 1 || got[j.ID] != 1 {
		t.Fatalf("TC-26: finished notices %v, want exactly one for the job", got)
	}
	// A job that is already FAILED can be retried by the owner? Not here: the
	// sweep never resurrects or re-fails it.
	if st := e.jobState(t, j.ID); st != behavior.WorkerJobFailed {
		t.Fatalf("TC-26: state drifted to %s", st)
	}
}

// TC-26 (REQ-5, abuse of volume): 150 stale interrupted jobs are failed 100
// per pass; a young interrupted job, a succeeded job and a cancelled job are
// never touched; each failed job is announced once.
func TestPhase12StaleInterruptedSweepIsBoundedAndSelective(t *testing.T) {
	e := newW7(t)
	obs := &phase12Observer{}
	e.s.SetObserver(obs)
	d, _ := e.enroll(t, workspaceA, "laptop")
	mk := func(state string, age time.Duration) string {
		j := e.job(t, workspaceA, nil, "")
		var interruptedAt any
		if state == behavior.WorkerJobInterrupted {
			interruptedAt = e.clock.Add(-age)
		}
		mustExec(t, e.ctx, e.pool, `UPDATE worker_jobs SET state=$2, device_id=$3, interrupted_at=$4 WHERE id=$1`, j.ID, state, d.ID, interruptedAt)
		return j.ID
	}
	const stale = 150
	for i := 0; i < stale; i++ {
		mk(behavior.WorkerJobInterrupted, 25*time.Hour)
	}
	young := mk(behavior.WorkerJobInterrupted, time.Hour)
	done := mk(behavior.WorkerJobSucceeded, 0)
	cancelled := mk(behavior.WorkerJobCancelled, 0)
	count := func(state string) int {
		var n int
		if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM worker_jobs WHERE state=$1`, state).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := e.s.ExpireLeases(e.ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(behavior.WorkerJobFailed); n != 100 {
		t.Fatalf("TC-26: first pass failed %d jobs, want the batch bound 100", n)
	}
	if _, err := e.s.ExpireLeases(e.ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(behavior.WorkerJobFailed); n != stale {
		t.Fatalf("TC-26: second pass total failed %d, want %d", n, stale)
	}
	if _, err := e.s.ExpireLeases(e.ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(behavior.WorkerJobFailed); n != stale {
		t.Fatalf("TC-26: third pass changed the total to %d", n)
	}
	if e.jobState(t, young) != behavior.WorkerJobInterrupted || e.jobState(t, done) != behavior.WorkerJobSucceeded || e.jobState(t, cancelled) != behavior.WorkerJobCancelled {
		t.Fatal("TC-26: the sweep touched a job that is not a stale interrupted one")
	}
	got := obs.finished(behavior.WorkerJobFailed)
	if len(got) != stale {
		t.Fatalf("TC-26: %d jobs announced, want %d", len(got), stale)
	}
	for id, n := range got {
		if n != 1 {
			t.Fatalf("TC-26: job %s announced %d times", id, n)
		}
	}
}

// TC-27 (REQ-5): cancelling is unchanged: a cancelled job stays cancelled
// through the 24 h sweep, and a running job cancels through CANCELLING.
func TestPhase12CancelUnchangedByExpiry(t *testing.T) {
	e := newW7(t)
	_, jj, _ := interrupted(t, e)
	if got, err := e.s.CancelJob(e.ctx, workspaceA, jj.ID); err != nil || got.State != behavior.WorkerJobCancelled {
		t.Fatalf("TC-27: cancel interrupted: %+v %v", got.State, err)
	}
	e.advance(48 * time.Hour)
	if _, err := e.s.ExpireLeases(e.ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.jobState(t, jj.ID); st != behavior.WorkerJobCancelled {
		t.Fatalf("TC-27: cancelled job became %s", st)
	}

	e2 := newW7(t)
	d, _ := e2.enroll(t, workspaceA, "laptop")
	j := e2.job(t, workspaceA, nil, "")
	e2.mustLease(t, d)
	if got, err := e2.s.CancelJob(e2.ctx, workspaceA, j.ID); err != nil || got.State != behavior.WorkerJobCancelling {
		t.Fatalf("TC-27: cancel running: %+v %v", got.State, err)
	}
	e2.advance(11 * time.Minute)
	if n, err := e2.s.ExpireLeases(e2.ctx); err != nil || n != 1 {
		t.Fatalf("TC-27: expire cancelling = %d %v", n, err)
	}
	if st := e2.jobState(t, j.ID); st != behavior.WorkerJobCancelled {
		t.Fatalf("TC-27: cancelling job became %s", st)
	}
}
