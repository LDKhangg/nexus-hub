package orchestrator

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Phase 10 Batch A, K1 (REQ-10, REQ-11, REQ-12): the worker wait state, the
// owner's Retry/Cancel wait, the durable wake time, executor waits and
// the clickup.write offer. Case ids start with P10-K.

// P10-K1a (REQ-10): clickup.write is offered only when the execution works on a ClickUp task.
func TestP10Plan_ClickUpWriteOfferedOnlyWithClickUpTask(t *testing.T) {
	for _, c := range []struct {
		name  string
		task  string
		offer bool
	}{{"with a ClickUp task", "86abc", true}, {"without a ClickUp task", "", false}} {
		t.Run(c.name, func(t *testing.T) {
			h := newSH(t)
			h.s.Executors[ExecutorBehaviorActions] = h.inv
			h.routes.route.ClickUpTaskID = c.task
			h.plan("EXECUTE", shRead("a", "Đọc task"))
			h.must(StepKeyPlan)
			var prompt string
			for _, r := range h.rt.Requests() {
				if r.SchemaName == "plan" {
					prompt = r.Prompt
				}
			}
			if prompt == "" {
				t.Fatal("no plan request")
			}
			if got := strings.Contains(prompt, CapClickUpWrite); got != c.offer {
				t.Fatalf("clickup.write offered=%v want %v", got, c.offer)
			}
			if !strings.Contains(prompt, CapTelegramNotify) {
				t.Fatal("telegram.notify must stay offered")
			}
		})
	}
}

// P10-K1b (REQ-10, abuse): a plan that uses clickup.write without a ClickUp task is rejected, not run.
func TestP10Plan_ClickUpWriteTaskWithoutClickUpTaskIsRejected(t *testing.T) {
	h := newSH(t)
	h.s.Executors[ExecutorBehaviorActions] = h.inv
	h.routes.route.ClickUpTaskID = ""
	h.plan("EXECUTE", ptask("w", "Đăng comment", []string{CapClickUpWrite}))
	_, _ = h.step(StepKeyPlan)
	if len(h.inv.calls) != 0 {
		t.Fatal("an executor ran for an unoffered capability")
	}
}

// p10Resumer is an in-process executor whose task waits for an owner decision.
type p10Resumer struct {
	mu       sync.Mutex
	runs     int
	resumes  []string
	waitRef  string
	outcome  TaskOutcome
	resumeFn func(waitRef string) TaskOutcome
}

func (e *p10Resumer) RunTask(_ context.Context, _ TaskRun) (TaskOutcome, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.runs++
	return TaskOutcome{Wait: e.waitRef}, nil
}

func (e *p10Resumer) ResumeTask(_ context.Context, _ TaskRun, waitRef string) (TaskOutcome, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resumes = append(e.resumes, waitRef)
	if e.resumeFn != nil {
		return e.resumeFn(waitRef), nil
	}
	return e.outcome, nil
}

func p10WriteSetup(t *testing.T, ex TaskExecutor) *shH {
	t.Helper()
	h := newSH(t)
	h.s.Executors[ExecutorBehaviorActions] = ex
	h.routes.route.ClickUpTaskID = "86abc"
	h.plan("EXECUTE", ptask("w", "Đăng kết quả lên ClickUp", []string{CapClickUpWrite}))
	h.must(StepKeyPlan)
	return h
}

// P10-K1c (REQ-10): a clickup.write task waits for the draft decision; posted gives success with evidence.
func TestP10Executor_WaitThenPostedSucceedsWithEvidence(t *testing.T) {
	ex := &p10Resumer{waitRef: "draft-5", outcome: TaskOutcome{Succeeded: true, Evidence: []EvidenceRef{{Kind: EvidenceArtifact, ID: "https://app.clickup.com/t/86abc"}}}}
	h := p10WriteSetup(t, ex)
	res, err := h.step("run_01")
	if err != nil || res.WaitState != workflow.StateWaitingForApproval {
		t.Fatalf("wait %q err %v", res.WaitState, err)
	}
	if refValue(res.Checkpoint, CheckpointWait) != "task:draft-5" {
		t.Fatalf("wait marker %q", refValue(res.Checkpoint, CheckpointWait))
	}
	if task := h.plans.task(1, "w"); task.State != TaskWaiting {
		t.Fatalf("task %s, want WAITING (not finished before the owner decides)", task.State)
	}
	res, err = h.step("run_01")
	if err != nil || res.WaitState != "" {
		t.Fatalf("after the decision: wait %q err %v", res.WaitState, err)
	}
	task := h.plans.task(1, "w")
	found := false
	for _, e := range task.EvidenceRefs {
		found = found || e == "https://app.clickup.com/t/86abc"
	}
	if task.State != TaskSucceeded || !found {
		t.Fatalf("task %+v", task)
	}
	if ex.runs != 1 || len(ex.resumes) != 1 || ex.resumes[0] != "draft-5" {
		t.Fatalf("runs %d resumes %v", ex.runs, ex.resumes)
	}
}

