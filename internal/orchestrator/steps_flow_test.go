package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Phase 7b B1: the plan -> run -> evaluate flow of the orchestrated_task
// steps (internal/orchestrator/steps.go, steps_flow.go). Case ids are B1-Fn.

// B1-F1 (REQ-6, REQ-8): ready tasks run in dependency order, whatever the
// order of the plan; COMPLETE ends the execution; the report is sent.
func TestStepsFlow_DependencyOrderThenComplete(t *testing.T) {
	h := newSH(t)
	h.plan("EXECUTE", shRead("b", "Second", "a"), shRead("a", "First"))
	h.eval("EXECUTE", "next")
	h.eval("COMPLETE", "done")

	key, _, err := h.flow()
	if key != "" || err != nil {
		t.Fatalf("flow stopped at %q: %v", key, err)
	}
	if got := h.inv.keys(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("run order %v", got)
	}
	if got := strings.Join(h.plans.decisionNames(), ","); got != "EXECUTE,EXECUTE,COMPLETE" {
		t.Fatalf("decisions %s", got)
	}
	if h.plans.planStatus(1) != PlanCompleted {
		t.Fatalf("plan status %s", h.plans.planStatus(1))
	}
	if h.calls("plan") != 1 || h.calls("evaluate") != 2 {
		t.Fatalf("model calls plan=%d evaluate=%d", h.calls("plan"), h.calls("evaluate"))
	}
	shHas(t, h.owner.lastSaid(), "WF-1: hoàn tất", "Quyết định: EXECUTE ×2, COMPLETE ×1")
}

// B1-F2 (REQ-6, abuse): a retried or woken step never repeats a model call:
// the decision row of the step is applied again.
func TestStepsFlow_RetryDoesNotRepeatModelCalls(t *testing.T) {
	h := newSH(t)
	h.plan("EXECUTE", shRead("a", "First"), shRead("b", "Second"))
	h.eval("EXECUTE", "next")
	h.must(StepKeyPlan)
	h.must("run_01")
	h.must("eval_01")
	rows := len(h.plans.decisionNames())

	h.must("eval_01")
	h.must(StepKeyPlan)
	if h.calls("evaluate") != 1 || h.calls("plan") != 1 {
		t.Fatalf("model calls plan=%d evaluate=%d", h.calls("plan"), h.calls("evaluate"))
	}
	if len(h.plans.decisionNames()) != rows {
		t.Fatalf("decision rows %d -> %d", rows, len(h.plans.decisionNames()))
	}
	if h.rt.Remaining("evaluate") != 0 {
		t.Fatal("unexpected script left")
	}
}

// B1-F2b: a storage failure while recording a decision is a retryable step
// error, never a silent success.
func TestStepsFlow_DecisionStorageFailureRetryable(t *testing.T) {
	h := newSH(t)
	h.plan("EXECUTE", shRead("a", "First"))
	h.plans.failDec = true
	_, err := h.step(StepKeyPlan)
	code, retry := shStepErrCode(t, err)
	if code != ReasonStorageUnavailable || !retry {
		t.Fatalf("%s retry=%v", code, retry)
	}
}

// B1-F3 (REQ-9, REQ-10): the 30 slots bound the runs; a plan still unfinished
// after them FAILs with task_slots_exhausted, and the owner gets the report
// before the execution fails.
func TestStepsFlow_SlotExhaustionFailsWithReport(t *testing.T) {
	h := newSH(t)
	var tasks []map[string]any
	for i := 1; i <= OrchestratedTaskSlots+1; i++ {
		tasks = append(tasks, shRead(fmt.Sprintf("t%02d", i), "Task"))
	}
	h.plan("EXECUTE", tasks...)
	for i := 0; i < OrchestratedTaskSlots; i++ {
		h.eval("EXECUTE", "next")
	}
	key, _, err := h.flow()
	if key != StepKeyReport {
		t.Fatalf("flow stopped at %q: %v", key, err)
	}
	code, retry := shStepErrCode(t, err)
	if code != ReasonTaskSlotsExhausted || retry {
		t.Fatalf("%s retry=%v", code, retry)
	}
	if n := len(h.inv.keys()); n != OrchestratedTaskSlots {
		t.Fatalf("%d tasks ran", n)
	}
	if d := h.plans.lastDecision(); d.Decision != DecisionFail || d.ReasonCode != ReasonTaskSlotsExhausted {
		t.Fatalf("%+v", d)
	}
	shHas(t, h.owner.lastSaid(), "WF-1: thất bại (task_slots_exhausted)")
	if h.plans.planStatus(1) != PlanFailed {
		t.Fatalf("plan status %s", h.plans.planStatus(1))
	}
}

