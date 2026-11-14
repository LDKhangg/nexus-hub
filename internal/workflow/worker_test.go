package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

const workerTestWorkspace = "019a6690-abcd-7000-8000-000000000001"

// fakeClock is a deterministic workflow.Clock. After registers a waiter that
// fires only when Advance moves time past it.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
	notify  chan struct{}
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{now: now, notify: make(chan struct{}, 1024)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, fakeWaiter{at: c.now.Add(d), ch: ch})
	select {
	case c.notify <- struct{}{}:
	default:
	}
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
		} else {
			kept = append(kept, w)
		}
	}
	c.waiters = kept
}

// waitForWaiters blocks until at least n After calls are pending.
func (c *fakeClock) waitForWaiters(t *testing.T, n int) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		c.mu.Lock()
		got := len(c.waiters)
		c.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-c.notify:
		case <-deadline.C:
			t.Fatalf("timed out waiting for %d clock waiters (have %d)", n, got)
		}
	}
}

type storeCall struct {
	method string
	req    workflow.JobResultRequest
}

// fakeJobStore records every queue write the worker makes.
type fakeJobStore struct {
	mu           sync.Mutex
	jobs         []*workflow.Job
	claimErrs    []error
	authorizeErr error
	resultErr    map[int64]error
	calls        []storeCall
	authorized   []int64
	completed    chan int64
}

func newFakeJobStore(jobs ...*workflow.Job) *fakeJobStore {
	return &fakeJobStore{jobs: jobs, resultErr: map[int64]error{}, completed: make(chan int64, 16)}
}

func (s *fakeJobStore) ClaimJob(_ context.Context, at time.Time) (*workflow.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at.IsZero() {
		return nil, errors.New("zero claim time")
	}
	if len(s.claimErrs) > 0 {
		err := s.claimErrs[0]
		s.claimErrs = s.claimErrs[1:]
		return nil, err
	}
	if len(s.jobs) == 0 {
		return nil, nil
	}
	job := s.jobs[0]
	s.jobs = s.jobs[1:]
	return job, nil
}

func (s *fakeJobStore) AuthorizeAction(_ context.Context, jobID int64, _ string, _ string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authorized = append(s.authorized, jobID)
	return s.authorizeErr
}

func (s *fakeJobStore) CheckpointJob(context.Context, workflow.JobCheckpointRequest) (workflow.Step, error) {
	return workflow.Step{}, nil
}

func (s *fakeJobStore) record(method string, req workflow.JobResultRequest) error {
	s.mu.Lock()
	s.calls = append(s.calls, storeCall{method: method, req: req})
	err := s.resultErr[req.JobID]
	s.mu.Unlock()
	if method == "complete" && err == nil {
		s.completed <- req.JobID
	}
	return err
}

func (s *fakeJobStore) CompleteJob(_ context.Context, req workflow.JobResultRequest) error {
	return s.record("complete", req)
}
func (s *fakeJobStore) WaitJob(_ context.Context, req workflow.JobResultRequest) error {
	return s.record("wait", req)
}
func (s *fakeJobStore) RetryJob(_ context.Context, req workflow.JobResultRequest) error {
	return s.record("retry", req)
}
func (s *fakeJobStore) FailJob(_ context.Context, req workflow.JobResultRequest) error {
	return s.record("fail", req)
}

func (s *fakeJobStore) snapshot() []storeCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]storeCall(nil), s.calls...)
}

func (s *fakeJobStore) callsFor(jobID int64) []storeCall {
	var out []storeCall
	for _, c := range s.snapshot() {
		if c.req.JobID == jobID {
			out = append(out, c)
		}
	}
	return out
}

type execFunc func(context.Context, workflow.StepContext) (workflow.StepResult, error)

func (f execFunc) Execute(ctx context.Context, sc workflow.StepContext) (workflow.StepResult, error) {
	return f(ctx, sc)
}

