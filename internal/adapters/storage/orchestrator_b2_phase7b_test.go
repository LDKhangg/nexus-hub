package storage

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/orchestrator"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Phase 7b Fix Brief B2 (REQ-6, REQ-7, REQ-9, REQ-11, REQ-13, REQ-14): the
// database side of the orchestrator: migrations 00029 and 00030,
// orchestrator_routes, the worker wake query, tool_runs ownership and the
// durable investigate audit. CI only; DB tests use the loopback test DB.

type b2Env struct {
	*wfEnv
	routes *OrchestratedTaskStore
	plans  *OrchestratorStore
	update int64
}

// newB2Env is a database with workspace A cut over to the durable runtime
// (workspace B is not) and no execution yet.
func newB2Env(t *testing.T) *b2Env {
	t.Helper()
	e := newWFEnv(t)
	if _, err := e.store.Cutover(e.ctx, workspaceA, true); err != nil {
		t.Fatalf("cutover: %v", err)
	}
	routes, err := NewOrchestratedTaskStore(e.pool, e.store)
	if err != nil {
		t.Fatal(err)
	}
	plans, err := NewOrchestratorStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	return &b2Env{wfEnv: e, routes: routes, plans: plans}
}

func (e *b2Env) req(update int64) orchestrator.TaskRequest {
	return orchestrator.TaskRequest{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: 7001, ThreadID: 5,
		UpdateID: update, Repo: "acme/api", TaskRef: "FC-1", ClickUpTaskID: "86abc"}
}

func (e *b2Env) startTask(t *testing.T) int64 {
	t.Helper()
	e.update++
	ex, err := e.routes.StartOrchestratedTask(e.ctx, e.req(e.update))
	if err != nil {
		t.Fatalf("start orchestrated task: %v", err)
	}
	return ex.ID
}

func (e *b2Env) migrate(sql string) {
	e.t.Helper()
	mustExec(e.t, e.ctx, e.pool, sql)
}

// ---- orchestrator_routes (00029) ----