// B1-F4 (REQ-6, human): the report is always sent before FAIL, and the step
// error code is limited to the fixed list (a model's own reason becomes
// orchestrator_failed).
func TestStepsFlow_FailReasonsLimitedToFixedList(t *testing.T) {
	cases := []struct{ reason, want string }{
		{ReasonMissingEvidence, ReasonMissingEvidence},
		{ReasonNoExecutor, ReasonNoExecutor},
		{ReasonNoRunnableTask, ReasonNoRunnableTask},
		{ReasonTaskSlotsExhausted, ReasonTaskSlotsExhausted},
		{ReasonApprovalRejected, ReasonApprovalRejected},
		{ReasonApprovalExpired, ReasonApprovalExpired},
		{ReasonPlanningBudgetExceeded, ReasonPlanningBudgetExceeded},
		{ReasonInvalidOutput, ReasonInvalidOutput},
		{ReasonLLMError, ReasonLLMError},
		{"model_made_this_up", ReasonOrchestratorFailed},
		{ReasonWorkerFailed, ReasonOrchestratorFailed},
	}
	for _, c := range cases {
		t.Run(c.reason, func(t *testing.T) {
			h := newSH(t)
			h.plan("EXECUTE", shRead("a", "First"))
			h.eval("FAIL", c.reason)
			key, _, err := h.flow()
			if key != StepKeyReport {
				t.Fatalf("stopped at %q: %v", key, err)
			}
			code, retry := shStepErrCode(t, err)
			if string(code) != c.want || retry {
				t.Fatalf("code %s retry=%v, want %s", code, retry, c.want)
			}
			shHas(t, h.owner.lastSaid(), "WF-1: thất bại (")
			if h.plans.planStatus(1) != PlanFailed {
				t.Fatalf("plan status %s", h.plans.planStatus(1))
			}
		})
	}
	known := map[workflow.ReasonCode]bool{}
	for _, r := range KnownWorkflowReasons() {
		known[r] = true
	}
	for _, c := range cases[:9] {
		if !known[workflow.ReasonCode(c.want)] {
			t.Errorf("%s is not in KnownWorkflowReasons", c.want)
		}
	}
}

// B1-F5 (REQ-10): every step stores the budget snapshot in its checkpoint
// and the next step restores it, so counters continue across steps.
func TestStepsFlow_BudgetCheckpointAndRestore(t *testing.T) {
	h := newSH(t)
	h.plan("EXECUTE", shRead("a", "First"))
	h.eval("EXECUTE", "next")

	res := h.must(StepKeyPlan)
	snap := shBudget(t, res)
	if snap.Used.PlanIterations != 1 || snap.Used.Tokens != 60 || snap.Caps.Wall != DefaultMaxWall {
		t.Fatalf("after plan: %+v", snap)
	}
	if res.Usage.InputTokens != 40 || res.Usage.OutputTokens != 20 {
		t.Fatalf("step usage %+v", res.Usage)
	}
	run := h.must("run_01")
	if got := shBudget(t, run); got.Used.PlanIterations != 1 || got.Used.Tokens != 60 {
		t.Fatalf("run slot did not restore the plan's counters: %+v", got)
	}
	ev := h.must("eval_01")
	got := shBudget(t, ev)
	if got.Used.PlanIterations != 1 || got.Used.Tokens != 75 {
		t.Fatalf("eval counters %+v", got)
	}
	steps, _ := h.steps.ListSteps(context.Background(), shWS, shExec)
	latest, ok := LatestBudgetSnapshot(steps)
	if !ok || latest.Used.Tokens != 75 {
		t.Fatalf("latest %+v ok=%v", latest, ok)
	}
}

