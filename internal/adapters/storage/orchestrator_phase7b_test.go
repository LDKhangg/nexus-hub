package storage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/orchestrator"
)

type orchEnv struct {
	*wfEnv
	s         *OrchestratorStore
	exA, exA2 int64
	exB       int64
}

func newOrchEnv(t *testing.T) *orchEnv {
	t.Helper()
	e := newWFEnv(t)
	s, err := NewOrchestratorStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	vA := e.enabledVersion(workspaceA, "orch-a")
	vB := e.enabledVersion(workspaceB, "orch-b")
	return &orchEnv{
		wfEnv: e, s: s,
		exA:  e.start(workspaceA, vA.ID, "orch-a-1", chainDef("a")).ID,
		exA2: e.start(workspaceA, vA.ID, "orch-a-2", chainDef("a")).ID,
		exB:  e.start(workspaceB, vB.ID, "orch-b-1", chainDef("a")).ID,
	}
}

func orchTask(key string, deps ...string) orchestrator.PlanTaskRecord {
	return orchestrator.PlanTaskRecord{Key: key, Title: "Task " + key, Capabilities: []string{"repo.read"},
		DependsOn: deps, Priority: "normal", Complexity: "s", Executor: "investigate"}
}

func orchPlan(exec int64, version int) orchestrator.PlanRecord {
	return orchestrator.PlanRecord{ExecutionID: exec, Version: version, SchemaVersion: 1, Intent: "task", TaskType: "bug_fix",
		Priority: "high", Complexity: "m", Confidence: 0.8, Budget: json.RawMessage(`{"tokens":1000}`), TokensIn: 10, TokensOut: 5, CostUSD: 0.01}
}

func (e *orchEnv) planCount(exec int64) int {
	return e.count(`SELECT count(*) FROM orchestrator_plans WHERE execution_id=$1`, exec)
}

// TC-S1 (REQ-1): plan and tasks round trip.
func TestOrchestratorStore_RoundTrip(t *testing.T) {
	e := newOrchEnv(t)
	got, err := e.s.InsertPlan(e.ctx, workspaceA, orchPlan(e.exA, 1), []orchestrator.PlanTaskRecord{orchTask("a"), orchTask("b", "a")})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == 0 || got.Version != 1 || got.Status != orchestrator.PlanActive || got.Intent != "task" || got.Confidence != 0.8 || got.TokensIn != 10 {
		t.Fatalf("plan %+v", got)
	}
	plans, err := e.s.ListPlans(e.ctx, workspaceA, e.exA)
	if err != nil || len(plans) != 1 || plans[0].ID != got.ID {
		t.Fatalf("%+v %v", plans, err)
	}
	var budget map[string]int
	if err := json.Unmarshal(plans[0].Budget, &budget); err != nil || budget["tokens"] != 1000 {
		t.Fatalf("budget %s", plans[0].Budget)
	}
	tasks, err := e.s.ListTasks(e.ctx, workspaceA, e.exA)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("%+v %v", tasks, err)
	}
	if tasks[0].Key != "a" || tasks[1].Key != "b" || !slices.Equal(tasks[1].DependsOn, []string{"a"}) ||
		tasks[0].State != orchestrator.TaskPending || tasks[0].PlanID != got.ID || tasks[0].Executor != "investigate" ||
		tasks[0].Result != nil && string(tasks[0].Result) != "null" || tasks[0].EvidenceRefs == nil {
		t.Fatalf("tasks %+v", tasks)
	}
	// Defaults for omitted fields.
	d, err := e.s.InsertPlan(e.ctx, workspaceA, orchestrator.PlanRecord{ExecutionID: e.exA2, Version: 1}, []orchestrator.PlanTaskRecord{{Key: "x", Title: "X"}})
	if err != nil || d.Priority != "normal" || d.Complexity != "m" || d.SchemaVersion != 1 || string(d.Budget) != "{}" {
		t.Fatalf("%+v %v", d, err)
	}
}