// P10-K1d (REQ-10): Huỷ or an expired draft blocks the task; it does not fail it.
func TestP10Executor_CancelledOrExpiredBlocksTask(t *testing.T) {
	for _, reason := range []string{"draft_cancelled", "draft_expired"} {
		t.Run(reason, func(t *testing.T) {
			ex := &p10Resumer{waitRef: "draft-5", outcome: TaskOutcome{Blocked: true, ReasonCode: reason}}
			h := p10WriteSetup(t, ex)
			h.step("run_01")
			res, err := h.step("run_01")
			if err != nil || res.WaitState != "" {
				t.Fatalf("wait %q err %v", res.WaitState, err)
			}
			task := h.plans.task(1, "w")
			var result map[string]string
			_ = json.Unmarshal(task.Result, &result)
			if task.State != TaskBlocked || result["outcome"] != "blocked" || result["reason_code"] != reason {
				t.Fatalf("task %s result %v", task.State, result)
			}
		})
	}
}

// P10-K1e (REQ-10, human): a step retried after a crash resumes the task; it never runs the executor again.
func TestP10Executor_RetryAfterCrashResumesWithoutSecondRun(t *testing.T) {
	ex := &p10Resumer{waitRef: "draft-9", outcome: TaskOutcome{Succeeded: true}}
	h := p10WriteSetup(t, ex)
	h.step("run_01")
	// The checkpoint with the wait marker was lost: only the task row says it started.
	h.steps.mu.Lock()
	st := h.steps.steps["run_01"]
	st.Checkpoint = nil
	h.steps.steps["run_01"] = st
	h.steps.mu.Unlock()
	if _, err := h.step("run_01"); err != nil {
		t.Fatal(err)
	}
	if ex.runs != 1 {
		t.Fatalf("executor ran %d times", ex.runs)
	}
	if len(ex.resumes) != 1 || ex.resumes[0] != "" {
		t.Fatalf("resume refs %v, want one resume with no ref", ex.resumes)
	}
	// An executor that cannot resume still fails an interrupted task, never repeats it.
	h2 := newSH(t)
	h2.s.Executors[ExecutorBehaviorActions] = h2.inv
	h2.routes.route.ClickUpTaskID = "86abc"
	h2.plan("EXECUTE", ptask("w", "x", []string{CapClickUpWrite}))
	h2.must(StepKeyPlan)
	h2.inv.fn = func(TaskRun) (TaskOutcome, error) { return TaskOutcome{Wait: "ref"}, nil }
	h2.must("run_01") // a non-resumer's wait is refused: executor_failed, task failed
	if task := h2.plans.task(1, "w"); task.State != TaskFailed || !strings.Contains(string(task.Result), ReasonExecutorFailed) {
		t.Fatalf("task %+v", task)
	}
}

// P10-K1f (REQ-10, abuse): a wait reference with spaces or over 128 bytes is refused.
func TestP10Executor_InvalidWaitRefFailsTask(t *testing.T) {
	for name, ref := range map[string]string{"space": "draft 5", "newline": "a\nb", "too long": strings.Repeat("x", 129)} {
		t.Run(name, func(t *testing.T) {
			ex := &p10Resumer{waitRef: ref}
			h := p10WriteSetup(t, ex)
			res, err := h.step("run_01")
			if err != nil || res.WaitState != "" {
				t.Fatalf("wait %q err %v", res.WaitState, err)
			}
			if task := h.plans.task(1, "w"); task.State != TaskFailed {
				t.Fatalf("task %s", task.State)
			}
		})
	}
}