// budgetSetup makes a tiny token budget so a bad first answer exhausts it.
func budgetSetup(t *testing.T) *shH {
	h := newSH(t, shSettings(func(s *StepSettings) {
		s.Caps.Tokens = 1000
		s.PlanEstimate, s.EvaluateEstimate, s.ExecuteEstimate = 0, 0, 0
	}))
	h.plan("EXECUTE", shRead("a", "First"))
	h.rt.Script("evaluate", rawReply(`{"bad":1}`, 600, 400), reply(t, m("decision", "EXECUTE", "reason_code", "next"), 10, 5))
	h.must(StepKeyPlan)
	h.must("run_01")
	return h
}

// B1-F6 (REQ-10): BeforeAttempt is wired, so a repair attempt is never made
// once the budget is spent; the step asks the owner instead.
func TestStepsFlow_RepairStopsAtBudget(t *testing.T) {
	h := budgetSetup(t)
	res, err := h.step("eval_01")
	if err != nil {
		t.Fatal(err)
	}
	if res.WaitState != workflow.StateWaitingForUser {
		t.Fatalf("wait state %q", res.WaitState)
	}
	if h.calls("evaluate") != 1 || h.rt.Remaining("evaluate") != 1 {
		t.Fatalf("repair call was made: calls=%d left=%d", h.calls("evaluate"), h.rt.Remaining("evaluate"))
	}
	if len(h.owner.asked) != 1 || h.owner.asked[0].Purpose != "budget_tokens" {
		t.Fatalf("%+v", h.owner.asked)
	}
	g := h.owner.asked[0]
	if g.Approve != "Thêm 1 vòng" || g.Reject != "Huỷ" || !strings.Contains(g.Text, "WF-1") {
		t.Fatalf("%+v", g)
	}
	if snap := shBudget(t, res); snap.Used.Tokens != 1060 {
		t.Fatalf("spent tokens of the bad call were not charged: %d", snap.Used.Tokens)
	}
}

// B1-F7 (REQ-10, human): O4a gate. Open gate keeps waiting without a model
// call; approve extends once and continues; Huỷ and expiry FAIL.
func TestStepsFlow_BudgetGate(t *testing.T) {
	t.Run("open gate keeps waiting", func(t *testing.T) {
		h := budgetSetup(t)
		h.step("eval_01")
		res, err := h.step("eval_01")
		if err != nil || res.WaitState != workflow.StateWaitingForUser {
			t.Fatalf("%q %v", res.WaitState, err)
		}
		if len(h.owner.asked) != 1 || h.calls("evaluate") != 1 {
			t.Fatalf("asked=%d calls=%d", len(h.owner.asked), h.calls("evaluate"))
		}
	})
	t.Run("approve extends and continues", func(t *testing.T) {
		h := budgetSetup(t)
		h.step("eval_01")
		h.owner.decide(GateApproved)
		res, err := h.step("eval_01")
		if err != nil || res.WaitState != "" {
			t.Fatalf("%q %v", res.WaitState, err)
		}
		snap := shBudget(t, res)
		if snap.Extensions != 1 || snap.Caps.Tokens != 1310 {
			t.Fatalf("%+v", snap)
		}
		if d := h.plans.lastDecision(); d.Decision != DecisionExecute {
			t.Fatalf("%+v", d)
		}
		if !h.owner.gates[0].Consumed {
			t.Fatal("gate signal not consumed")
		}
	})
	for _, c := range []struct{ name, decision string }{{"Huỷ", GateRejected}, {"expiry", GateExpired}} {
		t.Run(c.name+" fails", func(t *testing.T) {
			h := budgetSetup(t)
			h.step("eval_01")
			h.owner.decide(c.decision)
			if _, err := h.step("eval_01"); err != nil {
				t.Fatal(err)
			}
			if d := h.plans.lastDecision(); d.Decision != DecisionFail || d.ReasonCode != ReasonPlanningBudgetExceeded {
				t.Fatalf("%+v", d)
			}
			_, err := h.step(StepKeyReport)
			if code, _ := shStepErrCode(t, err); code != ReasonPlanningBudgetExceeded {
				t.Fatalf("report code %s", code)
			}
		})
	}
}