// TC-S2 (REQ-1): InsertPlan is one transaction; a bad task leaves no plan and no tasks.
func TestOrchestratorStore_InsertRollsBack(t *testing.T) {
	e := newOrchEnv(t)
	cases := map[string][]orchestrator.PlanTaskRecord{
		"bad key on second task": {orchTask("a"), orchTask("Bad Key")},
		"duplicate keys":         {orchTask("a"), orchTask("a")},
		"title too long":         {orchTask("a"), {Key: "b", Title: strings.Repeat("x", 201)}},
		"empty title":            {{Key: "a", Title: ""}},
		"bad priority":           {orchTask("a"), {Key: "b", Title: "B", Priority: "asap"}},
		"bad executor":           {{Key: "a", Title: "A", Executor: "Bad Exec"}},
		"too many capabilities":  {{Key: "a", Title: "A", Capabilities: make([]string, 17)}},
	}
	for name, tasks := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := e.s.InsertPlan(e.ctx, workspaceA, orchPlan(e.exA, 1), tasks)
			if err == nil {
				t.Fatal("accepted")
			}
			if !errors.Is(err, behavior.ErrInvalid) && !errors.Is(err, behavior.ErrConflict) {
				t.Fatalf("unexpected error class: %v", err)
			}
			if n := e.planCount(e.exA); n != 0 {
				t.Fatalf("%d plan rows left after a failed insert", n)
			}
			if n := e.count(`SELECT count(*) FROM orchestrator_plan_tasks`); n != 0 {
				t.Fatalf("%d task rows left after a failed insert", n)
			}
		})
	}
	// The version is still free afterwards.
	if _, err := e.s.InsertPlan(e.ctx, workspaceA, orchPlan(e.exA, 1), []orchestrator.PlanTaskRecord{orchTask("a")}); err != nil {
		t.Fatal(err)
	}
}

