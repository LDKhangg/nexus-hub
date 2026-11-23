package workflow_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// releaseCall is one ReleaseJobLease call seen by releasingStore.
type releaseCall struct {
	jobID                int64
	workspaceID, claimTk string
}

// releasingStore is the fake job store plus the optional LeaseReleaser.
type releasingStore struct {
	*fakeJobStore
	rmu      sync.Mutex
	releases []releaseCall
}

func (s *releasingStore) ReleaseJobLease(_ context.Context, jobID int64, ws, token string, at time.Time) error {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if at.IsZero() {
		panic("zero release time")
	}
	s.releases = append(s.releases, releaseCall{jobID, ws, token})
	return nil
}

func (s *releasingStore) released() []releaseCall {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	return append([]releaseCall(nil), s.releases...)
}

func phase12Worker(t *testing.T, store workflow.JobStore, workers int, exec workflow.StepExecutor) *workflow.Worker {
	t.Helper()
	registry := workflow.NewRegistry()
	if err := registry.Register(workflow.StepKindModel, exec); err != nil {
		t.Fatal(err)
	}
	clock := newFakeClock(time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC))
	return workflow.NewWorker(store, registry, clock, workflow.WorkerSettings{Workers: workers, PollInterval: time.Second})
}

func waitDone(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return", what)
	}
}