// B2-S1 (REQ-6): a start stores the route; the same update returns the same
// execution and no second route; another update is a new execution.
func TestB2Routes_StartStoresRouteAndIsIdempotent(t *testing.T) {
	e := newB2Env(t)
	first, err := e.routes.StartOrchestratedTask(e.ctx, e.req(1))
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.routes.StartOrchestratedTask(e.ctx, e.req(1))
	if err != nil || again.ID != first.ID {
		t.Fatalf("redelivery: id %d err %v want %d", again.ID, err, first.ID)
	}
	if n := e.count(`SELECT count(*) FROM orchestrator_routes`); n != 1 {
		t.Fatalf("routes after redelivery: %d", n)
	}
	other, err := e.routes.StartOrchestratedTask(e.ctx, e.req(2))
	if err != nil || other.ID == first.ID {
		t.Fatalf("second update: %d %v", other.ID, err)
	}
	if n := e.count(`SELECT count(*) FROM orchestrator_routes`); n != 2 {
		t.Fatalf("routes %d", n)
	}
	rt, err := e.routes.GetRoute(e.ctx, workspaceA, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := orchestrator.Route{WorkspaceID: workspaceA, ExecutionID: first.ID, IntegrationID: integrationA, ChatID: 7001, ThreadID: 5,
		Repo: "acme/api", TaskRef: "FC-1", ClickUpTaskID: "86abc"}
	if rt != want {
		t.Fatalf("route %+v want %+v", rt, want)
	}
	// The execution is the orchestrated_task source kind, pinned to the internal behavior.
	var kind string
	if err := e.pool.QueryRow(e.ctx, `SELECT source_kind FROM workflow_executions WHERE id=$1`, first.ID).Scan(&kind); err != nil || kind != orchestrator.WorkflowSourceOrchestratedTask {
		t.Fatalf("source kind %q err %v", kind, err)
	}
}

// B2-S2 (REQ-6): many starts share one internal behavior and one version while the definition is unchanged.
func TestB2Routes_InternalBehaviorAndVersionReused(t *testing.T) {
	e := newB2Env(t)
	for i := 0; i < 3; i++ {
		e.startTask(t)
	}
	if n := e.count(`SELECT count(*) FROM behaviors WHERE workspace_id=$1 AND internal_kind='orchestrated_task'`, workspaceA); n != 1 {
		t.Fatalf("internal behaviors %d", n)
	}
	if n := e.count(`SELECT count(*) FROM behavior_versions v JOIN behaviors b ON b.workspace_id=v.workspace_id AND b.id=v.behavior_id WHERE b.internal_kind='orchestrated_task'`); n != 1 {
		t.Fatalf("versions %d", n)
	}
}

// B2-S3 (REQ-6, abuse): GetRoute is workspace scoped and answers ErrRouteNotFound for any other execution.
func TestB2Routes_GetRouteScopingAndNotFound(t *testing.T) {
	e := newB2Env(t)
	id := e.startTask(t)
	if _, err := e.routes.GetRoute(e.ctx, workspaceB, id); !errors.Is(err, orchestrator.ErrRouteNotFound) {
		t.Fatalf("foreign workspace: %v", err)
	}
	if _, err := e.routes.GetRoute(e.ctx, workspaceA, id+1000); !errors.Is(err, orchestrator.ErrRouteNotFound) {
		t.Fatalf("unknown execution: %v", err)
	}
	plain := e.start(workspaceA, e.enabledVersion(workspaceA, "plain").ID, "plain-1", chainDef("a"))
	if _, err := e.routes.GetRoute(e.ctx, workspaceA, plain.ID); !errors.Is(err, orchestrator.ErrRouteNotFound) {
		t.Fatalf("execution without a route: %v", err)
	}
	if _, err := e.routes.GetRoute(e.ctx, "not-a-uuid", id); !errors.Is(err, behavior.ErrInvalid) {
		t.Fatalf("bad workspace: %v", err)
	}
}

// B2-S4 (REQ-6, abuse): bad input never starts an execution; a workspace that does not run durable workflows is ErrNotAvailable.
func TestB2Routes_StartValidationAndAvailability(t *testing.T) {
	e := newB2Env(t)
	bad := map[string]func(r *orchestrator.TaskRequest){
		"bad repo":        func(r *orchestrator.TaskRequest) { r.Repo = "../etc" },
		"long clickup id": func(r *orchestrator.TaskRequest) { r.ClickUpTaskID = strings.Repeat("a", 65) },
		"long task ref":   func(r *orchestrator.TaskRequest) { r.TaskRef = strings.Repeat("a", 65) },
		"no chat":         func(r *orchestrator.TaskRequest) { r.ChatID = 0 },
		"no update":       func(r *orchestrator.TaskRequest) { r.UpdateID = 0 },
		"bad integration": func(r *orchestrator.TaskRequest) { r.IntegrationID = "x" },
		"bad workspace":   func(r *orchestrator.TaskRequest) { r.WorkspaceID = "x" },
	}
	for name, mod := range bad {
		r := e.req(50)
		mod(&r)
		if _, err := e.routes.StartOrchestratedTask(e.ctx, r); !errors.Is(err, behavior.ErrInvalid) {
			t.Errorf("%s: err %v", name, err)
		}
	}
	if e.count(`SELECT count(*) FROM workflow_executions`) != 0 || e.count(`SELECT count(*) FROM orchestrator_routes`) != 0 {
		t.Fatal("rejected input left rows")
	}
	r := e.req(51)
	r.WorkspaceID = workspaceB
	if _, err := e.routes.StartOrchestratedTask(e.ctx, r); !errors.Is(err, behavior.ErrNotAvailable) {
		t.Fatalf("workspace not on the durable runtime: %v", err)
	}
	if e.count(`SELECT count(*) FROM workflow_executions`) != 0 {
		t.Fatal("unavailable start left an execution")
	}
}

// B2-S5 (REQ-6): table constraints hold below the store: repo and id bounds, execution FK, primary key, RLS.
func TestB2Routes_TableConstraints(t *testing.T) {
	e := newB2Env(t)
	id := e.startTask(t)
	ins := func(exec int64, repo, ref, clickup string) error {
		_, err := e.pool.Exec(e.ctx, `INSERT INTO orchestrator_routes(workspace_id,execution_id,integration_id,chat_id,repo,task_ref,clickup_task_id) VALUES($1,$2,$3,1,$4,$5,$6)`,
			workspaceA, exec, integrationA, repo, ref, clickup)
		return err
	}
	plain := e.start(workspaceA, e.enabledVersion(workspaceA, "plain").ID, "plain-1", chainDef("a"))
	for name, err := range map[string]error{
		"repo too short":      ins(plain.ID, "ab", "", ""),
		"repo too long":       ins(plain.ID, strings.Repeat("r", 201), "", ""),
		"task_ref too long":   ins(plain.ID, "", strings.Repeat("t", 65), ""),
		"clickup id too long": ins(plain.ID, "", "", strings.Repeat("c", 65)),
		"unknown execution":   ins(999999, "", "", ""),
		"duplicate route":     ins(id, "", "", ""),
	} {
		if err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := ins(plain.ID, "", "", ""); err != nil {
		t.Fatalf("empty repo, task and id are valid: %v", err)
	}
	var rls bool
	if err := e.pool.QueryRow(e.ctx, `SELECT relrowsecurity FROM pg_class WHERE oid='public.orchestrator_routes'::regclass`).Scan(&rls); err != nil || !rls {
		t.Fatalf("RLS %v err %v", rls, err)
	}
}

// ---- approvals kind and internal kind (00029) ----

func (e *b2Env) gate(nonce string) int64 {
	e.t.Helper()
	var id int64
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO approvals(workspace_id,kind,nonce,expires_at) VALUES($1,'orchestrator_gate',$2,now()+interval '1 hour') RETURNING id`, workspaceA, nonce).Scan(&id); err != nil {
		e.t.Fatalf("orchestrator_gate approval: %v", err)
	}
	return id
}

// B2-S6 (REQ-7): the orchestrator_gate approval kind and the orchestrated_task internal kind are accepted; unknown ones are not.
func TestB2Migration00029_KindsAccepted(t *testing.T) {
	e := newB2Env(t)
	e.gate("gate-nonce-0000001")
	e.startTask(t)
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO approvals(workspace_id,kind,nonce,expires_at) VALUES($1,'orchestrator_other','gate-nonce-0000002',now()+interval '1 hour')`, workspaceA); err == nil {
		t.Fatal("unknown approval kind accepted")
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO behaviors(workspace_id,name,status,draft,internal_kind) VALUES($1,'internal:orchestrated_other','enabled','{}','orchestrated_other')`, workspaceA); err == nil {
		t.Fatal("unknown internal kind accepted")
	}
}

// seedUsed fills the database the way a used orchestrator does: orchestrated
// executions with plans, tasks, decisions, a worker job, a gate approval with
// wait and signal, and an unrelated execution with an ordinary approval.
func (e *b2Env) seedUsed(t *testing.T) (orch []int64, plain int64, plainApproval int64) {
	t.Helper()
	for i := 0; i < 2; i++ {
		orch = append(orch, e.startTask(t))
	}
	for _, id := range orch {
		if _, err := e.plans.InsertPlan(e.ctx, workspaceA, orchPlan(id, 1), []orchestrator.PlanTaskRecord{orchTask("a"), orchTask("b", "a")}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.plans.InsertDecision(e.ctx, workspaceA, orchestrator.DecisionRecord{ExecutionID: id, PlanVersion: 1, Decision: "COMPLETE"}); err != nil {
			t.Fatal(err)
		}
		mustExec(t, e.ctx, e.pool, `INSERT INTO worker_jobs(workspace_id,execution_id,step_id) VALUES($1,$2,'run')`, workspaceA, id)
	}
	// Gate with wait and signal on the first orchestrated execution.
	var step string
	if err := e.pool.QueryRow(e.ctx, `SELECT step_key FROM execution_steps WHERE execution_id=$1 ORDER BY position LIMIT 1`, orch[0]).Scan(&step); err != nil {
		t.Fatal(err)
	}
	gate := e.gate("gate-nonce-0000010")
	var wait int64
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO workflow_waits(workspace_id,execution_id,step_key,approval_id,wait_kind) VALUES($1,$2,$3,$4,'approval') RETURNING id`,
		workspaceA, orch[0], step, gate).Scan(&wait); err != nil {
		t.Fatalf("wait: %v", err)
	}
	mustExec(t, e.ctx, e.pool, `INSERT INTO workflow_signals(workspace_id,signal_id,execution_id,step_key,wait_id,reference_kind,reference_id,decision) VALUES($1,'sig-1',$2,$3,$4,'approval',$5,'approved')`,
		workspaceA, orch[0], step, wait, gate)
	// An ordinary approval with a wait on a plain execution must survive.
	pl := e.start(workspaceA, e.enabledVersion(workspaceA, "plain").ID, "plain-1", chainDef("a"))
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO approvals(workspace_id,kind,nonce,expires_at) VALUES($1,'confirm_action','plain-nonce-0000001',now()+interval '1 hour') RETURNING id`, workspaceA).Scan(&plainApproval); err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.ctx, e.pool, `INSERT INTO workflow_waits(workspace_id,execution_id,step_key,approval_id,wait_kind) VALUES($1,$2,'a',$3,'approval')`, workspaceA, pl.ID, plainApproval)
	// Workflow-owned tool run (00030) on an orchestrated execution.
	mustExec(t, e.ctx, e.pool, `INSERT INTO tool_runs(workspace_id,workflow_execution_id,step,tool,tier,idempotency_key,status) VALUES($1,$2,1,'clickup_get_task','read','k1','ok')`, workspaceA, orch[0])
	return orch, pl.ID, plainApproval
}

// R-B-5 / B2-S7 (REQ-7): 00029 Down after real use (orchestrated executions, gate approvals with waits and signals, plans, worker jobs, the internal behavior) succeeds, removes only the orchestrator rows and Up works again.
func TestB2Migration00029_DownAfterUseThenUp(t *testing.T) {
	e := newB2Env(t)
	orch, plain, plainApproval := e.seedUsed(t)
	execBefore := e.count(`SELECT count(*) FROM workflow_executions`)
	if execBefore != len(orch)+1 {
		t.Fatalf("executions %d", execBefore)
	}

	// Rollback order: 00030 first (workflow-owned tool runs), then 00029.
	e.migrate(downSection(t, "00030_tool_runs_workflow_owner.sql"))
	e.migrate(downSection(t, "00029_orchestrated_task.sql"))

	for _, id := range orch {
		if n := e.count(`SELECT count(*) FROM workflow_executions WHERE id=$1`, id); n != 0 {
			t.Fatalf("orchestrated execution %d survived Down", id)
		}
		for table, col := range map[string]string{"execution_steps": "execution_id", "execution_events": "execution_id", "orchestrator_plans": "execution_id", "orchestrator_decisions": "execution_id", "worker_jobs": "execution_id"} {
			if n := e.count(`SELECT count(*) FROM `+table+` WHERE `+col+`=$1`, id); n != 0 {
				t.Fatalf("%s rows of execution %d survived Down", table, id)
			}
		}
	}
	if e.count(`SELECT count(*) FROM orchestrator_plan_tasks`) != 0 {
		t.Fatal("plan tasks survived Down")
	}
	if e.count(`SELECT count(*) FROM approvals WHERE kind='orchestrator_gate'`) != 0 ||
		e.count(`SELECT count(*) FROM workflow_signals`) != 0 {
		t.Fatal("gate approvals, waits or signals survived Down")
	}
	if e.count(`SELECT count(*) FROM behaviors WHERE internal_kind='orchestrated_task'`) != 0 {
		t.Fatal("internal behavior survived Down")
	}
	var exists bool
	if err := e.pool.QueryRow(e.ctx, `SELECT to_regclass('public.orchestrator_routes') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("routes table survived Down (err %v)", err)
	}
	// Unrelated data is untouched.
	if e.count(`SELECT count(*) FROM workflow_executions WHERE id=$1`, plain) != 1 ||
		e.count(`SELECT count(*) FROM approvals WHERE id=$1 AND kind='confirm_action'`, plainApproval) != 1 ||
		e.count(`SELECT count(*) FROM workflow_waits WHERE approval_id=$1`, plainApproval) != 1 ||
		e.count(`SELECT count(*) FROM execution_steps WHERE execution_id=$1`, plain) == 0 {
		t.Fatal("Down touched an unrelated execution, approval or wait")
	}
	// The old CHECKs are back.
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO approvals(workspace_id,kind,nonce,expires_at) VALUES($1,'orchestrator_gate','gate-nonce-0000099',now()+interval '1 hour')`, workspaceA); err == nil {
		t.Fatal("old approvals CHECK not restored")
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO behaviors(workspace_id,name,status,draft,internal_kind) VALUES($1,'internal:orchestrated_task','enabled','{}','orchestrated_task')`, workspaceA); err == nil {
		t.Fatal("old internal_kind CHECK not restored")
	}

	// Up again, in migration order, and use the feature once more.
	e.migrate(upSection(t, "00029_orchestrated_task.sql"))
	e.migrate(upSection(t, "00030_tool_runs_workflow_owner.sql"))
	id := e.startTask(t)
	if _, err := e.routes.GetRoute(e.ctx, workspaceA, id); err != nil {
		t.Fatalf("route after Up: %v", err)
	}
	e.gate("gate-nonce-0000100")
	if _, err := e.plans.InsertPlan(e.ctx, workspaceA, orchPlan(id, 1), []orchestrator.PlanTaskRecord{orchTask("a")}); err != nil {
		t.Fatalf("plan after Up: %v", err)
	}
}