// P10-K2a (REQ-11): the worker wait carries a durable wake time at the end of the wall-time budget.
func TestP10Worker_WaitHasWakeTimeAtWallBudgetEnd(t *testing.T) {
	h := startWorker(t)
	res, err := h.step("run_01")
	if err != nil || res.WaitState != workflow.StateWaitingForWorker {
		t.Fatalf("%q %v", res.WaitState, err)
	}
	snap := shBudget(t, res)
	want := snap.StartedAt.Add(snap.Caps.Wall).Unix()
	got, perr := strconv.ParseInt(refValue(res.Checkpoint, CheckpointWakeAt), 10, 64)
	if perr != nil || got < want || got > want+1 {
		t.Fatalf("wake at %q, want %d (+1 s at most)", refValue(res.Checkpoint, CheckpointWakeAt), want)
	}
	if got > t0.Add(DefaultMaxWall).Unix()+1 {
		t.Fatalf("wake time %d is past the wall budget", got)
	}
	// The owner wait of a failed job has the same timer.
	h.jobs.set("run_01", func(j *WorkerJob) { j.State = JobFailed })
	res, err = h.step("run_01")
	if err != nil || res.WaitState != workflow.StateWaitingForUser || refValue(res.Checkpoint, CheckpointWakeAt) == "" {
		t.Fatalf("owner wait: %q %v wake=%q", res.WaitState, err, refValue(res.Checkpoint, CheckpointWakeAt))
	}
}

// P10-K2b (REQ-11, K1 ruling 2): FAILED, NEEDS_INPUT and INTERRUPTED jobs wait for the
// owner's Retry or Cancel; the task is not ended and no second job is created.
func TestP10Worker_FailedJobWaitsForOwner(t *testing.T) {
	for _, state := range []string{JobFailed, JobNeedsInput, JobInterrupted} {
		t.Run(state, func(t *testing.T) {
			h := startWorker(t)
			h.step("run_01")
			h.jobs.set("run_01", func(j *WorkerJob) { j.State, j.ReasonCode = state, "tests_failed" })
			res, err := h.step("run_01")
			if err != nil || res.WaitState != workflow.StateWaitingForUser {
				t.Fatalf("%q %v", res.WaitState, err)
			}
			if refValue(res.Checkpoint, CheckpointWait) != "job_owner:job-1" {
				t.Fatalf("wait marker %q", refValue(res.Checkpoint, CheckpointWait))
			}
			if task := h.plans.task(1, "w"); task.State != TaskWaiting {
				t.Fatalf("task %s, want WAITING", task.State)
			}
			if res.Usage != (workflow.Usage{}) || len(h.jobs.created) != 1 {
				t.Fatalf("usage %+v jobs %v", res.Usage, h.jobs.created)
			}
			// Retry: the same job is waiting again, the step follows it in WAITING_FOR_WORKER.
			h.jobs.set("run_01", func(j *WorkerJob) { j.State = JobWaiting })
			res, err = h.step("run_01")
			if err != nil || res.WaitState != workflow.StateWaitingForWorker || refValue(res.Checkpoint, CheckpointWait) != "job:job-1" {
				t.Fatalf("after retry: %q %q %v", res.WaitState, refValue(res.Checkpoint, CheckpointWait), err)
			}
			h.jobs.set("run_01", func(j *WorkerJob) { j.State, j.Branch = JobSucceeded, "feat/x" })
			if res, err = h.step("run_01"); err != nil || res.WaitState != "" || h.plans.task(1, "w").State != TaskSucceeded {
				t.Fatalf("after success: %q %v %s", res.WaitState, err, h.plans.task(1, "w").State)
			}
			if len(h.jobs.created) != 1 {
				t.Fatalf("jobs %v", h.jobs.created)
			}
		})
	}
}

