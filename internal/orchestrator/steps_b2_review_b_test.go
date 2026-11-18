package orchestrator

import (
	"strings"
	"testing"
)

// Review B rules (R-B-3, R-B-4): an in-process task needs executor evidence,
// and a retry never runs a task that is still RUNNING for the same step.

// B2-O1 (REQ-7): a task whose executor succeeds without any evidence ref
// cannot complete the plan: COMPLETE is converted (missing_evidence).
func TestStepsFlow_ExecutorWithoutEvidenceCannotComplete(t *testing.T) {
	h := newSH(t)
	h.inv.fn = func(TaskRun) (TaskOutcome, error) { return TaskOutcome{Succeeded: true}, nil }
	h.plan("EXECUTE", shRead("a", "First"))
	h.plan("EXECUTE", shRead("a", "First"))
	h.eval("COMPLETE", "done")
	h.must(StepKeyPlan)
	h.must("run_01")
	h.must("eval_01")
	var conv *DecisionRecord
	for _, d := range h.plans.decisions {
		if d.ConvertedFrom == DecisionComplete {
			d := d
			conv = &d
		}
	}
	if conv == nil || conv.ReasonCode != ReasonMissingEvidence {
		t.Fatalf("COMPLETE without executor evidence was accepted: %+v", h.plans.decisionNames())
	}
	if h.plans.task(1, "a").EvidenceRefs[0] != "wf1:step:run_01" {
		t.Fatalf("step ref missing: %v", h.plans.task(1, "a").EvidenceRefs)
	}
}

// B2-O2 (REQ-7): the executor's evidence refs are stored after the step ref and COMPLETE is then accepted.
func TestStepsFlow_ExecutorEvidenceStored(t *testing.T) {
	h := newSH(t)
	h.plan("EXECUTE", shRead("a", "First"))
	h.eval("COMPLETE", "done")
	if key, _, err := h.flow(); key != "" || err != nil {
		t.Fatalf("flow stopped at %q: %v", key, err)
	}
	refs := h.plans.task(1, "a").EvidenceRefs
	if len(refs) != 2 || refs[0] != "wf1:step:run_01" || refs[1] != "https://github.com/o/r" {
		t.Fatalf("refs %v", refs)
	}
}

// B2-O3 (REQ-7, abuse): a retry that finds its task RUNNING (the earlier attempt may have spent tokens and made calls) fails it with executor_interrupted and never calls the executor again.
func TestRunTask_RunningTaskOfSameStepIsInterrupted(t *testing.T) {
	h := newSH(t)
	h.plan("EXECUTE", shRead("a", "First"))
	h.must(StepKeyPlan)
	h.plans.mu.Lock()
	for i := range h.plans.tasks {
		if h.plans.tasks[i].Key == "a" {
			h.plans.tasks[i].State = TaskRunning
			h.plans.tasks[i].ResultStepID = "run_01"
		}
	}
	h.plans.mu.Unlock()
	h.must("run_01")
	if len(h.inv.keys()) != 0 {
		t.Fatalf("executor ran again: %v", h.inv.keys())
	}
	a := h.plans.task(1, "a")
	if a.State != TaskFailed || string(a.Result) == "" || !strings.Contains(string(a.Result), ReasonExecutorInterrupted) {
		t.Fatalf("task %+v result %s", a, a.Result)
	}
}