// B2-S8 (REQ-7): 00029 Down on a database that never used the feature also succeeds.
func TestB2Migration00029_DownOnEmptyDatabase(t *testing.T) {
	e := newB2Env(t)
	e.migrate(downSection(t, "00030_tool_runs_workflow_owner.sql"))
	e.migrate(downSection(t, "00029_orchestrated_task.sql"))
	e.migrate(upSection(t, "00029_orchestrated_task.sql"))
	e.migrate(upSection(t, "00030_tool_runs_workflow_owner.sql"))
	e.startTask(t)
}

// ---- tool_runs owner (00030) ----

type b2Owners struct {
	job, run, exec int64
}

func (e *b2Env) owners(t *testing.T) b2Owners {
	t.Helper()
	mustExec(t, e.ctx, e.pool, `INSERT INTO integrations(id,workspace_id,platform) VALUES($1,$2,'clickup')`, integrationA, workspaceA)
	mustExec(t, e.ctx, e.pool, `INSERT INTO hub_threads(id,workspace_id,integration_id,thread_type,external_id) VALUES($1,$2,$3,'task','t1')`, threadA, workspaceA, integrationA)
	var o b2Owners
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO agent_jobs(workspace_id,source_integration_id,thread_id,run_at) VALUES($1,$2,$3,now()) RETURNING id`, workspaceA, integrationA, threadA).Scan(&o.job); err != nil {
		t.Fatal(err)
	}
	o.run = e.legacyRun(workspaceA, "legacy", "subject-1").ID
	o.exec = e.startTask(t)
	return o
}

func (e *b2Env) toolRun(job, run, exec any, key string) error {
	_, err := e.pool.Exec(e.ctx, `INSERT INTO tool_runs(workspace_id,agent_job_id,behavior_run_id,workflow_execution_id,step,tool,tier,idempotency_key,status,correlation_id) VALUES($1,$2,$3,$4,1,'t','read',$5,'started',CASE WHEN $3::bigint IS NULL THEN NULL ELSE gen_random_uuid() END)`,
		workspaceA, job, run, exec, key)
	return err
}

// B2-S9 (REQ-13): exactly one of agent job, behavior run and workflow execution owns a tool run, and the execution must exist in the workspace.
func TestB2Migration00030_ThreeOwnerCheckAndFK(t *testing.T) {
	e := newB2Env(t)
	o := e.owners(t)
	for name, err := range map[string]error{
		"agent job only":     e.toolRun(o.job, nil, nil, "k1"),
		"behavior run only":  e.toolRun(nil, o.run, nil, "k2"),
		"workflow exec only": e.toolRun(nil, nil, o.exec, "k3"),
	} {
		if err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
	for name, err := range map[string]error{
		"no owner":              e.toolRun(nil, nil, nil, "k4"),
		"job and run":           e.toolRun(o.job, o.run, nil, "k5"),
		"job and workflow":      e.toolRun(o.job, nil, o.exec, "k6"),
		"run and workflow":      e.toolRun(nil, o.run, o.exec, "k7"),
		"all three":             e.toolRun(o.job, o.run, o.exec, "k8"),
		"unknown execution":     e.toolRun(nil, nil, int64(999999), "k9"),
		"execution of other ws": func() error { return e.toolRunWorkspace(workspaceB, o.exec) }(),
	} {
		if err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if n := e.count(`SELECT count(*) FROM tool_runs`); n != 3 {
		t.Fatalf("rows %d, want the 3 valid ones", n)
	}
}

func (e *b2Env) toolRunWorkspace(ws string, exec int64) error {
	_, err := e.pool.Exec(e.ctx, `INSERT INTO tool_runs(workspace_id,workflow_execution_id,step,tool,tier,idempotency_key,status) VALUES($1,$2,1,'t','read','kx','started')`, ws, exec)
	return err
}

// B2-S10 (REQ-13): 00030 Down deletes workflow-only rows, keeps agent and behavior rows, restores the two-owner CHECK; Up restores the column.
func TestB2Migration00030_DownKeepsOtherOwnersAndUpRestores(t *testing.T) {
	e := newB2Env(t)
	o := e.owners(t)
	for i, args := range [][3]any{{o.job, nil, nil}, {nil, o.run, nil}, {nil, nil, o.exec}, {nil, nil, o.exec}} {
		if err := e.toolRun(args[0], args[1], args[2], string(rune('a'+i))+"-key"); err != nil {
			t.Fatal(err)
		}
	}
	e.migrate(downSection(t, "00030_tool_runs_workflow_owner.sql"))
	if n := e.count(`SELECT count(*) FROM tool_runs`); n != 2 {
		t.Fatalf("rows after Down: %d (workflow rows go, agent and behavior rows stay)", n)
	}
	if e.count(`SELECT count(*) FROM tool_runs WHERE agent_job_id IS NOT NULL`) != 1 || e.count(`SELECT count(*) FROM tool_runs WHERE behavior_run_id IS NOT NULL`) != 1 {
		t.Fatal("Down removed an agent or behavior row")
	}
	var col bool
	if err := e.pool.QueryRow(e.ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='tool_runs' AND column_name='workflow_execution_id')`).Scan(&col); err != nil || col {
		t.Fatalf("column survived Down (err %v)", err)
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO tool_runs(workspace_id,agent_job_id,behavior_run_id,step,tool,tier,idempotency_key,status) VALUES($1,$2,$3,1,'t','read','both','started')`, workspaceA, o.job, o.run); err == nil {
		t.Fatal("two owners accepted after Down")
	}
	e.migrate(upSection(t, "00030_tool_runs_workflow_owner.sql"))
	if err := e.toolRun(nil, nil, o.exec, "after-up"); err != nil {
		t.Fatalf("workflow owner after Up: %v", err)
	}
	if err := e.toolRun(o.job, nil, o.exec, "bad-after-up"); err == nil {
		t.Fatal("two owners accepted after Up")
	}
}

// ---- durable investigate audit (workflow tool run store) ----

// B2-S11 (REQ-13): the workflow store writes rows owned by the execution only, finishes and denies them, keeps no tool output.
func TestB2WorkflowToolRunStore_RowsOwnedByExecution(t *testing.T) {
	e := newB2Env(t)
	exec := e.startTask(t)
	s, err := NewWorkflowToolRunStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	var _ tools.ToolRunStore = s
	start := tools.RunStart{WorkspaceID: workspaceA, JobID: exec, Step: 1, Tool: "clickup_get_task", Tier: tenant.TierRead, Key: "orchestrator:k1",
		InputRedacted: json.RawMessage(`"{\"task_id\":\"86abc\"}"`)}
	id, prior, err := s.Begin(e.ctx, start)
	if err != nil || prior != nil || id == 0 {
		t.Fatalf("begin: %d %+v %v", id, prior, err)
	}
	if err := s.Finish(e.ctx, tools.RunFinish{RunID: id, Status: tools.StatusOK, DurationMS: 4, OutputBytes: 100, Summary: "task read"}); err != nil {
		t.Fatal(err)
	}
	denied := start
	denied.Tool = "github_pr_comment"
	denied.Tier = tenant.TierWrite
	denied.Key = "orchestrator:k2"
	if err := s.Deny(e.ctx, denied, "tool_disabled"); err != nil {
		t.Fatal(err)
	}
	var owned, other int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FILTER (WHERE workflow_execution_id=$1 AND agent_job_id IS NULL AND behavior_run_id IS NULL), count(*) FILTER (WHERE workflow_execution_id IS NULL) FROM tool_runs`, exec).Scan(&owned, &other); err != nil || owned != 2 || other != 0 {
		t.Fatalf("owned %d other %d err %v", owned, other, err)
	}
	var status, summary string
	if err := e.pool.QueryRow(e.ctx, `SELECT status, result_summary FROM tool_runs WHERE id=$1`, id).Scan(&status, &summary); err != nil || status != tools.StatusOK || summary != "task read" {
		t.Fatalf("finished row: %q %q %v", status, summary, err)
	}
	if e.count(`SELECT count(*) FROM tool_runs WHERE status='denied' AND error_code='tool_disabled' AND tool='github_pr_comment'`) != 1 {
		t.Fatal("denial not stored")
	}
	// The workflow audit is reachable through the queries that count writes of the execution.
	writes, dangerous, err := s.JobCounts(e.ctx, exec)
	if err != nil || writes != 0 || dangerous != 0 {
		t.Fatalf("counts %d %d %v", writes, dangerous, err)
	}
}

