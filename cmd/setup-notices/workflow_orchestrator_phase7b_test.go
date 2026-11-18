package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/orchestrator"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Phase 7b B2 (REQ-14): `nh workflow show` and the orchestrator object.

// B2-W1: workflowCode lets codes through and replaces any free text.
func TestWorkflowCode_PassesCodesOnly(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "SUCCEEDED": "SUCCEEDED", "missing_evidence": "missing_evidence", "repo.write": "repo.write", "a-b_c.9": "a-b_c.9",
		"has space":              "unknown",
		"line\nbreak":            "unknown",
		"token=abc123":           "unknown",
		"https://evil.example/x": "unknown",
		"Việc":                   "unknown",
		strings.Repeat("a", 64):  strings.Repeat("a", 64),
		strings.Repeat("a", 65):  "unknown",
		"<script>":               "unknown",
		"IGNORE PREVIOUS":        "unknown",
	} {
		if got := workflowCode(in); got != want {
			t.Errorf("workflowCode(%q)=%q want %q", in, got, want)
		}
	}
}

func (f *workflowCLIFixture) insertPlan(t *testing.T, exec int64, version int, tokensIn int64, tasks ...orchestrator.PlanTaskRecord) {
	t.Helper()
	s, err := storage.NewOrchestratorStore(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertPlan(f.ctx, f.ws, orchestrator.PlanRecord{ExecutionID: exec, Version: version, SchemaVersion: 1, Intent: "task",
		TaskType: "bug_fix", Priority: "high", Complexity: "m", Confidence: 0.8, Budget: json.RawMessage(`{"tokens":1000}`),
		TokensIn: tokensIn, TokensOut: 5, CostUSD: 0.01}, tasks); err != nil {
		t.Fatal(err)
	}
}

func cliTask(key string, state string, evidence []string, deps ...string) orchestrator.PlanTaskRecord {
	return orchestrator.PlanTaskRecord{Key: key, Title: "Task " + key, Capabilities: []string{"repo.read"}, DependsOn: deps, Priority: "normal",
		Complexity: "s", Executor: "investigate", State: state, EvidenceRefs: evidence}
}

// B2-W2: a plain execution has no orchestrator object; an orchestrated one without plans neither.
func TestWorkflowShow_NoOrchestratorWithoutPlans(t *testing.T) {
	f := newWorkflowCLIFixture(t)
	plain := f.start(t, "plain", workflow.References{}, "model")
	out := decodeObject(t, f.must("workflow", "show", workflow.FormatRef(plain.ID), "--workspace-id", f.ws))
	if _, ok := out["orchestrator"]; ok {
		t.Fatalf("plain execution has an orchestrator object: %v", out["orchestrator"])
	}
	orch := f.start(t, "orch-no-plan", workflow.References{SourceKind: orchestrator.WorkflowSourceOrchestratedTask, SourceID: "1:1", Subject: "86abc", OperationKey: "op:orch"}, "model")
	out = decodeObject(t, f.must("workflow", "show", workflow.FormatRef(orch.ID), "--workspace-id", f.ws))
	if _, ok := out["orchestrator"]; ok {
		t.Fatalf("execution without plans has an orchestrator object: %v", out["orchestrator"])
	}
}

// B2-W3: with plans the object lists plan versions, tasks by version, decisions, summed usage; metadata only.
func TestWorkflowShow_OrchestratorObject(t *testing.T) {
	f := newWorkflowCLIFixture(t)
	ex := f.start(t, "orch", workflow.References{SourceKind: orchestrator.WorkflowSourceOrchestratedTask, SourceID: "1:2", Subject: "86abc", OperationKey: "op:orch2"}, "model")
	f.insertPlan(t, ex.ID, 1, 100, cliTask("a", orchestrator.TaskSucceeded, []string{"wf1:step:run_01", "https://github.com/o/r"}), cliTask("b", orchestrator.TaskPending, nil, "a"))
	f.insertPlan(t, ex.ID, 2, 250, cliTask("c", orchestrator.TaskPending, nil))
	s, _ := storage.NewOrchestratorStore(f.pool)
	if _, err := s.InsertDecision(f.ctx, f.ws, orchestrator.DecisionRecord{ExecutionID: ex.ID, PlanVersion: 1, TaskKey: "a", Decision: "REPLAN",
		ReasonCode: "missing_evidence", ConvertedFrom: "COMPLETE", EvidenceRefs: []string{"secret-model-ref"}}); err != nil {
		t.Fatal(err)
	}
	raw := f.must("workflow", "show", workflow.FormatRef(ex.ID), "--workspace-id", f.ws)
	o, ok := decodeObject(t, raw)["orchestrator"].(map[string]any)
	if !ok {
		t.Fatalf("no orchestrator object: %s", raw)
	}
	if plans := o["plans"].([]any); len(plans) != 2 || plans[0].(map[string]any)["version"] != float64(1) {
		t.Fatalf("plans %v", o["plans"])
	}
	tasks := o["tasks"].([]any)
	if len(tasks) != 3 {
		t.Fatalf("tasks %v", tasks)
	}
	byKey := map[string]map[string]any{}
	for _, it := range tasks {
		m := it.(map[string]any)
		byKey[m["key"].(string)] = m
	}
	if byKey["a"]["plan_version"] != float64(1) || byKey["c"]["plan_version"] != float64(2) || byKey["a"]["executor"] != "investigate" || byKey["a"]["state"] != orchestrator.TaskSucceeded {
		t.Fatalf("tasks %v", byKey)
	}
	if refs, _ := byKey["a"]["evidence_refs"].([]any); len(refs) != 2 {
		t.Fatalf("evidence refs %v", byKey["a"]["evidence_refs"])
	}
	if _, ok := byKey["b"]["evidence_refs"]; ok {
		t.Fatal("empty evidence refs listed")
	}
	if deps, ok := byKey["c"]["depends_on"].([]any); !ok || len(deps) != 0 {
		t.Fatalf("depends_on must be an empty list, got %v", byKey["c"]["depends_on"])
	}
	d := o["decisions"].([]any)[0].(map[string]any)
	if d["decision"] != "REPLAN" || d["reason"] != "missing_evidence" || d["converted_from"] != "COMPLETE" || d["task_key"] != "a" || d["plan_version"] != float64(1) {
		t.Fatalf("decision %v", d)
	}
	if strings.Contains(raw, "secret-model-ref") {
		t.Fatal("decision evidence refs printed")
	}
	u := o["usage"].(map[string]any)
	if u["input_tokens"] != float64(350) || u["output_tokens"] != float64(10) {
		t.Fatalf("usage %v", u)
	}
	if _, ok := o["budget"]; ok {
		t.Fatal("budget shown without a snapshot")
	}
}

// B2-W4: the newest budget snapshot of the steps is shown with used and cap.
func TestWorkflowShow_OrchestratorBudget(t *testing.T) {
	f := newWorkflowCLIFixture(t)
	ex := f.start(t, "orch-budget", workflow.References{SourceKind: orchestrator.WorkflowSourceOrchestratedTask, SourceID: "1:3", Subject: "86abc", OperationKey: "op:orch3"}, "model", "notify")
	f.insertPlan(t, ex.ID, 1, 1, cliTask("a", orchestrator.TaskPending, nil))
	started := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	snap := orchestrator.BudgetSnapshot{Caps: orchestrator.BudgetCaps{PlanIterations: 6, Replans: 3, RepairAttempts: 4, Tokens: 200000, Wall: 90 * time.Minute},
		Used: orchestrator.BudgetUsed{PlanIterations: 2, Replans: 1, RepairAttempts: 3, Tokens: 4500}, StartedAt: started, Extensions: 1}
	b, _ := json.Marshal(snap)
	cp, _ := json.Marshal(workflow.Checkpoint{References: []workflow.CheckpointReference{{Kind: orchestrator.CheckpointBudget, ReferenceID: base64.RawURLEncoding.EncodeToString(b)}}})
	if _, err := f.pool.Exec(f.ctx, `UPDATE execution_steps SET checkpoint=$1 WHERE execution_id=$2 AND position=1`, cp, ex.ID); err != nil {
		t.Fatal(err)
	}
	o := decodeObject(t, f.must("workflow", "show", workflow.FormatRef(ex.ID), "--workspace-id", f.ws))["orchestrator"].(map[string]any)
	bud, ok := o["budget"].(map[string]any)
	if !ok {
		t.Fatalf("no budget: %v", o)
	}
	pair := func(k string) (float64, float64) {
		m := bud[k].(map[string]any)
		return m["used"].(float64), m["cap"].(float64)
	}
	for k, want := range map[string][2]float64{"plan_iterations": {2, 6}, "replans": {1, 3}, "repair_attempts": {3, 4}, "tokens": {4500, 200000}} {
		if u, c := pair(k); u != want[0] || c != want[1] {
			t.Errorf("%s used %v cap %v want %v", k, u, c, want)
		}
	}
	wall := bud["wall_minutes"].(map[string]any)
	if wall["cap"] != float64(90) || bud["extensions"] != float64(1) {
		t.Fatalf("wall %v extensions %v", wall, bud["extensions"])
	}
}
