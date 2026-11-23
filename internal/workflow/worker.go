package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/obs"
)

// Clock is the worker's only source of time. Tests and embedded runtimes can
// supply a deterministic implementation for leases, deadlines and backoff.
type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Job is a leased unit of serial execution work.
type Job struct {
	ID             int64
	WorkspaceID    string
	ExecutionID    int64
	StepKey        string
	Attempts       int
	MaxAttempts    int
	ClaimToken     string
	LeaseExpiresAt time.Time
	Execution      Execution
	Step           Step
}

// JobCheckpointRequest records cumulative usage and a bounded checkpoint.
// The write succeeds only while the supplied claim is current and authorized.
type JobCheckpointRequest struct {
	JobID           int64
	WorkspaceID     string
	ExecutionID     int64
	StepKey         string
	ExpectedAttempt int
	ClaimToken      string
	IdempotencyKey  string
	At              time.Time
	Checkpoint      *Checkpoint
	Usage           Usage
}

// JobResultRequest atomically ends one attempt.
type JobResultRequest struct {
	JobCheckpointRequest
	WaitState      State
	WaitReferences []CheckpointReference
	Reason         ReasonCode
	RetryAt        time.Time
}

// JobStore is the transactional queue seam. Implementations must fence every
// mutating method by the active claim token, execution state and deadline.
type JobStore interface {
	ClaimJob(context.Context, time.Time) (*Job, error)
	AuthorizeAction(context.Context, int64, string, string, time.Time) error
	CheckpointJob(context.Context, JobCheckpointRequest) (Step, error)
	CompleteJob(context.Context, JobResultRequest) error
	WaitJob(context.Context, JobResultRequest) error
	RetryJob(context.Context, JobResultRequest) error
	FailJob(context.Context, JobResultRequest) error
}

// LeaseReleaser is an optional JobStore extension used on graceful
// shutdown: the claim holder ends its own live lease so the next
// reconciliation resumes the job at once. It never completes, retries or
// fails the job; that stays the reconciler's decision.
type LeaseReleaser interface {
	ReleaseJobLease(ctx context.Context, jobID int64, workspaceID, claimToken string, at time.Time) error
}

// leaseReleaseTimeout bounds the lease release write on shutdown.
const leaseReleaseTimeout = 2 * time.Second

// StepError is a sanitized executor failure. Retryable must be explicit;
// unknown errors fail closed so recovery cannot blindly repeat side effects.
type StepError struct {
	Code      ReasonCode
	Retryable bool
	Uncertain bool
}

func (e *StepError) Error() string {
	if e == nil {
		return "workflow: step failed"
	}
	return "workflow: step failed: " + string(e.Code)
}

// Registry holds the in-process implementations for persisted step kinds.
type Registry struct {
	mu        sync.RWMutex
	executors map[string]StepExecutor
}

func NewRegistry() *Registry { return &Registry{executors: make(map[string]StepExecutor)} }

// Register adds one kind. Replacing a registered kind is rejected so a live
// process cannot silently change the implementation used by pinned work.
func (r *Registry) Register(kind string, executor StepExecutor) error {
	if r == nil || kind == "" || executor == nil {
		return ErrInvalidInput
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.executors[kind]; exists {
		return ErrConflict
	}
	r.executors[kind] = executor
	return nil
}

func (r *Registry) executor(kind string) (StepExecutor, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	executor, ok := r.executors[kind]
	return executor, ok
}

// WorkerSettings tune only local polling and concurrency. Lease and timeout
// policy remains pinned in each execution definition.
type WorkerSettings struct {
	Workers      int
	PollInterval time.Duration
	// OnStepFailure, when set, is told of each step attempt that ends in a
	// retry (final false) or a failure (final true), with the normalized
	// reason code only (Phase 12 metrics). It must not block.
	OnStepFailure func(reason ReasonCode, final bool)
}

// Worker claims and executes durable jobs.
type Worker struct {
	store    JobStore
	registry *Registry
	clock    Clock
	settings WorkerSettings
}

func NewWorker(store JobStore, registry *Registry, clock Clock, settings WorkerSettings) *Worker {
	if clock == nil {
		clock = systemClock{}
	}
	if settings.Workers < 1 {
		settings.Workers = 1
	}
	if settings.PollInterval <= 0 {
		settings.PollInterval = time.Second
	}
	return &Worker{store: store, registry: registry, clock: clock, settings: settings}
}

// Run polls until ctx is cancelled. A per-job storage failure leaves the
// lease for reconciliation and does not stop independent executions.
func (w *Worker) Run(ctx context.Context) error { return w.RunUntil(ctx, ctx) }

// RunUntil claims new jobs until stop ends, and runs every claimed job with
// hard, so a job step already in flight when stop ends may finish (graceful
// shutdown drain). When hard ends too, an interrupted step releases its own
// lease (LeaseReleaser) and the next start resumes it. RunUntil returns
// after every in-flight step has returned.
func (w *Worker) RunUntil(stop, hard context.Context) error {
	if w == nil || w.store == nil || w.registry == nil {
		return ErrInvalidInput
	}
	var workers sync.WaitGroup
	for range w.settings.Workers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			w.loop(stop, hard)
		}()
	}
	workers.Wait()
	return nil
}