// B1-F8 (REQ-10): after MaxBudgetExtensions the execution fails at once with
// no gate and no model call.
func TestStepsFlow_ExtensionLimitFailsWithoutGate(t *testing.T) {
	h := newSH(t)
	caps := DefaultBudgetCaps()
	snap := BudgetSnapshot{Caps: caps, Used: BudgetUsed{Tokens: caps.Tokens}, StartedAt: t0, Extensions: MaxBudgetExtensions}
	h.steps.steps[StepKeyPlan] = workflow.Step{Key: StepKeyPlan, Position: 1, State: workflow.StepFailed,
		Checkpoint: &workflow.Checkpoint{References: []workflow.CheckpointReference{{Kind: CheckpointBudget, ReferenceID: encodeSnapshot(snap)}}}}
	res, err := h.step(StepKeyPlan)
	if err != nil || res.WaitState != "" {
		t.Fatalf("%q %v", res.WaitState, err)
	}
	if len(h.owner.asked) != 0 || len(h.rt.Requests()) != 0 {
		t.Fatalf("asked=%d requests=%d", len(h.owner.asked), len(h.rt.Requests()))
	}
	if d := h.plans.lastDecision(); d.Decision != DecisionFail || d.ReasonCode != ReasonPlanningBudgetExceeded {
		t.Fatalf("%+v", d)
	}
}

// B1-F9 (REQ-6, human): REQUEST_APPROVAL asks "Làm"/"Huỷ"; Làm continues,
// Huỷ and expiry FAIL with their own reasons; an open gate keeps waiting.
func TestStepsFlow_RequestApproval(t *testing.T) {
	setup := func(t *testing.T) (*shH, workflow.StepResult) {
		h := newSH(t)
		h.plan("EXECUTE", shRead("a", "First"))
		h.eval("REQUEST_APPROVAL", "needs_ok", "next_action", "xóa nhánh cũ")
		h.must(StepKeyPlan)
		h.must("run_01")
		res, err := h.step("eval_01")
		if err != nil {
			t.Fatal(err)
		}
		return h, res
	}
	h, res := setup(t)
	if res.WaitState != workflow.StateWaitingForApproval {
		t.Fatalf("wait %q", res.WaitState)
	}
	g := h.owner.asked[0]
	if g.Purpose != "approval" || g.Approve != "Làm" || g.Reject != "Huỷ" || g.TTL != 24*time.Hour {
		t.Fatalf("%+v", g)
	}
	shHas(t, g.Text, "WF-1", "xóa nhánh cũ")
	if r, err := h.step("eval_01"); err != nil || r.WaitState != workflow.StateWaitingForApproval || len(h.owner.asked) != 1 {
		t.Fatalf("open gate: %q %v asked=%d", r.WaitState, err, len(h.owner.asked))
	}
	h.owner.decide(GateApproved)
	if r, err := h.step("eval_01"); err != nil || r.WaitState != "" {
		t.Fatalf("approved: %q %v", r.WaitState, err)
	}
	if names := h.plans.decisionNames(); names[len(names)-1] != DecisionRequestApproval {
		t.Fatalf("approval must not add a FAIL: %v", names)
	}
	for _, c := range []struct{ decision, reason string }{{GateRejected, ReasonApprovalRejected}, {GateExpired, ReasonApprovalExpired}} {
		h, _ := setup(t)
		h.owner.decide(c.decision)
		if _, err := h.step("eval_01"); err != nil {
			t.Fatal(err)
		}
		if d := h.plans.lastDecision(); d.Decision != DecisionFail || d.ReasonCode != c.reason {
			t.Fatalf("%s: %+v", c.decision, d)
		}
	}
}