// TC-S3 (REQ-1): plan and task limits are enforced before or by the database.
func TestOrchestratorStore_PlanLimits(t *testing.T) {
	e := newOrchEnv(t)
	one := []orchestrator.PlanTaskRecord{orchTask("a")}
	bad := func(name string, mut func(*orchestrator.PlanRecord), tasks []orchestrator.PlanTaskRecord) {
		t.Run(name, func(t *testing.T) {
			p := orchPlan(e.exA, 1)
			mut(&p)
			if _, err := e.s.InsertPlan(e.ctx, workspaceA, p, tasks); !errors.Is(err, behavior.ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
			if e.planCount(e.exA) != 0 {
				t.Fatal("plan row left")
			}
		})
	}
	bad("version 0", func(p *orchestrator.PlanRecord) { p.Version = 0 }, one)
	bad("execution 0", func(p *orchestrator.PlanRecord) { p.ExecutionID = 0 }, one)
	bad("no tasks", func(*orchestrator.PlanRecord) {}, nil)
	var many []orchestrator.PlanTaskRecord
	for i := 0; i < 51; i++ {
		many = append(many, orchTask("t"+string(rune('a'+i/10))+string(rune('a'+i%10))))
	}
	bad("51 tasks", func(*orchestrator.PlanRecord) {}, many)
	bad("confidence above 1", func(p *orchestrator.PlanRecord) { p.Confidence = 1.5 }, one)
	bad("negative confidence", func(p *orchestrator.PlanRecord) { p.Confidence = -0.1 }, one)
	bad("negative tokens", func(p *orchestrator.PlanRecord) { p.TokensIn = -1 }, one)
	bad("negative cost", func(p *orchestrator.PlanRecord) { p.CostUSD = -1 }, one)
	bad("intent not a label", func(p *orchestrator.PlanRecord) { p.Intent = "Drop Table" }, one)
	bad("bad priority", func(p *orchestrator.PlanRecord) { p.Priority = "asap" }, one)
	bad("budget over 1 KiB", func(p *orchestrator.PlanRecord) {
		p.Budget = json.RawMessage(`{"x":"` + strings.Repeat("a", 1100) + `"}`)
	}, one)
	bad("budget not an object", func(p *orchestrator.PlanRecord) { p.Budget = json.RawMessage(`[1]`) }, one)
	// 50 tasks is fine.
	if _, err := e.s.InsertPlan(e.ctx, workspaceA, orchPlan(e.exA, 1), many[:50]); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.InsertPlan(e.ctx, "not-a-uuid", orchPlan(e.exA, 2), one); !errors.Is(err, behavior.ErrInvalid) {
		t.Fatalf("bad workspace id: %v", err)
	}
}

// TC-S4 (REQ-1): (execution_id, version) is unique and a second writer changes nothing.
func TestOrchestratorStore_UniqueVersion(t *testing.T) {
	e := newOrchEnv(t)
	if _, err := e.s.InsertPlan(e.ctx, workspaceA, orchPlan(e.exA, 1), []orchestrator.PlanTaskRecord{orchTask("a")}); err != nil {
		t.Fatal(err)
	}
	_, err := e.s.InsertPlan(e.ctx, workspaceA, orchPlan(e.exA, 1), []orchestrator.PlanTaskRecord{orchTask("zz")})
	if !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	if n := e.count(`SELECT count(*) FROM orchestrator_plan_tasks`); n != 1 {
		t.Fatalf("tasks=%d", n)
	}
	// Same version number on another execution is fine; the next version is fine.
	if _, err := e.s.InsertPlan(e.ctx, workspaceA, orchPlan(e.exA2, 1), []orchestrator.PlanTaskRecord{orchTask("a")}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.InsertPlan(e.ctx, workspaceA, orchPlan(e.exA, 2), []orchestrator.PlanTaskRecord{orchTask("a")}); err != nil {
		t.Fatal(err)
	}
	// The unique key is global, not per workspace: workspace B cannot take it either (FK refuses first).
	if _, err := e.s.InsertPlan(e.ctx, workspaceB, orchPlan(e.exA, 3), []orchestrator.PlanTaskRecord{orchTask("a")}); err == nil {
		t.Fatal("workspace B inserted a plan for A's execution")
	}
}

// TC-S5 (REQ-1): the trigger rejects every non-status UPDATE of a plan; status changes work.
func TestOrchestratorStore_PlanImmutable(t *testing.T) {
	e := newOrchEnv(t)
	p, err := e.s.InsertPlan(e.ctx, workspaceA, orchPlan(e.exA, 1), []orchestrator.PlanTaskRecord{orchTask("a")})
	if err != nil {
		t.Fatal(err)
	}
	for _, set := range []string{
		"intent='other'", "task_type='docs'", "priority='low'", "complexity='xl'", "confidence=0.1",
		`budget='{"tokens":1}'`, "tokens_in=999", "tokens_out=999", "cost_usd=9", "version=7", "schema_version=2",
		"created_at=now()+interval '1 day'", "execution_id=" + itoa(e.exA2),
	} {
		_, err := e.pool.Exec(e.ctx, `UPDATE orchestrator_plans SET `+set+` WHERE id=$1`, p.ID)
		if err == nil {
			t.Fatalf("UPDATE %s accepted", set)
		}
		if !strings.Contains(err.Error(), "immutable") && !strings.Contains(err.Error(), "violates") {
			t.Fatalf("UPDATE %s: %v", set, err)
		}
	}
	// A status change together with another column is also refused.
	if _, err := e.pool.Exec(e.ctx, `UPDATE orchestrator_plans SET status='FAILED', intent='other' WHERE id=$1`, p.ID); err == nil {
		t.Fatal("mixed update accepted")
	}
	plans, _ := e.s.ListPlans(e.ctx, workspaceA, e.exA)
	if plans[0].Intent != "task" || plans[0].Status != orchestrator.PlanActive || plans[0].TokensIn != 10 {
		t.Fatalf("plan changed: %+v", plans[0])
	}
	for _, st := range []string{"SUPERSEDED", "COMPLETED", "FAILED", "CANCELLED", "ACTIVE"} {
		if err := e.s.SetPlanStatus(e.ctx, workspaceA, e.exA, 1, st); err != nil {
			t.Fatalf("status %s: %v", st, err)
		}
	}
	if err := e.s.SetPlanStatus(e.ctx, workspaceA, e.exA, 1, "DONE"); !errors.Is(err, behavior.ErrInvalid) {
		t.Fatalf("bad status: %v", err)
	}
	if err := e.s.SetPlanStatus(e.ctx, workspaceA, e.exA, 9, "FAILED"); !errors.Is(err, behavior.ErrInvalid) {
		t.Fatalf("unknown version: %v", err)
	}
	if err := e.s.SetPlanStatus(e.ctx, "not-a-uuid", e.exA, 1, "FAILED"); !errors.Is(err, behavior.ErrInvalid) {
		t.Fatalf("bad workspace: %v", err)
	}
	// DELETE of a plan with tasks is blocked by the task foreign key.
	if _, err := e.pool.Exec(e.ctx, `DELETE FROM orchestrator_plans WHERE id=$1`, p.ID); err == nil {
		t.Fatal("plan with tasks deleted")
	}
}

// TC-S6 (REQ-1): everything is workspace scoped.
func TestOrchestratorStore_WorkspaceScoping(t *testing.T) {
	e := newOrchEnv(t)
	pa, err := e.s.InsertPlan(e.ctx, workspaceA, orchPlan(e.exA, 1), []orchestrator.PlanTaskRecord{orchTask("a")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.InsertDecision(e.ctx, workspaceA, orchestrator.DecisionRecord{ExecutionID: e.exA, PlanVersion: 1, Decision: "EXECUTE", ReasonCode: "ok"}); err != nil {
		t.Fatal(err)
	}
	pb, err := e.s.InsertPlan(e.ctx, workspaceB, orchPlan(e.exB, 1), []orchestrator.PlanTaskRecord{orchTask("a")})
	if err != nil {
		t.Fatal(err)
	}
	// Reads with the wrong workspace return nothing.
	if l, _ := e.s.ListPlans(e.ctx, workspaceB, e.exA); len(l) != 0 {
		t.Fatalf("B reads A plans: %+v", l)
	}
	if l, _ := e.s.ListTasks(e.ctx, workspaceB, e.exA); len(l) != 0 {
		t.Fatalf("B reads A tasks: %+v", l)
	}
	if l, _ := e.s.ListDecisions(e.ctx, workspaceB, e.exA); len(l) != 0 {
		t.Fatalf("B reads A decisions: %+v", l)
	}
	if l, _ := e.s.ListTasks(e.ctx, workspaceA, e.exB); len(l) != 0 {
		t.Fatalf("A reads B tasks: %+v", l)
	}
	// Writes with the wrong workspace fail and change nothing.
	if err := e.s.SetPlanStatus(e.ctx, workspaceB, e.exA, 1, "FAILED"); !errors.Is(err, behavior.ErrInvalid) {
		t.Fatalf("B set A status: %v", err)
	}
	if err := e.s.UpdateTask(e.ctx, workspaceB, orchestrator.PlanTaskRecord{PlanID: pa.ID, Key: "a", State: "RUNNING"}); !errors.Is(err, behavior.ErrInvalid) {
		t.Fatalf("B updated A task: %v", err)
	}
	if _, err := e.s.InsertDecision(e.ctx, workspaceB, orchestrator.DecisionRecord{ExecutionID: e.exA, PlanVersion: 1, Decision: "FAIL"}); err == nil {
		t.Fatal("B recorded a decision on A's execution")
	}
	if _, err := e.s.InsertPlan(e.ctx, workspaceB, orchPlan(e.exA, 5), []orchestrator.PlanTaskRecord{orchTask("a")}); err == nil {
		t.Fatal("B inserted a plan on A's execution")
	}
	// The composite foreign keys refuse a cross-workspace task or plan row in raw SQL too.
	_, err = e.pool.Exec(e.ctx, `INSERT INTO orchestrator_plan_tasks(workspace_id,plan_id,key,title) VALUES($1,$2,'evil','Evil')`, workspaceB, pa.ID)
	requirePGCode(t, err, "23503")
	_, err = e.pool.Exec(e.ctx, `INSERT INTO orchestrator_plans(workspace_id,execution_id,version,schema_version) VALUES($1,$2,9,1)`, workspaceB, e.exA)
	requirePGCode(t, err, "23503")
	_, err = e.pool.Exec(e.ctx, `INSERT INTO orchestrator_decisions(workspace_id,execution_id,plan_version,decision) VALUES($1,$2,1,'FAIL')`, workspaceB, e.exA)
	requirePGCode(t, err, "23503")
	// Each side still sees only its own.
	if l, _ := e.s.ListTasks(e.ctx, workspaceA, e.exA); len(l) != 1 || l[0].PlanID != pa.ID || l[0].State != "PENDING" {
		t.Fatalf("A tasks %+v", l)
	}
	if l, _ := e.s.ListTasks(e.ctx, workspaceB, e.exB); len(l) != 1 || l[0].PlanID != pb.ID {
		t.Fatalf("B tasks %+v", l)
	}
}

// TC-S7 (REQ-1): row level security is enabled on every new table.
func TestOrchestratorStore_RLSEnabled(t *testing.T) {
	e := newOrchEnv(t)
	for _, tbl := range []string{"orchestrator_plans", "orchestrator_plan_tasks", "orchestrator_decisions"} {
		var on bool
		if err := e.pool.QueryRow(e.ctx, `SELECT relrowsecurity FROM pg_class WHERE oid=('public.'||$1)::regclass`, tbl).Scan(&on); err != nil || !on {
			t.Fatalf("%s: RLS enabled=%v err=%v", tbl, on, err)
		}
	}
}

// TC-S8 (REQ-1): UpdateTask stores state, result and evidence within limits.
func TestOrchestratorStore_UpdateTask(t *testing.T) {
	e := newOrchEnv(t)
	p, err := e.s.InsertPlan(e.ctx, workspaceA, orchPlan(e.exA, 1), []orchestrator.PlanTaskRecord{orchTask("a"), orchTask("b")})
	if err != nil {
		t.Fatal(err)
	}
	upd := orchestrator.PlanTaskRecord{PlanID: p.ID, Key: "a", State: orchestrator.TaskSucceeded, Executor: "local-worker",
		ResultStepID: "step-9", Result: json.RawMessage(`{"outcome":"ok"}`), EvidenceRefs: []string{"s1", "pr-7"}}
	if err := e.s.UpdateTask(e.ctx, workspaceA, upd); err != nil {
		t.Fatal(err)
	}
	tasks, _ := e.s.ListTasks(e.ctx, workspaceA, e.exA)
	a, b := tasks[0], tasks[1]
	if a.State != "SUCCEEDED" || a.Executor != "local-worker" || a.ResultStepID != "step-9" || !slices.Equal(a.EvidenceRefs, []string{"s1", "pr-7"}) {
		t.Fatalf("a %+v", a)
	}
	var res map[string]string
	if err := json.Unmarshal(a.Result, &res); err != nil || res["outcome"] != "ok" {
		t.Fatalf("result %s", a.Result)
	}
	if b.State != "PENDING" {
		t.Fatal("update touched another task")
	}
	for name, mut := range map[string]func(*orchestrator.PlanTaskRecord){
		"bad state":    func(r *orchestrator.PlanTaskRecord) { r.State = "DONE" },
		"result array": func(r *orchestrator.PlanTaskRecord) { r.Result = json.RawMessage(`[1]`) },
		"result too big": func(r *orchestrator.PlanTaskRecord) {
			r.Result = json.RawMessage(`{"x":"` + strings.Repeat("a", 4200) + `"}`)
		},
		"evidence big":    func(r *orchestrator.PlanTaskRecord) { r.EvidenceRefs = []string{strings.Repeat("e", 4200)} },
		"step id too big": func(r *orchestrator.PlanTaskRecord) { r.ResultStepID = strings.Repeat("s", 129) },
		"bad executor":    func(r *orchestrator.PlanTaskRecord) { r.Executor = "Bad Exec" },
	} {
		r := upd
		mut(&r)
		if err := e.s.UpdateTask(e.ctx, workspaceA, r); !errors.Is(err, behavior.ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := e.s.UpdateTask(e.ctx, workspaceA, orchestrator.PlanTaskRecord{PlanID: p.ID, Key: "nope", State: "RUNNING"}); !errors.Is(err, behavior.ErrInvalid) {
		t.Fatalf("unknown task: %v", err)
	}
	// Task fields other than state, executor, result and evidence are not changed by UpdateTask.
	if tasks, _ = e.s.ListTasks(e.ctx, workspaceA, e.exA); tasks[0].Title != "Task a" || !slices.Equal(tasks[0].Capabilities, []string{"repo.read"}) {
		t.Fatalf("definition changed: %+v", tasks[0])
	}
	// Failed updates left the successful one intact.
	if tasks[0].State != "SUCCEEDED" {
		t.Fatal("failed update changed state")
	}
}

// TC-S9 (REQ-1): decisions, including a Go conversion, are audited with bounded fields.
func TestOrchestratorStore_Decisions(t *testing.T) {
	e := newOrchEnv(t)
	d1, err := e.s.InsertDecision(e.ctx, workspaceA, orchestrator.DecisionRecord{ExecutionID: e.exA, PlanVersion: 1, TaskKey: "a",
		Decision: "REQUEST_CLARIFICATION", ReasonCode: "needs_evidence", ConvertedFrom: "COMPLETE", EvidenceRefs: []string{"s1"}})
	if err != nil {
		t.Fatal(err)
	}
	d2, err := e.s.InsertDecision(e.ctx, workspaceA, orchestrator.DecisionRecord{ExecutionID: e.exA, PlanVersion: 1, Decision: "COMPLETE"})
	if err != nil {
		t.Fatal(err)
	}
	if d1.ID == 0 || d1.TaskKey != "a" || d1.ConvertedFrom != "COMPLETE" || !slices.Equal(d1.EvidenceRefs, []string{"s1"}) {
		t.Fatalf("%+v", d1)
	}
	if d2.TaskKey != "" || d2.ConvertedFrom != "" || d2.EvidenceRefs == nil || len(d2.EvidenceRefs) != 0 || d2.ReasonCode != "" {
		t.Fatalf("%+v", d2)
	}
	l, _ := e.s.ListDecisions(e.ctx, workspaceA, e.exA)
	if len(l) != 2 || l[0].ID != d1.ID || l[1].ID != d2.ID {
		t.Fatalf("order %+v", l)
	}
	for name, d := range map[string]orchestrator.DecisionRecord{
		"unknown decision":  {ExecutionID: e.exA, PlanVersion: 1, Decision: "MAYBE"},
		"lowercase":         {ExecutionID: e.exA, PlanVersion: 1, Decision: "execute"},
		"bad converted":     {ExecutionID: e.exA, PlanVersion: 1, Decision: "FAIL", ConvertedFrom: "MAYBE"},
		"reason with space": {ExecutionID: e.exA, PlanVersion: 1, Decision: "FAIL", ReasonCode: "Bad Reason"},
		"bad task key":      {ExecutionID: e.exA, PlanVersion: 1, Decision: "FAIL", TaskKey: "Bad Key"},
		"plan version 0":    {ExecutionID: e.exA, PlanVersion: 0, Decision: "FAIL"},
		"evidence too big":  {ExecutionID: e.exA, PlanVersion: 1, Decision: "FAIL", EvidenceRefs: []string{strings.Repeat("e", 4200)}},
	} {
		if _, err := e.s.InsertDecision(e.ctx, workspaceA, d); !errors.Is(err, behavior.ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := e.s.InsertDecision(e.ctx, workspaceA, orchestrator.DecisionRecord{ExecutionID: 99999, PlanVersion: 1, Decision: "FAIL"}); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("unknown execution: %v", err)
	}
	if l, _ := e.s.ListDecisions(e.ctx, workspaceA, e.exA); len(l) != 2 {
		t.Fatalf("rejected decisions were stored: %d", len(l))
	}
}

// TC-S10 (REQ-1): tasks of every version are listed by version; versions stay independent.
func TestOrchestratorStore_VersionsListedInOrder(t *testing.T) {
	e := newOrchEnv(t)
	if _, err := e.s.InsertPlan(e.ctx, workspaceA, orchPlan(e.exA, 1), []orchestrator.PlanTaskRecord{orchTask("a"), orchTask("b")}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.InsertPlan(e.ctx, workspaceA, orchPlan(e.exA, 2), []orchestrator.PlanTaskRecord{orchTask("a"), orchTask("c")}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SetPlanStatus(e.ctx, workspaceA, e.exA, 1, orchestrator.PlanSuperseded); err != nil {
		t.Fatal(err)
	}
	plans, _ := e.s.ListPlans(e.ctx, workspaceA, e.exA)
	if len(plans) != 2 || plans[0].Version != 1 || plans[0].Status != "SUPERSEDED" || plans[1].Version != 2 || plans[1].Status != "ACTIVE" {
		t.Fatalf("%+v", plans)
	}
	tasks, _ := e.s.ListTasks(e.ctx, workspaceA, e.exA)
	var keys []string
	for _, tk := range tasks {
		keys = append(keys, tk.Key)
	}
	if strings.Join(keys, ",") != "a,b,a,c" || tasks[0].PlanID == tasks[2].PlanID {
		t.Fatalf("keys %v", keys)
	}
}

// TC-S11 (REQ-1): the Down migration removes the tables and function; Up restores them with RLS and trigger.
func TestOrchestratorMigration_DownUp(t *testing.T) {
	e := newOrchEnv(t)
	if _, err := e.s.InsertPlan(e.ctx, workspaceA, orchPlan(e.exA, 1), []orchestrator.PlanTaskRecord{orchTask("a")}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.ctx, e.pool, downSection(t, "00028_orchestrator.sql"))
	for _, name := range []string{"orchestrator_plans", "orchestrator_plan_tasks", "orchestrator_decisions"} {
		var exists bool
		if err := e.pool.QueryRow(e.ctx, `SELECT to_regclass('public.'||$1) IS NOT NULL`, name).Scan(&exists); err != nil || exists {
			t.Fatalf("Down left %s (err=%v)", name, err)
		}
	}
	var fn bool
	if err := e.pool.QueryRow(e.ctx, `SELECT EXISTS(SELECT 1 FROM pg_proc WHERE proname='orchestrator_plans_immutable')`).Scan(&fn); err != nil || fn {
		t.Fatal("Down left the trigger function")
	}
	if e.count(`SELECT count(*) FROM workflow_executions`) != 3 {
		t.Fatal("Down touched workflow executions")
	}
	_, file, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../../db/migrations/00028_orchestrator.sql"))
	if err != nil {
		t.Fatal(err)
	}
	up, _, _ := strings.Cut(string(raw), "-- +goose Down")
	mustExec(t, e.ctx, e.pool, up)
	// The store also needs the later orchestrator migration (merge_target, 00037); the approvals CHECK it redefines is replaced by name, so applying it again is safe.
	mustExec(t, e.ctx, e.pool, upSection(t, "00037_orchestrator_merge_target.sql"))
	var on bool
	if err := e.pool.QueryRow(e.ctx, `SELECT relrowsecurity FROM pg_class WHERE oid='public.orchestrator_plans'::regclass`).Scan(&on); err != nil || !on {
		t.Fatal("Up did not restore RLS")
	}
	p, err := e.s.InsertPlan(e.ctx, workspaceA, orchPlan(e.exA, 1), []orchestrator.PlanTaskRecord{orchTask("a")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE orchestrator_plans SET intent='other' WHERE id=$1`, p.ID); err == nil {
		t.Fatal("Up did not restore the immutability trigger")
	}
}

// R-A-2 (REQ-1, REQ-8): tasks carried into a new plan version keep state, result step, result and evidence.
func TestOrchestratorStore_InsertPlanKeepsCarriedTaskState(t *testing.T) {
	e := newOrchEnv(t)
	carried := orchTask("a")
	carried.State = orchestrator.TaskSucceeded
	carried.ResultStepID = "step-3"
	carried.Result = json.RawMessage(`{"outcome":"ok"}`)
	carried.EvidenceRefs = []string{"s1", "pr-7"}
	p, err := e.s.InsertPlan(e.ctx, workspaceA, orchPlan(e.exA, 2), []orchestrator.PlanTaskRecord{carried, orchTask("b", "a")})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := e.s.ListTasks(e.ctx, workspaceA, e.exA)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("%+v %v", tasks, err)
	}
	a, b := tasks[0], tasks[1]
	if a.PlanID != p.ID || a.State != orchestrator.TaskSucceeded || a.ResultStepID != "step-3" || !slices.Equal(a.EvidenceRefs, []string{"s1", "pr-7"}) {
		t.Fatalf("carried task lost its state: %+v", a)
	}
	var res map[string]string
	if err := json.Unmarshal(a.Result, &res); err != nil || res["outcome"] != "ok" {
		t.Fatalf("result %s", a.Result)
	}
	if b.State != orchestrator.TaskPending || b.ResultStepID != "" || len(b.EvidenceRefs) != 0 {
		t.Fatalf("new task not pending/empty: %+v", b)
	}
	if b.Result != nil && string(b.Result) != "null" {
		t.Fatalf("empty result must be NULL, got %s", b.Result)
	}
	var nulls int
	nulls = e.count(`SELECT count(*) FROM orchestrator_plan_tasks WHERE plan_id=$1 AND key='b' AND result IS NULL`, p.ID)
	if nulls != 1 {
		t.Fatal("empty result was not stored as SQL NULL")
	}
}

func TestOrchestratorStore_InsertPlanEmptyStateIsPending(t *testing.T) {
	e := newOrchEnv(t)
	task := orchTask("a")
	task.State = ""
	if _, err := e.s.InsertPlan(e.ctx, workspaceA, orchPlan(e.exA, 1), []orchestrator.PlanTaskRecord{task}); err != nil {
		t.Fatal(err)
	}
	tasks, _ := e.s.ListTasks(e.ctx, workspaceA, e.exA)
	if len(tasks) != 1 || tasks[0].State != orchestrator.TaskPending {
		t.Fatalf("%+v", tasks)
	}
}

// A carried task with an invalid state or result rolls the whole plan back.
func TestOrchestratorStore_InsertPlanCarriedInvalidRollsBack(t *testing.T) {
	e := newOrchEnv(t)
	bad := map[string]func(*orchestrator.PlanTaskRecord){
		"bad state":    func(r *orchestrator.PlanTaskRecord) { r.State = "DONE" },
		"result array": func(r *orchestrator.PlanTaskRecord) { r.Result = json.RawMessage(`[1]`) },
		"step too big": func(r *orchestrator.PlanTaskRecord) { r.ResultStepID = strings.Repeat("s", 129) },
		"evidence big": func(r *orchestrator.PlanTaskRecord) { r.EvidenceRefs = []string{strings.Repeat("e", 4200)} },
	}
	for name, mut := range bad {
		t.Run(name, func(t *testing.T) {
			ok := orchTask("a")
			ok.State = orchestrator.TaskSucceeded
			ok.Result = json.RawMessage(`{"outcome":"ok"}`)
			ng := orchTask("b")
			ng.State = orchestrator.TaskSucceeded
			ng.Result = json.RawMessage(`{"outcome":"ok"}`)
			mut(&ng)
			if _, err := e.s.InsertPlan(e.ctx, workspaceA, orchPlan(e.exA, 1), []orchestrator.PlanTaskRecord{ok, ng}); err == nil {
				t.Fatal("accepted")
			}
			if n := e.planCount(e.exA); n != 0 {
				t.Fatalf("%d plan rows left", n)
			}
			if n := e.count(`SELECT count(*) FROM orchestrator_plan_tasks`); n != 0 {
				t.Fatalf("%d task rows left", n)
			}
		})
	}
}
