package orchestrator

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Phase 7b B1: the DELEGATE worker path of a run slot (REQ-14, REQ-10).
// Case ids are B1-Wn.

// startWorker plans one repo.write task and runs plan; run_01 is left to
// the test.
func startWorker(t *testing.T, opts ...shOpt) *shH {
	h := newSH(t, opts...)
	h.plan("DELEGATE", shWrite("w", "Sửa validate"))
	h.must(StepKeyPlan)
	return h
}

func jobPayload(t *testing.T, h *shH) workerJobPayload {
	t.Helper()
	raw, ok := h.jobs.payload["run_01"]
	if !ok {
		t.Fatalf("no job created; created=%v", h.jobs.created)
	}
	var p workerJobPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// B1-W1 (REQ-14): ResolveRepo order inside the run slot: explicit repo
// (registered spelling), remembered repo of the ClickUp task, the only
// registered repo; an unregistered explicit repo never falls back.
func TestWorkerPath_RepoResolutionOrder(t *testing.T) {
	cases := []struct {
		name       string
		registered []string
		explicit   string
		clickup    string
		last       string
		wantRepo   string // "" means clarification
	}{
		{"explicit wins, registered spelling", []string{"org/alpha", "org/beta"}, "ORG/BETA", "CU-1", "org/alpha", "org/beta"},
		{"explicit unregistered does not fall back", []string{"org/alpha"}, "org/zzz", "", "", ""},
		{"remembered repo of the task", []string{"org/alpha", "org/beta"}, "", "CU-1", "org/beta", "org/beta"},
		{"remembered repo no longer registered", []string{"org/alpha", "org/beta"}, "", "CU-1", "org/gone", ""},
		{"remembered gone, one registered", []string{"org/alpha"}, "", "CU-1", "org/gone", "org/alpha"},
		{"only registered repo", []string{"org/alpha"}, "", "", "", "org/alpha"},
		{"two registered, nothing known", []string{"org/alpha", "org/beta"}, "", "", "", ""},
		{"none registered", nil, "", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := startWorker(t)
			h.repos.registered, h.repos.last = c.registered, c.last
			h.routes.route.Repo, h.routes.route.ClickUpTaskID = c.explicit, c.clickup
			res, err := h.step("run_01")
			if err != nil || res.WaitState != workflow.StateWaitingForUser {
				t.Fatalf("%q %v", res.WaitState, err)
			}
			if c.wantRepo == "" {
				if len(h.jobs.created) != 0 {
					t.Fatalf("job created without a repo: %v", h.jobs.created)
				}
				if d := h.plans.lastDecision(); d.Decision != DecisionRequestClarification || d.ReasonCode != ReasonRepoUnresolved || d.TaskKey != "w" {
					t.Fatalf("%+v", d)
				}
				return
			}
			if got := jobPayload(t, h).Repo; got != c.wantRepo {
				t.Fatalf("repo %q, want %q", got, c.wantRepo)
			}
		})
	}
}

// B1-W2 (REQ-14, REQ-9): without a repo the run slot asks which repo
// (naming WF-n and the registered repos), creates no job, and the owner's
// answer resumes it.
func TestWorkerPath_ClarificationOnNoRepo(t *testing.T) {
	h := startWorker(t)
	h.repos.registered = []string{"org/alpha", "org/beta"}
	res, err := h.step("run_01")
	if err != nil || res.WaitState != workflow.StateWaitingForUser {
		t.Fatalf("%q %v", res.WaitState, err)
	}
	shHas(t, h.owner.lastSaid(), "WF-1", "org/alpha, org/beta", "Sửa validate")
	if task := h.plans.task(1, "w"); task.State != TaskWaiting {
		t.Fatalf("task state %s", task.State)
	}
	if r, _ := h.step("run_01"); r.WaitState != workflow.StateWaitingForUser || h.owner.saidCount() != 2 || len(h.jobs.created) != 0 {
		t.Fatalf("no answer yet: wait=%q said=%d jobs=%v", r.WaitState, h.owner.saidCount(), h.jobs.created)
	}

	h.instr.add("WF-1 org/beta")
	res, err = h.step("run_01")
	if err != nil || res.WaitState != workflow.StateWaitingForUser {
		t.Fatalf("after answer: %q %v", res.WaitState, err)
	}
	p := jobPayload(t, h)
	if p.Repo != "org/beta" || p.Title != "Sửa validate" || p.InstructionVersion != 2 || p.TaskID != "WF-1" {
		t.Fatalf("%+v", p)
	}
	shHas(t, p.Instruction, "kiểm tra việc này", "WF-1 org/beta", "Việc w trong kế hoạch WF-1")
	if got := h.jobs.caps["run_01"]; len(got) != 1 || got[0] != CapRepoWrite {
		t.Fatalf("caps %v", got)
	}
}