// B1-F10 (REQ-9): REQUEST_CLARIFICATION asks a question bound to WF-n and
// waits; the answer (a newer instruction) replans with clarification_answered.
func TestStepsFlow_ClarificationAnswerReplans(t *testing.T) {
	h := newSH(t)
	h.plan("EXECUTE", shRead("a", "First"))
	h.eval("REQUEST_CLARIFICATION", "need_info", "next_action", "bạn muốn US nào?")
	h.plan("EXECUTE", shRead("a", "First"), shRead("c", "Third", "a"))
	h.must(StepKeyPlan)
	h.must("run_01")
	res, err := h.step("eval_01")
	if err != nil || res.WaitState != workflow.StateWaitingForUser {
		t.Fatalf("%q %v", res.WaitState, err)
	}
	shHas(t, h.owner.lastSaid(), "WF-1", "bạn muốn US nào?")

	if r, err := h.step("eval_01"); err != nil || r.WaitState != workflow.StateWaitingForUser {
		t.Fatalf("no answer yet: %q %v", r.WaitState, err)
	}
	if h.calls("plan") != 1 {
		t.Fatal("replanned before the answer")
	}
	h.instr.add("WF-1 chọn dieu-khoan-cuoi")
	r, err := h.step("eval_01")
	if err != nil || r.WaitState != "" {
		t.Fatalf("after answer: %q %v", r.WaitState, err)
	}
	if h.calls("plan") != 2 || !strings.Contains(lastPrompt(t, h.rt), "dieu-khoan-cuoi") {
		t.Fatalf("plan calls=%d", h.calls("plan"))
	}
	var replan *DecisionRecord
	for _, d := range h.plans.decisions {
		if d.Decision == DecisionReplan {
			d := d
			replan = &d
		}
	}
	if replan == nil || replan.ReasonCode != ReasonClarified || replan.PlanVersion != 1 {
		t.Fatalf("%+v", replan)
	}
	if h.plans.planStatus(1) != PlanSuperseded || h.plans.planStatus(2) != PlanActive {
		t.Fatalf("%s %s", h.plans.planStatus(1), h.plans.planStatus(2))
	}
	if shBudget(t, r).Used.Replans != 1 {
		t.Fatal("replan not charged")
	}
}

// B1-F11 (REQ-8): REPLAN stores plan v+1; a finished task is carried with
// its result and is not run again; the old version is superseded.
func TestStepsFlow_ReplanCarriesFinishedTasks(t *testing.T) {
	h := newSH(t)
	h.plan("EXECUTE", shRead("a", "First"), shRead("b", "Second", "a"))
	h.eval("REPLAN", "need_more")
	h.plan("EXECUTE", ptask("a", "Changed title", []string{CapRepoWrite}), shRead("c", "Third", "a"))
	h.eval("COMPLETE", "done")

	key, _, err := h.flow()
	if key != "" || err != nil {
		t.Fatalf("stopped at %q: %v", key, err)
	}
	a1, a2 := h.plans.task(1, "a"), h.plans.task(2, "a")
	if a2.State != TaskSucceeded || a2.Title != "First" || a2.Executor != ExecutorInvestigate {
		t.Fatalf("carried task changed: %+v", a2)
	}
	if string(a2.Result) != string(a1.Result) || len(a2.EvidenceRefs) == 0 || a2.EvidenceRefs[0] != "wf1:step:run_01" {
		t.Fatalf("result not carried: %+v", a2)
	}
	if h.plans.task(2, "b").Key != "" {
		t.Fatal("unfinished task b was carried")
	}
	if got := h.inv.keys(); len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("runs %v", got)
	}
	if h.plans.planStatus(1) != PlanSuperseded || h.plans.planStatus(2) != PlanCompleted {
		t.Fatalf("%s %s", h.plans.planStatus(1), h.plans.planStatus(2))
	}
}