// defaultDefinition is the schema 1 default graph, so backoff assertions
// use the shipped 5s/15s/30s/60s policy rather than a test copy.
func defaultDefinition(t *testing.T) workflow.Definition {
	t.Helper()
	d, err := behavior.NormalizeWorkflow(behavior.ResolvedDoc{Schema: 1, Instructions: "summarize"})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func testJob(id int64, attempts int, def workflow.Definition) *workflow.Job {
	return &workflow.Job{
		ID: id, WorkspaceID: workerTestWorkspace, ExecutionID: id * 100, StepKey: "model",
		Attempts: attempts, MaxAttempts: def.Retry.MaxAttempts, ClaimToken: fmt.Sprintf("019a6690-abcd-7000-8000-%012d", id),
		Execution: workflow.Execution{ID: id * 100, WorkspaceID: workerTestWorkspace, State: workflow.StateRunning, Definition: def},
		Step:      workflow.Step{Key: "model", Kind: workflow.StepKindModel, Attempts: attempts},
	}
}

func newTestWorker(t *testing.T, store workflow.JobStore, clock workflow.Clock, kind string, exec workflow.StepExecutor) *workflow.Worker {
	t.Helper()
	registry := workflow.NewRegistry()
	if exec != nil {
		if err := registry.Register(kind, exec); err != nil {
			t.Fatal(err)
		}
	}
	return workflow.NewWorker(store, registry, clock, workflow.WorkerSettings{Workers: 1, PollInterval: time.Second})
}

func onlyCall(t *testing.T, store *fakeJobStore, jobID int64) storeCall {
	t.Helper()
	calls := store.callsFor(jobID)
	if len(calls) != 1 {
		t.Fatalf("job %d: want exactly one queue write, got %#v", jobID, calls)
	}
	return calls[0]
}

// TC-22 (REQ-7): retryable failures back off 5s/15s/30s/60s, carry usage and
// checkpoint, and the fifth attempt dead-letters with attempts_exhausted.
func TestJobRetryBackoffAndExhaustionDeadLetter(t *testing.T) {
	def := defaultDefinition(t)
	if def.Retry.MaxAttempts != 5 || !reflect.DeepEqual(def.Retry.Delays, []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}) {
		t.Fatalf("TC-22: default retry policy = %#v", def.Retry)
	}
	clock := newFakeClock(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
	checkpoint := &workflow.Checkpoint{References: []workflow.CheckpointReference{{Kind: "tool_run", ReferenceID: "41"}}, Reusable: true}
	calls := 0
	exec := execFunc(func(context.Context, workflow.StepContext) (workflow.StepResult, error) {
		calls++
		return workflow.StepResult{Usage: workflow.Usage{InputTokens: 7, OutputTokens: 3}, Checkpoint: checkpoint},
			&workflow.StepError{Code: "provider_busy", Retryable: true}
	})
	store := newFakeJobStore()
	worker := newTestWorker(t, store, clock, workflow.StepKindModel, exec)
	wantDelays := []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}
	for attempt := 1; attempt <= 5; attempt++ {
		job := testJob(int64(attempt), attempt, def)
		store.jobs = []*workflow.Job{job}
		now := clock.Now()
		worked, err := worker.ProcessNext(context.Background())
		if !worked || err != nil {
			t.Fatalf("attempt %d: worked=%v err=%v", attempt, worked, err)
		}
		call := onlyCall(t, store, job.ID)
		if call.req.ExpectedAttempt != attempt || call.req.ClaimToken != job.ClaimToken || call.req.WorkspaceID != workerTestWorkspace ||
			call.req.ExecutionID != job.ExecutionID || call.req.StepKey != "model" {
			t.Fatalf("attempt %d: request not fenced to the claim: %#v", attempt, call.req)
		}
		if call.req.Usage != (workflow.Usage{InputTokens: 7, OutputTokens: 3}) || call.req.Checkpoint == nil || !reflect.DeepEqual(*call.req.Checkpoint, *checkpoint) {
			t.Fatalf("attempt %d: usage/checkpoint not carried: %#v", attempt, call.req)
		}
		if attempt < 5 {
			if call.method != "retry" || call.req.Reason != "provider_busy" {
				t.Fatalf("attempt %d: want retry provider_busy, got %s %q", attempt, call.method, call.req.Reason)
			}
			if got := call.req.RetryAt.Sub(now); got != wantDelays[attempt-1] {
				t.Fatalf("TC-22: attempt %d backoff = %v, want %v", attempt, got, wantDelays[attempt-1])
			}
		} else if call.method != "fail" || call.req.Reason != workflow.ReasonAttemptsExhausted {
			t.Fatalf("TC-22: attempt 5 want fail attempts_exhausted, got %s %q", call.method, call.req.Reason)
		}
		clock.Advance(time.Minute)
	}
	if calls != 5 {
		t.Fatalf("executor ran %d times, want 5", calls)
	}

	t.Run("TC-22/custom policy caps at its own max attempts", func(t *testing.T) {
		custom := def
		custom.Retry = workflow.RetryPolicy{MaxAttempts: 3, Delays: []time.Duration{2 * time.Second, 9 * time.Second}}
		store := newFakeJobStore(testJob(21, 1, custom), testJob(22, 2, custom), testJob(23, 3, custom))
		worker := newTestWorker(t, store, clock, workflow.StepKindModel, exec)
		for i := 0; i < 3; i++ {
			if _, err := worker.ProcessNext(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		now := clock.Now()
		if c := onlyCall(t, store, 21); c.method != "retry" || c.req.RetryAt.Sub(now) != 2*time.Second {
			t.Fatalf("attempt 1: %#v", c)
		}
		if c := onlyCall(t, store, 22); c.method != "retry" || c.req.RetryAt.Sub(now) != 9*time.Second {
			t.Fatalf("attempt 2: %#v", c)
		}
		if c := onlyCall(t, store, 23); c.method != "fail" || c.req.Reason != workflow.ReasonAttemptsExhausted {
			t.Fatalf("attempt 3: %#v", c)
		}
	})
}

// TC-23 (REQ-7, REQ-9): non-retryable fails at once; unclassified and
// uncertain errors fail closed as side_effect_uncertain and are never resent.
func TestWorkerClassifiesExecutorErrors(t *testing.T) {
	def := defaultDefinition(t)
	cases := []struct {
		name   string
		err    error
		reason workflow.ReasonCode
	}{
		{"TC-23/non-retryable", &workflow.StepError{Code: "bad_request"}, "bad_request"},
		{"TC-23/non-retryable without code", &workflow.StepError{}, workflow.ReasonNonRetryable},
		{"TC-23/unclassified error", errors.New("dial tcp: connection reset; token=sk-ant-PRIVATE"), workflow.ReasonSideEffectUncertain},
		{"TC-23/provider uncertainty sentinel", fmt.Errorf("send: %w", workflow.ErrProviderUncertainty), workflow.ReasonSideEffectUncertain},
		{"TC-23/uncertain beats retryable", &workflow.StepError{Code: "provider_busy", Retryable: true, Uncertain: true}, workflow.ReasonSideEffectUncertain},
		{"TC-23/wrapped uncertain step error", fmt.Errorf("x: %w", &workflow.StepError{Uncertain: true}), workflow.ReasonSideEffectUncertain},
		{"TC-23/unsafe reason code is sanitized", &workflow.StepError{Code: "Bad Code; sk-ant-PRIVATE"}, workflow.ReasonNonRetryable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
			calls := 0
			exec := execFunc(func(context.Context, workflow.StepContext) (workflow.StepResult, error) {
				calls++
				return workflow.StepResult{}, tc.err
			})
			store := newFakeJobStore(testJob(1, 1, def))
			worker := newTestWorker(t, store, clock, workflow.StepKindModel, exec)
			if _, err := worker.ProcessNext(context.Background()); err != nil {
				t.Fatal(err)
			}
			call := onlyCall(t, store, 1)
			if call.method != "fail" || call.req.Reason != tc.reason {
				t.Fatalf("want fail %q, got %s %q", tc.reason, call.method, call.req.Reason)
			}
			if strings.Contains(string(call.req.Reason), "PRIVATE") {
				t.Fatal("raw error text persisted as reason")
			}
			if worked, err := worker.ProcessNext(context.Background()); worked || err != nil || calls != 1 {
				t.Fatalf("failed step was resent: worked=%v err=%v calls=%d", worked, err, calls)
			}
		})
	}

	t.Run("TC-23/unregistered kind fails without executing", func(t *testing.T) {
		clock := newFakeClock(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
		job := testJob(2, 1, def)
		job.Step.Kind = "shell.exec"
		store := newFakeJobStore(job)
		called := false
		worker := newTestWorker(t, store, clock, workflow.StepKindModel, execFunc(func(context.Context, workflow.StepContext) (workflow.StepResult, error) {
			called = true
			return workflow.StepResult{}, nil
		}))
		if _, err := worker.ProcessNext(context.Background()); err != nil {
			t.Fatal(err)
		}
		if c := onlyCall(t, store, 2); c.method != "fail" || c.req.Reason != workflow.ReasonNonRetryable || called {
			t.Fatalf("unregistered kind: %#v called=%v", c, called)
		}
	})

	t.Run("TC-23/registry refuses to replace a pinned kind", func(t *testing.T) {
		r := workflow.NewRegistry()
		e := execFunc(func(context.Context, workflow.StepContext) (workflow.StepResult, error) {
			return workflow.StepResult{}, nil
		})
		if err := r.Register(workflow.StepKindModel, e); err != nil {
			t.Fatal(err)
		}
		if err := r.Register(workflow.StepKindModel, e); !errors.Is(err, workflow.ErrConflict) {
			t.Fatalf("replace: %v", err)
		}
	})
}

// runAsync runs ProcessNext in the background and returns its result channel.
func runAsync(w *workflow.Worker) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := w.ProcessNext(context.Background())
		done <- err
	}()
	return done
}