func (w *Worker) loop(stop, hard context.Context) {
	for stop.Err() == nil && hard.Err() == nil {
		worked, _ := w.ProcessNext(hard)
		if worked {
			continue
		}
		select {
		case <-stop.Done():
			return
		case <-hard.Done():
			return
		case <-w.clock.After(w.settings.PollInterval):
		}
	}
}

// ProcessNext claims and runs at most one due job.
func (w *Worker) ProcessNext(ctx context.Context) (bool, error) {
	if w == nil || w.store == nil || w.registry == nil || w.clock == nil {
		return false, ErrInvalidInput
	}
	now := w.clock.Now()
	job, err := w.store.ClaimJob(ctx, now)
	if err != nil || job == nil {
		return false, err
	}
	return true, w.execute(ctx, job)
}

func (w *Worker) execute(ctx context.Context, job *Job) error {
	ctx = obs.WithCorrelation(ctx, job.Execution.CorrelationID)
	ctx = obs.WithExecution(ctx, job.ExecutionID)
	started := w.clock.Now()
	logger := obs.Logger(ctx)
	logger.Info("workflow step started", "job_id", job.ID, "step_key", job.StepKey, "attempt", job.Attempts)
	executor, ok := w.registry.executor(job.Step.Kind)
	if !ok {
		return w.fail(ctx, job, ReasonNonRetryable, nil, Usage{})
	}
	now := w.clock.Now()
	if err := w.store.AuthorizeAction(ctx, job.ID, job.WorkspaceID, job.ClaimToken, now); err != nil {
		return err
	}
	timeout := job.Execution.Definition.StepTimeout
	deadlineFailure := false
	if job.Execution.Deadline != nil {
		remaining := job.Execution.Deadline.Sub(now)
		if remaining <= 0 {
			return w.fail(ctx, job, ReasonDeadlineExceeded, nil, Usage{})
		}
		if timeout <= 0 || remaining < timeout {
			timeout = remaining
			deadlineFailure = true
		}
	}
	if timeout <= 0 {
		return w.fail(ctx, job, ReasonNonRetryable, nil, Usage{})
	}
	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	timedOut := make(chan struct{})
	go func() {
		select {
		case <-execCtx.Done():
		case <-w.clock.After(timeout):
			close(timedOut)
			cancel()
		}
	}()
	result, executeErr := executor.Execute(execCtx, StepContext{
		Execution: job.Execution, Definition: job.Execution.Definition, Step: job.Step,
		ClaimToken: job.ClaimToken, JobID: job.ID, Attempt: job.Attempts, Checkpoint: job.Step.Checkpoint,
	})
	if executeErr != nil {
		// An executor's explicit uncertainty wins over the timer and over
		// shutdown: the side effect may have happened, so the step must never
		// be retried automatically, through a resume or a manual retry child.
		var stepErr *StepError
		isStepErr := errors.As(executeErr, &stepErr)
		if isStepErr && stepErr.Uncertain {
			if ctx.Err() != nil {
				failCtx, cancelFail := context.WithTimeout(context.WithoutCancel(ctx), leaseReleaseTimeout)
				defer cancelFail()
				if err := w.fail(failCtx, job, ReasonSideEffectUncertain, result.Checkpoint, result.Usage); err != nil {
					return err
				}
				return ctx.Err()
			}
			return w.fail(ctx, job, ReasonSideEffectUncertain, result.Checkpoint, result.Usage)
		}
		if ctx.Err() != nil {
			w.releaseLease(job)
			return ctx.Err()
		}
		select {
		case <-timedOut:
			if deadlineFailure {
				return w.fail(ctx, job, ReasonDeadlineExceeded, result.Checkpoint, result.Usage)
			}
			return w.retryOrFail(ctx, job, ReasonDeadlineExceeded, result.Checkpoint, result.Usage)
		default:
		}
		if isStepErr {
			code := normalizedReason(stepErr.Code, ReasonNonRetryable)
			if stepErr.Retryable {
				return w.retryOrFail(ctx, job, code, result.Checkpoint, result.Usage)
			}
			return w.fail(ctx, job, code, result.Checkpoint, result.Usage)
		}
		return w.fail(ctx, job, ReasonSideEffectUncertain, result.Checkpoint, result.Usage)
	}
	request := JobResultRequest{JobCheckpointRequest: JobCheckpointRequest{
		JobID: job.ID, WorkspaceID: job.WorkspaceID, ExecutionID: job.ExecutionID,
		StepKey: job.StepKey, ExpectedAttempt: job.Attempts, ClaimToken: job.ClaimToken,
		At: w.clock.Now(), Checkpoint: result.Checkpoint, Usage: result.Usage,
	}}
	if result.WaitState != "" {
		if !result.WaitState.IsWaiting() || len(result.WaitReferences) == 0 {
			return w.fail(ctx, job, ReasonNonRetryable, result.Checkpoint, result.Usage)
		}
		request.WaitState = result.WaitState
		request.WaitReferences = result.WaitReferences
		request.Reason = ReasonStepWaiting
		err := w.store.WaitJob(ctx, request)
		if err == nil {
			logger.Info("workflow step waiting", "job_id", job.ID, "step_key", job.StepKey, "attempt", job.Attempts, "state", result.WaitState, "duration_ms", w.clock.Now().Sub(started).Milliseconds())
		}
		return err
	}
	request.Reason = ReasonStepSucceeded
	err := w.store.CompleteJob(ctx, request)
	if err == nil {
		logger.Info("workflow step completed", "job_id", job.ID, "step_key", job.StepKey, "attempt", job.Attempts, "state", JobCompleted, "duration_ms", w.clock.Now().Sub(started).Milliseconds())
	}
	return err
}