// B1-F12 (REQ-7): COMPLETE without proof is converted: REPLAN while the
// replan budget allows it, else FAIL missing_evidence; audited converted_from.
func TestStepsFlow_CompleteWithoutEvidenceConverted(t *testing.T) {
	t.Run("to REPLAN", func(t *testing.T) {
		h := newSH(t)
		h.plan("EXECUTE", shRead("a", "First"), shRead("b", "Second"))
		h.eval("COMPLETE", "done")
		h.plan("EXECUTE", shRead("a", "First"), shRead("b", "Second"))
		h.must(StepKeyPlan)
		h.must("run_01")
		h.must("eval_01")
		var conv *DecisionRecord
		for _, d := range h.plans.decisions {
			if d.ConvertedFrom != "" {
				d := d
				conv = &d
			}
		}
		if conv == nil || conv.Decision != DecisionReplan || conv.ReasonCode != ReasonMissingEvidence || conv.ConvertedFrom != DecisionComplete {
			t.Fatalf("%+v", conv)
		}
		if h.calls("plan") != 2 {
			t.Fatalf("plan calls %d", h.calls("plan"))
		}
	})
	t.Run("to FAIL without replan budget", func(t *testing.T) {
		h := newSH(t, shSettings(func(s *StepSettings) { s.Caps.Replans = 0 }))
		h.plan("EXECUTE", shRead("a", "First"), shRead("b", "Second"))
		h.eval("COMPLETE", "done")
		key, _, err := h.flow()
		if key != StepKeyReport {
			t.Fatalf("stopped at %q: %v", key, err)
		}
		if code, _ := shStepErrCode(t, err); code != ReasonMissingEvidence {
			t.Fatalf("code %s", code)
		}
		if d := h.plans.lastDecision(); d.Decision != DecisionFail || d.ConvertedFrom != DecisionComplete {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("worker task needs job id and a branch, PR or test", func(t *testing.T) {
		for _, c := range []struct {
			name string
			job  func(*WorkerJob)
			ok   bool
		}{
			{"job only", func(j *WorkerJob) { j.State = JobSucceeded }, false},
			{"with branch", func(j *WorkerJob) { j.State, j.Branch = JobSucceeded, "feat/x" }, true},
			{"with test outcome", func(j *WorkerJob) { j.State, j.TestOutcome = JobSucceeded, "passed" }, true},
			{"with PR", func(j *WorkerJob) { j.State, j.PRURL = JobSucceeded, "https://github.com/o/r/pull/1" }, true},
		} {
			t.Run(c.name, func(t *testing.T) {
				h := newSH(t, shSettings(func(s *StepSettings) { s.Caps.Replans = 0 }))
				h.plan("EXECUTE", shWrite("w", "Change"))
				h.eval("COMPLETE", "done")
				h.must(StepKeyPlan)
				// K1 ruling 1: the orchestrated worker wait is WAITING_FOR_WORKER.
				if r, err := h.step("run_01"); err != nil || r.WaitState != workflow.StateWaitingForWorker {
					t.Fatalf("%q %v", r.WaitState, err)
				}
				h.jobs.set("run_01", c.job)
				h.must("run_01")
				h.must("eval_01")
				d := h.plans.lastDecision()
				if c.ok && d.Decision != DecisionComplete {
					t.Fatalf("%+v", d)
				}
				if !c.ok && (d.Decision != DecisionFail || d.ReasonCode != ReasonMissingEvidence || d.ConvertedFrom != DecisionComplete) {
					t.Fatalf("%+v", d)
				}
			})
		}
	})
}

// B1-F13 (REQ-6): when no task ran and every task succeeded, the plan
// completes without a model call.
func TestStepsFlow_NoTaskRanAllSucceededCompletesWithoutModel(t *testing.T) {
	h := newSH(t)
	h.plan("EXECUTE", shRead("a", "First"))
	h.eval("EXECUTE", "next")
	key, _, err := h.flow()
	if key != "" || err != nil {
		t.Fatalf("%q %v", key, err)
	}
	if h.calls("evaluate") != 1 {
		t.Fatalf("evaluate calls %d", h.calls("evaluate"))
	}
	var found bool
	for _, d := range h.plans.decisions {
		if d.Decision == DecisionComplete && d.ReasonCode == ReasonAllTasksSucceeded {
			found = true
		}
	}
	if !found {
		t.Fatalf("%v", h.plans.decisionNames())
	}
	if h.plans.planStatus(1) != PlanCompleted {
		t.Fatalf("status %s", h.plans.planStatus(1))
	}
}

// B1-F14 (REQ-6): a task failed and its dependant cannot run: FAIL
// no_runnable_task, without a model call for the empty slot.
func TestStepsFlow_NoRunnableTaskFails(t *testing.T) {
	h := newSH(t)
	h.inv.fn = func(TaskRun) (TaskOutcome, error) {
		return TaskOutcome{Succeeded: false, ReasonCode: "boom_x"}, nil
	}
	h.plan("EXECUTE", shRead("a", "First"), shRead("b", "Second", "a"))
	h.eval("EXECUTE", "next")
	key, _, err := h.flow()
	if key != StepKeyReport {
		t.Fatalf("%q %v", key, err)
	}
	if code, _ := shStepErrCode(t, err); code != ReasonNoRunnableTask {
		t.Fatalf("code %s", code)
	}
	if h.calls("evaluate") != 1 || len(h.inv.keys()) != 1 {
		t.Fatalf("evaluate=%d runs=%v", h.calls("evaluate"), h.inv.keys())
	}
	if a := h.plans.task(1, "a"); a.State != TaskFailed || !strings.Contains(string(a.Result), "boom_x") {
		t.Fatalf("%+v", a)
	}
}

// B1-F15 (REQ-3): plan offers only capabilities of wired executors; a plan
// that needs repo.write without a worker fails as invalid output.
func TestStepsFlow_OnlyWiredCapabilitiesOffered(t *testing.T) {
	h := newSH(t, shNoJobs)
	for i := 0; i < 5; i++ {
		h.plan("EXECUTE", shWrite("w", "Change"))
	}
	if _, err := h.step(StepKeyPlan); err != nil {
		t.Fatal(err)
	}
	if p := h.rt.Requests()[0].Prompt; strings.Contains(p, CapRepoWrite) {
		t.Fatalf("repo.write offered without a worker:\n%s", p)
	}
	if d := h.plans.lastDecision(); d.Decision != DecisionFail || d.ReasonCode != ReasonInvalidOutput {
		t.Fatalf("%+v", d)
	}
}

// B1-F16: storage and route errors map to the right step errors.
func TestStepsFlow_LoadErrors(t *testing.T) {
	h := newSH(t)
	h.routes.err = ErrRouteNotFound
	_, err := h.step(StepKeyPlan)
	if code, retry := shStepErrCode(t, err); code != ReasonRouteMissing || retry {
		t.Fatalf("%s retry=%v", code, retry)
	}
	h.routes.err = errShDown
	_, err = h.step(StepKeyPlan)
	if code, retry := shStepErrCode(t, err); code != ReasonStorageUnavailable || !retry {
		t.Fatalf("%s retry=%v", code, retry)
	}
	h.routes.err = nil
	h.plans.failList = true
	_, err = h.step(StepKeyPlan)
	if code, retry := shStepErrCode(t, err); code != ReasonStorageUnavailable || !retry {
		t.Fatalf("%s retry=%v", code, retry)
	}
	if len(h.rt.Requests()) != 0 {
		t.Fatal("model called with broken state")
	}
}

// B1-F17: the last model failure ends in a reported FAIL llm_error instead
// of a silent step failure; earlier attempts retry.
func TestStepsFlow_LLMErrorRetriesThenFailsReported(t *testing.T) {
	h := newSH(t)
	h.rt.Script("plan", FakeReply{Err: fmt.Errorf("provider down")})
	_, err := h.stepAttempt(StepKeyPlan, 1)
	if code, retry := shStepErrCode(t, err); code != ReasonLLMError || !retry {
		t.Fatalf("%s retry=%v", code, retry)
	}
	h.rt.Script("plan", FakeReply{Err: fmt.Errorf("provider down")})
	res, err := h.stepAttempt(StepKeyPlan, 3)
	if err != nil || res.WaitState != "" {
		t.Fatalf("%q %v", res.WaitState, err)
	}
	if d := h.plans.lastDecision(); d.Decision != DecisionFail || d.ReasonCode != ReasonLLMError {
		t.Fatalf("%+v", d)
	}
}

// B1-F18 (REQ-9): InstructionWaker wakes only waiting orchestrator steps,
// ignores steps that are not eligible, and returns other errors.
type shStepList []workflow.Step

func (l shStepList) ListSteps(context.Context, string, int64) ([]workflow.Step, error) {
	return l, nil
}

type shWaker struct {
	woken []string
	errs  map[string]error
}

func (w *shWaker) WakeWorkerStep(_ context.Context, _ string, _ int64, step string) error {
	w.woken = append(w.woken, step)
	return w.errs[step]
}

func TestInstructionWaker_WakesOnlyWaitingOrchestratorSteps(t *testing.T) {
	steps := shStepList{
		{Key: "run_01", Kind: workflow.StepKindOrchestratorRunTask, State: workflow.StepWaiting},
		{Key: "eval_01", Kind: workflow.StepKindOrchestratorEvaluate, State: workflow.StepRunning},
		{Key: "other", Kind: "send_message", State: workflow.StepWaiting},
		{Key: "eval_02", Kind: workflow.StepKindOrchestratorEvaluate, State: workflow.StepWaiting},
	}
	w := &shWaker{errs: map[string]error{"run_01": workflow.ErrNotEligible}}
	iw := &InstructionWaker{Steps: steps, Waker: w}
	if err := iw.InstructionAdded(context.Background(), shWS, 1); err != nil {
		t.Fatal(err)
	}
	if strings.Join(w.woken, ",") != "run_01,eval_02" {
		t.Fatalf("woken %v", w.woken)
	}
	w = &shWaker{errs: map[string]error{"eval_02": errShDown}}
	iw = &InstructionWaker{Steps: steps, Waker: w}
	if err := iw.InstructionAdded(context.Background(), shWS, 1); err == nil {
		t.Fatal("waker error swallowed")
	}
	var nilWaker *InstructionWaker
	if nilWaker.InstructionAdded(context.Background(), shWS, 1) != nil || iw.InstructionAdded(context.Background(), shWS, 0) != nil {
		t.Fatal("nil waker or no execution must be a no-op")
	}
}

// B1-S1 (REQ-4): the plan message shows keys, titles, executors, dependencies
// and the caps line, in dependency order.
func TestSayPlan_Format(t *testing.T) {
	h := newSH(t)
	h.plan("EXECUTE", shRead("a", "First"), shRead("b", "Second", "a"), shWrite("c", "Third", "a", "b"))
	h.must(StepKeyPlan)
	msg := h.owner.lastSaid()
	shHas(t, msg,
		"WF-1: kế hoạch v1 (3 việc)",
		"Giới hạn: 3 vòng lập kế hoạch · 2 lần lập lại · 80000 token · 90 phút",
		"\n1. a · First [investigate]\n",
		"\n2. b · Second [investigate] ← a\n",
		"\n3. c · Third [local-worker] ← a, b")
	if h.owner.saidCount() != 1 {
		t.Fatalf("said %d messages", h.owner.saidCount())
	}
}

// B1-S2 (REQ-4, abuse): a long plan stays under the message limit and the
// cut line counts the remaining tasks exactly.
func TestSayPlan_Truncation(t *testing.T) {
	h := newSH(t)
	var tasks []map[string]any
	for i := 1; i <= MaxPlanTasks; i++ {
		tasks = append(tasks, shRead(fmt.Sprintf("t%02d", i), strings.Repeat("Việc dài ", 20)))
	}
	h.plan("EXECUTE", tasks...)
	h.must(StepKeyPlan)
	msg := h.owner.lastSaid()
	if n := utf8.RuneCountInString(msg); n > maxReportRunes {
		t.Fatalf("%d runes", n)
	}
	i := strings.LastIndex(msg, "… +")
	if i < 0 {
		t.Fatalf("no cut line:\n%s", msg)
	}
	var rest int
	if _, err := fmt.Sscanf(msg[i:], "… +%d việc nữa", &rest); err != nil || rest <= 0 {
		t.Fatalf("cut line %q: %v", msg[i:], err)
	}
	if !strings.HasSuffix(msg, "việc nữa") {
		t.Fatal("cut line is not last")
	}
	shown := 0
	for _, line := range strings.Split(msg, "\n") {
		var n int
		if _, err := fmt.Sscanf(line, "%d. t", &n); err == nil {
			shown++
		}
	}
	if shown+rest != MaxPlanTasks {
		t.Fatalf("shown %d + rest %d != %d", shown, rest, MaxPlanTasks)
	}
}

// B1-S3: a failed plan message never fails the step.
func TestSayPlan_SendFailureIgnored(t *testing.T) {
	h := newSH(t)
	h.owner.sayErr = errShDown
	h.plan("EXECUTE", shRead("a", "First"))
	if _, err := h.step(StepKeyPlan); err != nil {
		t.Fatal(err)
	}
	if h.plans.planStatus(1) != PlanActive {
		t.Fatal("plan not stored")
	}
}