// B1-W2b: an answer naming a repo that is not registered asks again.
func TestWorkerPath_UnregisteredAnswerAsksAgain(t *testing.T) {
	h := startWorker(t)
	h.repos.registered = []string{"org/alpha", "org/beta"}
	h.step("run_01")
	h.instr.add("org/zzz")
	res, err := h.step("run_01")
	if err != nil || res.WaitState != workflow.StateWaitingForUser || len(h.jobs.created) != 0 {
		t.Fatalf("%q %v jobs=%v", res.WaitState, err, h.jobs.created)
	}
	if h.owner.saidCount() != 3 {
		t.Fatalf("said %d, want plan + two questions", h.owner.saidCount())
	}
}

// B1-W3 (REQ-14): job display id and ClickUp id reach the payload; the
// payload carries ids and text only.
func TestWorkerPath_PayloadIdentifiers(t *testing.T) {
	h := startWorker(t)
	h.routes.route.TaskRef, h.routes.route.ClickUpTaskID = "FC-6583", "CU-9"
	h.repos.last = ""
	if _, err := h.step("run_01"); err != nil {
		t.Fatal(err)
	}
	p := jobPayload(t, h)
	if p.TaskID != "FC-6583" || p.ClickUpTaskID != "CU-9" || p.Repo != "org/alpha" {
		t.Fatalf("%+v", p)
	}
}

// B1-W4 (REQ-14): a finished job is read, never created twice.
func TestWorkerPath_JobCreatedOnce(t *testing.T) {
	h := startWorker(t)
	for i := 0; i < 3; i++ {
		if res, err := h.step("run_01"); err != nil || res.WaitState != workflow.StateWaitingForUser {
			t.Fatalf("%d: %q %v", i, res.WaitState, err)
		}
	}
	if len(h.jobs.created) != 1 {
		t.Fatalf("jobs created %v", h.jobs.created)
	}
}

// B1-W5 (REQ-14): job FAILED, NEEDS_INPUT and CANCELLED end the task as
// FAILED with a fixed reason; the worker's own reason code is kept only
// when it is a safe code.
func TestWorkerPath_JobFailureEndsTask(t *testing.T) {
	cases := []struct {
		name, state, reason, want string
	}{
		{"failed with code", JobFailed, "tests_failed", "tests_failed"},
		{"needs input", JobNeedsInput, "", ReasonWorkerFailed},
		{"cancelled", JobCancelled, "tests_failed", ReasonWorkerCancelled},
		{"unsafe reason code", JobFailed, "Bad Reason!", ReasonWorkerFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := startWorker(t)
			h.step("run_01")
			h.jobs.set("run_01", func(j *WorkerJob) { j.State, j.ReasonCode = c.state, c.reason })
			res, err := h.step("run_01")
			if err != nil || res.WaitState != "" {
				t.Fatalf("%q %v", res.WaitState, err)
			}
			task := h.plans.task(1, "w")
			var result map[string]string
			_ = json.Unmarshal(task.Result, &result)
			if task.State != TaskFailed || result["outcome"] != "failed" || result["reason_code"] != c.want {
				t.Fatalf("%+v %v", task, result)
			}
			if len(task.EvidenceRefs) == 0 || task.EvidenceRefs[0] != "wf1:job:job-1" {
				t.Fatalf("evidence %v", task.EvidenceRefs)
			}
		})
	}
}