// P10-K2b (REQ-11): Cancel after a failure ends the task as worker_cancelled and Evaluate decides.
func TestP10Worker_CancelAfterFailureEndsTask(t *testing.T) {
	h := startWorker(t)
	h.step("run_01")
	h.jobs.set("run_01", func(j *WorkerJob) { j.State = JobFailed })
	h.step("run_01")
	h.jobs.set("run_01", func(j *WorkerJob) { j.State = JobCancelled })
	if res, err := h.step("run_01"); err != nil || res.WaitState != "" {
		t.Fatalf("%q %v", res.WaitState, err)
	}
	task := h.plans.task(1, "w")
	if task.State != TaskFailed || !strings.Contains(string(task.Result), ReasonWorkerCancelled) {
		t.Fatalf("task %+v", task)
	}
}

// P10-K2c (REQ-11): once the wall-time budget is spent, a woken worker wait takes the O4 gate
// (the owner is asked), for a running job and for one waiting for Retry/Cancel; it never waits on.
func TestP10Worker_WallTimeWakeTakesO4(t *testing.T) {
	for _, state := range []string{JobRunning, JobFailed} {
		t.Run(state, func(t *testing.T) {
			h := startWorker(t)
			h.step("run_01")
			h.jobs.set("run_01", func(j *WorkerJob) { j.State = state })
			h.step("run_01")
			h.now = t0.Add(DefaultMaxWall + time.Second)
			res, err := h.step("run_01")
			if err != nil {
				t.Fatal(err)
			}
			if res.WaitState == workflow.StateWaitingForWorker {
				t.Fatal("still waiting for the worker past the wall-time budget")
			}
			if len(h.owner.asked) != 1 || h.owner.asked[0].Purpose != "budget_wall_time" {
				t.Fatalf("asked %+v", h.owner.asked)
			}
			// Huỷ at the gate ends the execution, it does not hold it until the 24 h deadline.
			h.owner.decide(GateRejected)
			if res, err = h.step("run_01"); err == nil && res.WaitState == workflow.StateWaitingForWorker {
				t.Fatalf("rejected gate left the step waiting for the worker: %q", res.WaitState)
			}
		})
	}
}

// P10-K2d (REQ-11, human): a restarted hub reads the same job again; the wait is rebuilt, no job is duplicated.
func TestP10Worker_RestartRebuildsWaitWithoutNewJob(t *testing.T) {
	h := startWorker(t)
	first, _ := h.step("run_01")
	// A new attempt after a restart starts from the stored checkpoint.
	res, err := h.stepAttempt("run_01", 2)
	if err != nil || res.WaitState != workflow.StateWaitingForWorker || len(h.jobs.created) != 1 {
		t.Fatalf("%q %v jobs %v", res.WaitState, err, h.jobs.created)
	}
	if refValue(res.Checkpoint, CheckpointWakeAt) != refValue(first.Checkpoint, CheckpointWakeAt) {
		t.Fatal("the wake time moved after a restart; the wall budget must not restart")
	}
}

// p10Routes is a WorkerJobs that also implements WorkerJobRoutes.
type p10JobRoutes struct {
	*shJobs
	prepared []string
	err      error
}

func (j *p10JobRoutes) PrepareJob(_ context.Context, _ Route, stepKey, repo string, _ int64) error {
	j.prepared = append(j.prepared, stepKey+"|"+repo)
	return j.err
}

// P10-K3a (REQ-12): the job's topic route is prepared right before the job is created; a failure still lets it run.
func TestP10Worker_JobRoutePreparedBeforeJob(t *testing.T) {
	for _, fail := range []bool{false, true} {
		h := newSH(t)
		jr := &p10JobRoutes{shJobs: h.jobs}
		if fail {
			jr.err = context.DeadlineExceeded
		}
		h.s.Jobs = jr
		h.plan("DELEGATE", shWrite("w", "Sửa validate"))
		h.must(StepKeyPlan)
		res, err := h.step("run_01")
		if err != nil || res.WaitState != workflow.StateWaitingForWorker {
			t.Fatalf("fail=%v: %q %v", fail, res.WaitState, err)
		}
		if len(jr.prepared) != 1 || jr.prepared[0] != "run_01|org/alpha" || len(h.jobs.created) != 1 {
			t.Fatalf("fail=%v: prepared %v created %v", fail, jr.prepared, h.jobs.created)
		}
		// A second pass reads the job; it prepares no second route.
		h.step("run_01")
		if len(jr.prepared) != 1 {
			t.Fatalf("route prepared again: %v", jr.prepared)
		}
	}
}