func awaitResult(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ProcessNext: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ProcessNext did not return")
	}
}

func blockingExec(started chan<- struct{}) workflow.StepExecutor {
	return execFunc(func(ctx context.Context, _ workflow.StepContext) (workflow.StepResult, error) {
		if started != nil {
			started <- struct{}{}
		}
		<-ctx.Done()
		return workflow.StepResult{Usage: workflow.Usage{InputTokens: 4}}, ctx.Err()
	})
}

// TC-24 (REQ-8): deadlines and step timeouts are recorded with the right
// reason and follow the retry policy; a stale claim never executes.
func TestWorkerDeadlineAndStepTimeout(t *testing.T) {
	def := defaultDefinition(t)

	t.Run("TC-24/deadline already passed fails without executing", func(t *testing.T) {
		clock := newFakeClock(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
		job := testJob(1, 1, def)
		past := clock.Now().Add(-time.Second)
		job.Execution.Deadline = &past
		called := false
		store := newFakeJobStore(job)
		worker := newTestWorker(t, store, clock, workflow.StepKindModel, execFunc(func(context.Context, workflow.StepContext) (workflow.StepResult, error) {
			called = true
			return workflow.StepResult{}, nil
		}))
		if _, err := worker.ProcessNext(context.Background()); err != nil {
			t.Fatal(err)
		}
		if c := onlyCall(t, store, 1); c.method != "fail" || c.req.Reason != workflow.ReasonDeadlineExceeded || called {
			t.Fatalf("expired deadline: %#v called=%v", c, called)
		}
	})

	t.Run("TC-24/deadline expires mid-attempt and is not retried", func(t *testing.T) {
		clock := newFakeClock(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
		job := testJob(2, 1, def)
		deadline := clock.Now().Add(30 * time.Second)
		job.Execution.Deadline = &deadline
		store := newFakeJobStore(job)
		worker := newTestWorker(t, store, clock, workflow.StepKindModel, blockingExec(nil))
		done := runAsync(worker)
		clock.waitForWaiters(t, 1)
		clock.Advance(29 * time.Second)
		if len(store.callsFor(2)) != 0 {
			t.Fatal("attempt ended before the deadline")
		}
		clock.Advance(time.Second)
		awaitResult(t, done)
		c := onlyCall(t, store, 2)
		if c.method != "fail" || c.req.Reason != workflow.ReasonDeadlineExceeded {
			t.Fatalf("mid-attempt deadline: %s %q", c.method, c.req.Reason)
		}
		if c.req.Usage.InputTokens != 4 {
			t.Fatalf("usage of the timed-out attempt lost: %#v", c.req.Usage)
		}
	})

	t.Run("TC-24/step timeout before deadline follows retry policy", func(t *testing.T) {
		clock := newFakeClock(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
		short := def
		short.StepTimeout = time.Minute
		short.LeaseDuration = 2 * time.Minute
		job := testJob(3, 1, short)
		deadline := clock.Now().Add(time.Hour)
		job.Execution.Deadline = &deadline
		store := newFakeJobStore(job)
		worker := newTestWorker(t, store, clock, workflow.StepKindModel, blockingExec(nil))
		done := runAsync(worker)
		clock.waitForWaiters(t, 1)
		clock.Advance(time.Minute)
		awaitResult(t, done)
		c := onlyCall(t, store, 3)
		// deadline_exceeded is the only timeout reason in the vocabulary.
		if c.method != "retry" || c.req.Reason != workflow.ReasonDeadlineExceeded {
			t.Fatalf("step timeout: %s %q", c.method, c.req.Reason)
		}
		if got := c.req.RetryAt.Sub(clock.Now()); got != 5*time.Second {
			t.Fatalf("step timeout backoff %v, want 5s", got)
		}
	})

	t.Run("TC-24/step timeout on the last attempt dead-letters", func(t *testing.T) {
		clock := newFakeClock(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
		short := def
		short.StepTimeout = time.Minute
		short.LeaseDuration = 2 * time.Minute
		store := newFakeJobStore(testJob(4, 5, short))
		worker := newTestWorker(t, store, clock, workflow.StepKindModel, blockingExec(nil))
		done := runAsync(worker)
		clock.waitForWaiters(t, 1)
		clock.Advance(time.Minute)
		awaitResult(t, done)
		if c := onlyCall(t, store, 4); c.method != "fail" || c.req.Reason != workflow.ReasonAttemptsExhausted {
			t.Fatalf("last-attempt timeout: %s %q", c.method, c.req.Reason)
		}
	})

	t.Run("TC-24/stale claim is not executed", func(t *testing.T) {
		clock := newFakeClock(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
		store := newFakeJobStore(testJob(5, 1, def))
		store.authorizeErr = workflow.ErrStaleClaim
		called := false
		worker := newTestWorker(t, store, clock, workflow.StepKindModel, execFunc(func(context.Context, workflow.StepContext) (workflow.StepResult, error) {
			called = true
			return workflow.StepResult{}, nil
		}))
		_, err := worker.ProcessNext(context.Background())
		if !errors.Is(err, workflow.ErrStaleClaim) || called || len(store.callsFor(5)) != 0 {
			t.Fatalf("stale claim: err=%v called=%v calls=%#v", err, called, store.callsFor(5))
		}
	})
}

// TC-25 (REQ-5, REQ-9): a valid wait is committed through WaitJob; a
// malformed wait fails closed instead of completing or keeping the job.
func TestWorkerWaitResults(t *testing.T) {
	def := defaultDefinition(t)
	refs := []workflow.CheckpointReference{{Kind: "approval", ReferenceID: "77"}, {Kind: "approval", ReferenceID: "78"}}
	for _, state := range []workflow.State{workflow.StateWaitingForApproval, workflow.StateWaitingForUser} {
		t.Run("TC-25/valid "+string(state), func(t *testing.T) {
			clock := newFakeClock(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
			store := newFakeJobStore(testJob(1, 1, def))
			worker := newTestWorker(t, store, clock, workflow.StepKindModel, execFunc(func(context.Context, workflow.StepContext) (workflow.StepResult, error) {
				return workflow.StepResult{WaitState: state, WaitReferences: refs, Usage: workflow.Usage{OutputTokens: 2}}, nil
			}))
			if _, err := worker.ProcessNext(context.Background()); err != nil {
				t.Fatal(err)
			}
			c := onlyCall(t, store, 1)
			if c.method != "wait" || c.req.WaitState != state || !reflect.DeepEqual(c.req.WaitReferences, refs) ||
				c.req.Reason != workflow.ReasonStepWaiting || c.req.Usage.OutputTokens != 2 || c.req.ClaimToken == "" {
				t.Fatalf("wait request: %#v", c)
			}
		})
	}
	malformed := []struct {
		name   string
		result workflow.StepResult
	}{
		{"TC-25/non-waiting state", workflow.StepResult{WaitState: workflow.StateRunning, WaitReferences: refs}},
		{"TC-25/terminal state", workflow.StepResult{WaitState: workflow.StateCompleted, WaitReferences: refs}},
		{"TC-25/unknown state", workflow.StepResult{WaitState: "WAITING", WaitReferences: refs}},
		{"TC-25/wait without references", workflow.StepResult{WaitState: workflow.StateWaitingForApproval}},
	}
	for _, tc := range malformed {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
			store := newFakeJobStore(testJob(2, 1, def))
			worker := newTestWorker(t, store, clock, workflow.StepKindModel, execFunc(func(context.Context, workflow.StepContext) (workflow.StepResult, error) {
				return tc.result, nil
			}))
			if _, err := worker.ProcessNext(context.Background()); err != nil {
				t.Fatal(err)
			}
			if c := onlyCall(t, store, 2); c.method != "fail" || c.req.Reason != workflow.ReasonNonRetryable {
				t.Fatalf("malformed wait not failed closed: %s %q", c.method, c.req.Reason)
			}
		})
	}
}

// TC-28 (REQ-5, REQ-9): one failing execution or storage error does not
// block independent due work.
func TestFailedExecutionDoesNotStopOtherJobs(t *testing.T) {
	def := defaultDefinition(t)
	ok := execFunc(func(_ context.Context, sc workflow.StepContext) (workflow.StepResult, error) {
		if sc.Execution.ID == 100 {
			return workflow.StepResult{}, &workflow.StepError{Code: "bad_request"}
		}
		return workflow.StepResult{Checkpoint: &workflow.Checkpoint{Reusable: true}}, nil
	})

	t.Run("TC-28/claim error then next job runs", func(t *testing.T) {
		clock := newFakeClock(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
		store := newFakeJobStore(testJob(2, 1, def))
		store.claimErrs = []error{errors.New("connection reset")}
		worker := newTestWorker(t, store, clock, workflow.StepKindModel, ok)
		if worked, err := worker.ProcessNext(context.Background()); worked || err == nil {
			t.Fatalf("claim error hidden: worked=%v err=%v", worked, err)
		}
		if worked, err := worker.ProcessNext(context.Background()); !worked || err != nil {
			t.Fatalf("second poll: worked=%v err=%v", worked, err)
		}
		if c := onlyCall(t, store, 2); c.method != "complete" {
			t.Fatalf("job 2: %#v", c)
		}
	})

	t.Run("TC-28/storage failure on one execution does not block the next", func(t *testing.T) {
		clock := newFakeClock(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
		store := newFakeJobStore(testJob(1, 1, def), testJob(2, 1, def))
		store.resultErr[1] = errors.New("deadlock detected")
		worker := newTestWorker(t, store, clock, workflow.StepKindModel, ok)
		if _, err := worker.ProcessNext(context.Background()); err == nil {
			t.Fatal("storage error on job 1 hidden")
		}
		if worked, err := worker.ProcessNext(context.Background()); !worked || err != nil {
			t.Fatalf("job 2: worked=%v err=%v", worked, err)
		}
		if c := onlyCall(t, store, 2); c.method != "complete" {
			t.Fatalf("job 2: %#v", c)
		}
	})

	t.Run("TC-28/a hung execution does not block another worker", func(t *testing.T) {
		clock := newFakeClock(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
		hung := testJob(1, 1, def)
		hung.Execution.ID = 999
		store := newFakeJobStore(hung, testJob(2, 1, def))
		exec := execFunc(func(ctx context.Context, sc workflow.StepContext) (workflow.StepResult, error) {
			if sc.Execution.ID == 999 {
				<-ctx.Done()
				return workflow.StepResult{}, ctx.Err()
			}
			return workflow.StepResult{}, nil
		})
		registry := workflow.NewRegistry()
		if err := registry.Register(workflow.StepKindModel, exec); err != nil {
			t.Fatal(err)
		}
		worker := workflow.NewWorker(store, registry, clock, workflow.WorkerSettings{Workers: 2, PollInterval: time.Second})
		ctx, cancel := context.WithCancel(context.Background())
		runDone := make(chan error, 1)
		go func() { runDone <- worker.Run(ctx) }()
		select {
		case id := <-store.completed:
			if id != 2 {
				t.Fatalf("completed job %d", id)
			}
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("job 2 was blocked by the hung execution")
		}
		cancel()
		select {
		case err := <-runDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not stop after cancel")
		}
		for _, c := range store.callsFor(1) {
			if c.method == "complete" {
				t.Fatal("cancelled hung job was completed")
			}
		}
	})
}