// B1-W6 (REQ-14, REQ-7): a succeeded job gives the task evidence: the job
// ref and, for an https PR link only, the PR URL.
func TestWorkerPath_SucceededJobEvidence(t *testing.T) {
	for _, c := range []struct {
		pr       string
		wantLink bool
	}{{"https://github.com/o/r/pull/3", true}, {"http://insecure/pull/3", false}, {"javascript:alert(1)", false}} {
		h := startWorker(t)
		h.step("run_01")
		h.jobs.set("run_01", func(j *WorkerJob) { j.State, j.Branch, j.PRURL, j.TestOutcome = JobSucceeded, "feat/x", c.pr, "passed" })
		if res, err := h.step("run_01"); err != nil || res.WaitState != "" {
			t.Fatalf("%q %v", res.WaitState, err)
		}
		task := h.plans.task(1, "w")
		if task.State != TaskSucceeded {
			t.Fatalf("state %s", task.State)
		}
		has := false
		for _, e := range task.EvidenceRefs {
			has = has || e == c.pr
		}
		if has != c.wantLink || task.EvidenceRefs[0] != "wf1:job:job-1" {
			t.Fatalf("pr %q evidence %v", c.pr, task.EvidenceRefs)
		}
		var result map[string]string
		_ = json.Unmarshal(task.Result, &result)
		if result["job_id"] != "job-1" || result["branch"] != "feat/x" || result["test_outcome"] != "passed" {
			t.Fatalf("%v", result)
		}
	}
}

// B1-W7 (REQ-10, human): while the worker is offline or busy the slot waits
// without any model call or token charge; the wait counts against wall time.
func TestWorkerPath_WaitingDoesNotChargeTokens(t *testing.T) {
	h := startWorker(t)
	before := shBudget(t, workflow.StepResult{Checkpoint: h.steps.steps[StepKeyPlan].Checkpoint})
	calls := len(h.rt.Requests())
	for _, state := range []string{JobWaiting, JobLeased, JobRunning, JobInterrupted} {
		if len(h.jobs.created) > 0 {
			h.jobs.set("run_01", func(j *WorkerJob) { j.State = state })
		}
		res, err := h.step("run_01")
		if err != nil || res.WaitState != workflow.StateWaitingForUser {
			t.Fatalf("%s: %q %v", state, res.WaitState, err)
		}
		if res.Usage != (workflow.Usage{}) {
			t.Fatalf("%s: usage %+v", state, res.Usage)
		}
		if got := shBudget(t, res); got.Used != before.Used {
			t.Fatalf("%s: counters moved %+v -> %+v", state, before.Used, got.Used)
		}
		if refValue(res.Checkpoint, CheckpointWait) != "job:job-1" || refValue(res.Checkpoint, CheckpointWorkerJob) != "job-1" {
			t.Fatalf("%s: refs %+v", state, res.Checkpoint.References)
		}
	}
	if len(h.rt.Requests()) != calls {
		t.Fatal("model called while waiting for the worker")
	}
	if task := h.plans.task(1, "w"); task.State != TaskWaiting {
		t.Fatalf("task state %s", task.State)
	}
}

// B1-W8 (REQ-10): the wall limit still applies to a worker wait, and before a
// job is created.
func TestWorkerPath_WallLimitGatesTheWait(t *testing.T) {
	t.Run("while waiting", func(t *testing.T) {
		h := startWorker(t)
		h.step("run_01")
		h.now = t0.Add(DefaultMaxWall + time.Minute)
		res, err := h.step("run_01")
		if err != nil || res.WaitState != workflow.StateWaitingForUser {
			t.Fatalf("%q %v", res.WaitState, err)
		}
		if len(h.owner.asked) != 1 || h.owner.asked[0].Purpose != "budget_wall_time" {
			t.Fatalf("%+v", h.owner.asked)
		}
	})
	t.Run("before the job exists", func(t *testing.T) {
		h := startWorker(t)
		h.now = t0.Add(DefaultMaxWall + time.Minute)
		if _, err := h.step("run_01"); err != nil {
			t.Fatal(err)
		}
		if len(h.jobs.created) != 0 || len(h.owner.asked) != 1 || h.owner.asked[0].Purpose != "budget_wall_time" {
			t.Fatalf("jobs=%v asked=%+v", h.jobs.created, h.owner.asked)
		}
	})
}