// B2-S12 (REQ-13, abuse): an execution that does not exist (or is in another workspace) cannot get audit rows, so Begin fails and the executor denies with audit_unavailable.
func TestB2WorkflowToolRunStore_UnknownExecutionFails(t *testing.T) {
	e := newB2Env(t)
	exec := e.startTask(t)
	s, _ := NewWorkflowToolRunStore(e.pool)
	if _, _, err := s.Begin(e.ctx, tools.RunStart{WorkspaceID: workspaceA, JobID: exec + 1000, Step: 1, Tool: "clickup_get_task", Tier: tenant.TierRead, Key: "k"}); err == nil {
		t.Fatal("begin for an unknown execution succeeded")
	}
	if _, _, err := s.Begin(e.ctx, tools.RunStart{WorkspaceID: workspaceB, JobID: exec, Step: 1, Tool: "clickup_get_task", Tier: tenant.TierRead, Key: "k"}); err == nil {
		t.Fatal("begin with another workspace succeeded")
	}
	if n := e.count(`SELECT count(*) FROM tool_runs`); n != 0 {
		t.Fatalf("rows %d", n)
	}
	if _, err := NewWorkflowToolRunStore(nil); err == nil {
		t.Fatal("nil pool accepted")
	}
}

// ---- worker wake query scope ----