// TC-15 (REQ-4): stop ends claiming, but a step already running finishes and
// writes its result; the next queued job is never claimed.
func TestWorkerRunUntil_InFlightStepFinishesAfterStopAndNoNewClaim(t *testing.T) {
	def := defaultDefinition(t)
	store := &releasingStore{fakeJobStore: newFakeJobStore(testJob(1, 1, def), testJob(2, 1, def))}
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	exec := execFunc(func(ctx context.Context, _ workflow.StepContext) (workflow.StepResult, error) {
		started <- struct{}{}
		select {
		case <-release:
			return workflow.StepResult{}, nil
		case <-ctx.Done():
			return workflow.StepResult{}, ctx.Err()
		}
	})
	w := phase12Worker(t, store, 1, exec)
	stop, stopCancel := context.WithCancel(context.Background())
	hard, hardCancel := context.WithCancel(context.Background())
	defer hardCancel()
	done := make(chan error, 1)
	go func() { done <- w.RunUntil(stop, hard) }()
	<-started
	stopCancel()
	select {
	case <-done:
		t.Fatal("RunUntil returned while a step was still in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	waitDone(t, done, "RunUntil")
	if c := onlyCall(t, store.fakeJobStore, 1); c.method != "complete" {
		t.Fatalf("in-flight job: %#v", c)
	}
	if calls := store.callsFor(2); len(calls) != 0 {
		t.Fatalf("job 2 was claimed after stop: %#v", calls)
	}
	store.mu.Lock()
	left := len(store.jobs)
	store.mu.Unlock()
	if left != 1 {
		t.Fatalf("queued jobs left = %d, want 1 (no claim after stop)", left)
	}
	if r := store.released(); len(r) != 0 {
		t.Fatalf("a finished step released its lease: %#v", r)
	}
}

// TC-16 (REQ-4): when hard ends while a step runs, the step releases its own
// lease with its own claim token and writes no result.
func TestWorkerRunUntil_HardCutReleasesLeaseOnceAndWritesNothing(t *testing.T) {
	def := defaultDefinition(t)
	job := testJob(7, 1, def)
	store := &releasingStore{fakeJobStore: newFakeJobStore(job)}
	started := make(chan struct{}, 1)
	w := phase12Worker(t, store, 1, blockingExec(started))
	stop, stopCancel := context.WithCancel(context.Background())
	hard, hardCancel := context.WithCancel(context.Background())
	defer hardCancel()
	done := make(chan error, 1)
	go func() { done <- w.RunUntil(stop, hard) }()
	<-started
	// TC-18 (REQ-4): stop first, hard later, then both again: no panic, no
	// second release.
	stopCancel()
	hardCancel()
	stopCancel()
	hardCancel()
	waitDone(t, done, "RunUntil")
	r := store.released()
	if len(r) != 1 || r[0].jobID != 7 || r[0].workspaceID != workerTestWorkspace || r[0].claimTk != job.ClaimToken {
		t.Fatalf("lease release = %#v, want exactly one with the job's own claim", r)
	}
	if calls := store.callsFor(7); len(calls) != 0 {
		t.Fatalf("a cut-off step wrote %#v; recovery belongs to the reconciler", calls)
	}
}

// TC-16 (REQ-4): a store without LeaseReleaser still survives a hard cut.
func TestWorkerRunUntil_HardCutWithoutReleaserIsSafe(t *testing.T) {
	def := defaultDefinition(t)
	store := newFakeJobStore(testJob(8, 1, def))
	started := make(chan struct{}, 1)
	w := phase12Worker(t, store, 1, blockingExec(started))
	stop, stopCancel := context.WithCancel(context.Background())
	hard, hardCancel := context.WithCancel(context.Background())
	defer stopCancel()
	done := make(chan error, 1)
	go func() { done <- w.RunUntil(stop, hard) }()
	<-started
	hardCancel()
	waitDone(t, done, "RunUntil")
	if calls := store.callsFor(8); len(calls) != 0 {
		t.Fatalf("cut-off step wrote %#v", calls)
	}
}

// TC-18 (REQ-4): stop with nothing in flight returns at once, for several
// workers, and Run (stop == hard) behaves the same.
func TestWorkerRunUntil_StopWithNothingInFlightReturnsPromptly(t *testing.T) {
	store := newFakeJobStore()
	w := phase12Worker(t, store, 3, execFunc(func(context.Context, workflow.StepContext) (workflow.StepResult, error) {
		return workflow.StepResult{}, nil
	}))
	for name, run := range map[string]func(stop, hard context.Context) error{
		"RunUntil": w.RunUntil,
		"Run":      func(stop, _ context.Context) error { return w.Run(stop) },
	} {
		stop, stopCancel := context.WithCancel(context.Background())
		hard, hardCancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- run(stop, hard) }()
		time.Sleep(20 * time.Millisecond)
		stopCancel()
		waitDone(t, done, name)
		hardCancel()
	}
}

// TC-22 (REQ-5): several executions run at once on one worker; each step sees
// only its own execution, the queue gives each job to exactly one runner, and
// nothing deadlocks (the barrier needs 4 steps inside at the same time).
func TestWorker_ParallelExecutionsDoNotShareStateOrDeadlock(t *testing.T) {
	def := defaultDefinition(t)
	const n = 16
	jobs := make([]*workflow.Job, 0, n)
	for i := int64(1); i <= n; i++ {
		jobs = append(jobs, testJob(i, 1, def))
	}
	store := newFakeJobStore(jobs...)
	var mu sync.Mutex
	seen := map[int64]int{}
	inside := 0
	arrived := make(chan struct{})
	var once sync.Once
	exec := execFunc(func(ctx context.Context, sc workflow.StepContext) (workflow.StepResult, error) {
		mu.Lock()
		seen[sc.Execution.ID]++
		inside++
		if inside == 4 {
			once.Do(func() { close(arrived) })
		}
		mu.Unlock()
		select {
		case <-arrived:
		case <-time.After(3 * time.Second):
			return workflow.StepResult{}, &workflow.StepError{Code: "barrier_timeout"}
		}
		mu.Lock()
		inside--
		mu.Unlock()
		return workflow.StepResult{}, nil
	})
	w := phase12Worker(t, store, 8, exec)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	got := map[int64]bool{}
	for len(got) < n {
		select {
		case id := <-store.completed:
			if got[id] {
				t.Fatalf("job %d completed twice", id)
			}
			got[id] = true
		case <-time.After(10 * time.Second):
			cancel()
			t.Fatalf("only %d of %d executions completed", len(got), n)
		}
	}
	cancel()
	waitDone(t, done, "Run")
	mu.Lock()
	defer mu.Unlock()
	for i := int64(1); i <= n; i++ {
		if seen[i*100] != 1 {
			t.Fatalf("execution %d ran %d times, want 1", i*100, seen[i*100])
		}
		if c := onlyCall(t, store, i); c.method != "complete" || c.req.JobID != i {
			t.Fatalf("job %d: %#v", i, c)
		}
	}
}

// R-A-3 (REQ-4): a step that reports an uncertain side effect while the hard
// shutdown cut is happening is failed with side_effect_uncertain; its lease
// is not released, so the reconciler can never replay it.
func TestWorkerRunUntil_UncertainStepOnHardCutFailsAndKeepsLease(t *testing.T) {
	def := defaultDefinition(t)
	job := testJob(11, 1, def)
	store := &releasingStore{fakeJobStore: newFakeJobStore(job)}
	started := make(chan struct{}, 1)
	exec := execFunc(func(ctx context.Context, _ workflow.StepContext) (workflow.StepResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return workflow.StepResult{}, &workflow.StepError{Code: "merge_unknown", Uncertain: true}
	})
	w := phase12Worker(t, store, 1, exec)
	stop, stopCancel := context.WithCancel(context.Background())
	hard, hardCancel := context.WithCancel(context.Background())
	defer stopCancel()
	done := make(chan error, 1)
	go func() { done <- w.RunUntil(stop, hard) }()
	<-started
	hardCancel()
	waitDone(t, done, "RunUntil")
	c := onlyCall(t, store.fakeJobStore, 11)
	if c.method != "fail" || c.req.Reason != workflow.ReasonSideEffectUncertain {
		t.Fatalf("R-A-3: queue write %#v, want fail side_effect_uncertain", c)
	}
	if r := store.released(); len(r) != 0 {
		t.Fatalf("R-A-3: an uncertain step released its lease: %#v", r)
	}
}

// R-A-3 (REQ-4): a plain (certain) cancellation on the same cut still
// releases the lease and writes nothing, so the uncertain case is the only
// difference.
func TestWorkerRunUntil_CertainCancelStillReleasesLease(t *testing.T) {
	def := defaultDefinition(t)
	store := &releasingStore{fakeJobStore: newFakeJobStore(testJob(12, 1, def))}
	started := make(chan struct{}, 1)
	w := phase12Worker(t, store, 1, blockingExec(started))
	stop, stopCancel := context.WithCancel(context.Background())
	hard, hardCancel := context.WithCancel(context.Background())
	defer stopCancel()
	done := make(chan error, 1)
	go func() { done <- w.RunUntil(stop, hard) }()
	<-started
	hardCancel()
	waitDone(t, done, "RunUntil")
	if len(store.released()) != 1 || len(store.callsFor(12)) != 0 {
		t.Fatalf("releases %v writes %v", store.released(), store.callsFor(12))
	}
}