// B1-W9 (REQ-10): the worker result arrives after the wall gate was
// approved: the wait continues, the job is not duplicated.
func TestWorkerPath_WallExtensionResumesWait(t *testing.T) {
	h := startWorker(t)
	h.step("run_01")
	h.now = t0.Add(DefaultMaxWall + time.Minute)
	h.step("run_01")
	h.owner.decide(GateApproved)
	res, err := h.step("run_01")
	if err != nil || res.WaitState != workflow.StateWaitingForUser {
		t.Fatalf("%q %v", res.WaitState, err)
	}
	if len(h.jobs.created) != 1 || shBudget(t, res).Extensions != 1 {
		t.Fatalf("jobs=%v ext=%d", h.jobs.created, shBudget(t, res).Extensions)
	}
	h.jobs.set("run_01", func(j *WorkerJob) { j.State, j.Branch = JobSucceeded, "feat/x" })
	if res, err := h.step("run_01"); err != nil || res.WaitState != "" {
		t.Fatalf("%q %v", res.WaitState, err)
	}
	if h.plans.task(1, "w").State != TaskSucceeded {
		t.Fatal("task not succeeded")
	}
}

// B1-W10 (REQ-5): a worker or executor that is not wired fails the task with
// executor_unavailable instead of hanging.
func TestWorkerPath_UnwiredExecutorFailsTask(t *testing.T) {
	t.Run("no worker jobs", func(t *testing.T) {
		h := startWorker(t)
		h.s.Jobs = nil
		if res, err := h.step("run_01"); err != nil || res.WaitState != "" {
			t.Fatalf("%q %v", res.WaitState, err)
		}
		if task := h.plans.task(1, "w"); task.State != TaskFailed || !strings.Contains(string(task.Result), ReasonExecutorUnavailable) {
			t.Fatalf("%+v", task)
		}
	})
	t.Run("no in-process executor", func(t *testing.T) {
		h := newSH(t)
		h.plan("EXECUTE", shRead("a", "First"))
		h.must(StepKeyPlan)
		delete(h.s.Executors, ExecutorInvestigate)
		if res, err := h.step("run_01"); err != nil || res.WaitState != "" {
			t.Fatalf("%q %v", res.WaitState, err)
		}
		if task := h.plans.task(1, "a"); task.State != TaskFailed || !strings.Contains(string(task.Result), ReasonExecutorUnavailable) {
			t.Fatalf("%+v", task)
		}
	})
}

// B1-W11 (REQ-5, REQ-10): an in-process task is not started when the token
// budget cannot cover it; the owner is asked, and the executor sees what
// the budget has left.
func TestRunTask_ExecutorBudget(t *testing.T) {
	h := newSH(t, shSettings(func(s *StepSettings) { s.Caps.Tokens = 5000 }))
	h.plan("EXECUTE", shRead("a", "First"))
	h.must(StepKeyPlan) // 60 tokens used
	var left int64
	h.inv.fn = func(r TaskRun) (TaskOutcome, error) {
		left = r.TokensLeft
		return TaskOutcome{Succeeded: true, Usage: Usage{Tokens: domain.Usage{Input: 300}}}, nil
	}
	res := h.must("run_01")
	if left != 4940 {
		t.Fatalf("tokens left %d", left)
	}
	if snap := shBudget(t, res); snap.Used.Tokens != 360 {
		t.Fatalf("executor usage not charged: %d", snap.Used.Tokens)
	}

	h = newSH(t, shSettings(func(s *StepSettings) { s.Caps.Tokens, s.PlanEstimate = 3000, 100 }))
	h.plan("EXECUTE", shRead("a", "First"))
	h.must(StepKeyPlan)
	res, err := h.step("run_01") // ExecuteEstimate 4000 does not fit
	if err != nil || res.WaitState != workflow.StateWaitingForUser || len(h.inv.keys()) != 0 {
		t.Fatalf("%q %v runs=%v", res.WaitState, err, h.inv.keys())
	}
}

// B1-W12: usage of a failing executor is still charged and the task fails
// with a fixed reason; the executor error text is never stored.
func TestRunTask_ExecutorErrorChargedAndSanitized(t *testing.T) {
	h := newSH(t)
	h.plan("EXECUTE", shRead("a", "First"))
	h.must(StepKeyPlan)
	h.inv.fn = func(TaskRun) (TaskOutcome, error) {
		return TaskOutcome{Usage: Usage{Tokens: domain.Usage{Input: 200}}}, errShDown
	}
	res := h.must("run_01")
	if snap := shBudget(t, res); snap.Used.Tokens != 260 {
		t.Fatalf("tokens %d", snap.Used.Tokens)
	}
	task := h.plans.task(1, "a")
	if task.State != TaskFailed || strings.Contains(string(task.Result), errShDown.Error()) || !strings.Contains(string(task.Result), "executor_failed") {
		t.Fatalf("%+v", task)
	}
}