// releaseLease ends the lease of a job whose step was cut off by shutdown,
// when the store supports it. A failure only means the lease expires on its
// own.
func (w *Worker) releaseLease(job *Job) {
	releaser, ok := w.store.(LeaseReleaser)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), leaseReleaseTimeout)
	defer cancel()
	_ = releaser.ReleaseJobLease(ctx, job.ID, job.WorkspaceID, job.ClaimToken, w.clock.Now())
}

func (w *Worker) retryOrFail(ctx context.Context, job *Job, reason ReasonCode, checkpoint *Checkpoint, usage Usage) error {
	if job.Attempts >= job.MaxAttempts {
		return w.fail(ctx, job, ReasonAttemptsExhausted, checkpoint, usage)
	}
	delays := job.Execution.Definition.Retry.Delays
	index := job.Attempts - 1
	if index < 0 || index >= len(delays) {
		return w.fail(ctx, job, ReasonAttemptsExhausted, checkpoint, usage)
	}
	now := w.clock.Now()
	if w.settings.OnStepFailure != nil {
		w.settings.OnStepFailure(normalizedReason(reason, ReasonStepRetry), false)
	}
	err := w.store.RetryJob(ctx, JobResultRequest{
		JobCheckpointRequest: JobCheckpointRequest{JobID: job.ID, WorkspaceID: job.WorkspaceID,
			ExecutionID: job.ExecutionID, StepKey: job.StepKey, ExpectedAttempt: job.Attempts,
			ClaimToken: job.ClaimToken, At: now, Checkpoint: checkpoint, Usage: usage},
		Reason: normalizedReason(reason, ReasonStepRetry), RetryAt: now.Add(delays[index]),
	})
	if err == nil {
		obs.Logger(ctx).Warn("workflow step retry scheduled", "job_id", job.ID, "step_key", job.StepKey, "attempt", job.Attempts, "reason", normalizedReason(reason, ReasonStepRetry))
	}
	return err
}

func (w *Worker) fail(ctx context.Context, job *Job, reason ReasonCode, checkpoint *Checkpoint, usage Usage) error {
	if w.settings.OnStepFailure != nil {
		w.settings.OnStepFailure(normalizedReason(reason, ReasonNonRetryable), true)
	}
	err := w.store.FailJob(ctx, JobResultRequest{JobCheckpointRequest: JobCheckpointRequest{
		JobID: job.ID, WorkspaceID: job.WorkspaceID, ExecutionID: job.ExecutionID,
		StepKey: job.StepKey, ExpectedAttempt: job.Attempts, ClaimToken: job.ClaimToken,
		At: w.clock.Now(), Checkpoint: checkpoint, Usage: usage,
	}, Reason: normalizedReason(reason, ReasonNonRetryable)})
	if err == nil {
		obs.Logger(ctx).Error("workflow step failed", "job_id", job.ID, "step_key", job.StepKey, "attempt", job.Attempts, "reason", normalizedReason(reason, ReasonNonRetryable))
	}
	return err
}

func normalizedReason(reason, fallback ReasonCode) ReasonCode {
	if reason == "" || len(reason) > 64 {
		return fallback
	}
	for i, r := range reason {
		if (r < 'a' || r > 'z') && (i == 0 || (r < '0' || r > '9') && r != '_') {
			return fallback
		}
	}
	return reason
}

func (j Job) String() string {
	return fmt.Sprintf("workflow job %d execution %d step %s", j.ID, j.ExecutionID, j.StepKey)
}