// B2-S13 (REQ-9; K1 rulings 1, 2 and 4): the wake query finds a waiting step whose job
// ended in a way that finishes it. SUCCEEDED and CANCELLED wake any waiting step except an
// orchestrator step that waits for an owner gate or answer (gate:, clarify:). FAILED,
// NEEDS_INPUT and INTERRUPTED wake only an orchestrator.run_task step that still waits for
// its worker (wait marker job:<id>); the step that already waits for the owner's Retry or
// Cancel (job_owner:<id>), another step kind, a running execution and a step that is not
// waiting never wake. WAITING_FOR_WORKER executions are woken like the other waits.
func TestB2WakeQuery_FailedAndNeedsInputOnlyForRunTaskSteps(t *testing.T) {
	e := newB2Env(t)
	var device string
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO worker_devices(workspace_id,name,token_hash) VALUES($1,'laptop',decode(repeat('ab',32),'hex')) RETURNING id`, workspaceA).Scan(&device); err != nil {
		t.Fatal(err)
	}
	vid := e.enabledVersion(workspaceA, "wake").ID
	const (
		none  = ``
		job   = `{"references":[{"kind":"orchestrator_wait","reference_id":"job:1"}]}`
		owner = `{"references":[{"kind":"orchestrator_wait","reference_id":"job_owner:1"}]}`
		gate  = `{"references":[{"kind":"orchestrator_wait","reference_id":"gate:7"}]}`
		clar  = `{"references":[{"kind":"orchestrator_wait","reference_id":"clarify:3"}]}`
	)
	type tc struct {
		name, job, stepKind, stepState, execState, checkpoint string
		wake                                                  bool
	}
	cases := []tc{
		{"run_task failed, waits for worker", "FAILED", workflow2RunTask, "WAITING", "WAITING_FOR_WORKER", job, true},
		{"run_task failed, waits for worker, execution WAITING_FOR_USER", "FAILED", workflow2RunTask, "WAITING", "WAITING_FOR_USER", job, true},
		{"run_task needs input, waits for worker", "NEEDS_INPUT", workflow2RunTask, "WAITING", "WAITING_FOR_WORKER", job, true},
		{"run_task interrupted, waits for worker", "INTERRUPTED", workflow2RunTask, "WAITING", "WAITING_FOR_WORKER", job, true},
		{"run_task failed, execution waits for approval", "FAILED", workflow2RunTask, "WAITING", "WAITING_FOR_APPROVAL", job, true},
		{"run_task failed, already waits for the owner", "FAILED", workflow2RunTask, "WAITING", "WAITING_FOR_USER", owner, false},
		{"run_task needs input, already waits for the owner", "NEEDS_INPUT", workflow2RunTask, "WAITING", "WAITING_FOR_USER", owner, false},
		{"run_task interrupted, already waits for the owner", "INTERRUPTED", workflow2RunTask, "WAITING", "WAITING_FOR_USER", owner, false},
		{"run_task failed, no wait marker", "FAILED", workflow2RunTask, "WAITING", "WAITING_FOR_USER", none, false},
		{"run_task failed, waits for a gate", "FAILED", workflow2RunTask, "WAITING", "WAITING_FOR_USER", gate, false},
		{"run_task succeeded, waits for worker", "SUCCEEDED", workflow2RunTask, "WAITING", "WAITING_FOR_WORKER", job, true},
		{"run_task succeeded, no marker", "SUCCEEDED", workflow2RunTask, "WAITING", "WAITING_FOR_USER", none, true},
		{"run_task cancelled, waits for the owner", "CANCELLED", workflow2RunTask, "WAITING", "WAITING_FOR_USER", owner, true},
		{"run_task succeeded, pending gate", "SUCCEEDED", workflow2RunTask, "WAITING", "WAITING_FOR_USER", gate, false},
		{"run_task cancelled, pending gate", "CANCELLED", workflow2RunTask, "WAITING", "WAITING_FOR_USER", gate, false},
		{"run_task succeeded, pending clarification", "SUCCEEDED", workflow2RunTask, "WAITING", "WAITING_FOR_USER", clar, false},
		{"worker.run succeeded", "SUCCEEDED", "worker.run", "WAITING", "WAITING_FOR_USER", none, true},
		{"worker.run cancelled", "CANCELLED", "worker.run", "WAITING", "WAITING_FOR_USER", none, true},
		{"worker.run failed", "FAILED", "worker.run", "WAITING", "WAITING_FOR_USER", none, false},
		{"worker.run failed, job marker", "FAILED", "worker.run", "WAITING", "WAITING_FOR_USER", job, false},
		{"worker.run needs input", "NEEDS_INPUT", "worker.run", "WAITING", "WAITING_FOR_USER", none, false},
		{"model step failed", "FAILED", "model", "WAITING", "WAITING_FOR_USER", job, false},
		{"run_task still running", "RUNNING", workflow2RunTask, "WAITING", "WAITING_FOR_WORKER", job, false},
		{"run_task failed, execution running", "FAILED", workflow2RunTask, "WAITING", "RUNNING", job, false},
		{"run_task failed, step already done", "FAILED", workflow2RunTask, "SUCCEEDED", "WAITING_FOR_USER", job, false},
	}
	want := map[int64]bool{}
	names := map[int64]string{}
	for i, c := range cases {
		ex := e.start(workspaceA, vid, "wake-"+string(rune('a'+i)), chainDef("a"))
		var cp any
		if c.checkpoint != "" {
			cp = c.checkpoint
		}
		mustExec(t, e.ctx, e.pool, `UPDATE execution_steps SET kind=$2, state=$3, checkpoint=$4::jsonb WHERE execution_id=$1 AND step_key='a'`, ex.ID, c.stepKind, c.stepState, cp)
		mustExec(t, e.ctx, e.pool, `UPDATE workflow_executions SET state=$2 WHERE id=$1`, ex.ID, c.execState)
		var dev any
		if c.job != "CANCELLED" {
			dev = device
		}
		mustExec(t, e.ctx, e.pool, `INSERT INTO worker_jobs(workspace_id,execution_id,step_id,state,device_id,attempt) VALUES($1,$2,'a',$3,$4,1)`, workspaceA, ex.ID, c.job, dev)
		want[ex.ID], names[ex.ID] = c.wake, c.name
	}
	rows, err := sqlc.New(e.pool).ListWorkerJobsToWake(e.ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, r := range rows {
		if r.StepID != "a" {
			t.Fatalf("unexpected step %q", r.StepID)
		}
		got[r.ExecutionID.Int64] = true
	}
	var missing, extra []string
	for id, w := range want {
		switch {
		case w && !got[id]:
			missing = append(missing, names[id])
		case !w && got[id]:
			extra = append(extra, names[id])
		}
	}
	slices.Sort(missing)
	slices.Sort(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("wake query: not woken %v, woken but must not be %v", missing, extra)
	}
}

const workflow2RunTask = workflow.StepKindOrchestratorRunTask
