package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Test harness for the orchestrated_task steps (Phase 7b, Batch B1). Every
// port is an in-memory fake; the model is a FakeRuntime keyed by schema.

const shWS = "ws-1"
const shExec int64 = 1

var errShDown = errors.New("fake store down")

// --- plan store ---

type shPlans struct {
	mu        sync.Mutex
	plans     []PlanRecord
	tasks     []PlanTaskRecord
	decisions []DecisionRecord
	nextID    int64
	failDec   bool
	failList  bool
}

func (p *shPlans) InsertPlan(_ context.Context, _ string, plan PlanRecord, tasks []PlanTaskRecord) (PlanRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	plan.ID = p.nextID
	plan.Status = PlanActive
	p.plans = append(p.plans, plan)
	for _, t := range tasks {
		t.PlanID = plan.ID
		p.tasks = append(p.tasks, t)
	}
	return plan, nil
}

func (p *shPlans) SetPlanStatus(_ context.Context, _ string, _ int64, version int, status string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.plans {
		if p.plans[i].Version == version {
			p.plans[i].Status = status
		}
	}
	return nil
}

func (p *shPlans) UpdateTask(_ context.Context, _ string, task PlanTaskRecord) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.tasks {
		if p.tasks[i].PlanID == task.PlanID && p.tasks[i].Key == task.Key {
			p.tasks[i] = task
			return nil
		}
	}
	return errors.New("no such task")
}

func (p *shPlans) InsertDecision(_ context.Context, _ string, d DecisionRecord) (DecisionRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failDec {
		return DecisionRecord{}, errShDown
	}
	p.nextID++
	d.ID = p.nextID
	p.decisions = append(p.decisions, d)
	return d, nil
}

func (p *shPlans) ListPlans(context.Context, string, int64) ([]PlanRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failList {
		return nil, errShDown
	}
	return append([]PlanRecord(nil), p.plans...), nil
}

func (p *shPlans) ListTasks(context.Context, string, int64) ([]PlanTaskRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]PlanTaskRecord(nil), p.tasks...), nil
}

func (p *shPlans) ListDecisions(context.Context, string, int64) ([]DecisionRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]DecisionRecord(nil), p.decisions...), nil
}

func (p *shPlans) task(version int, key string) PlanTaskRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pl := range p.plans {
		if pl.Version != version {
			continue
		}
		for _, t := range p.tasks {
			if t.PlanID == pl.ID && t.Key == key {
				return t
			}
		}
	}
	return PlanTaskRecord{}
}

func (p *shPlans) planStatus(version int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pl := range p.plans {
		if pl.Version == version {
			return pl.Status
		}
	}
	return ""
}

func (p *shPlans) decisionNames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, d := range p.decisions {
		out = append(out, d.Decision)
	}
	return out
}

func (p *shPlans) lastDecision() DecisionRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.decisions) == 0 {
		return DecisionRecord{}
	}
	return p.decisions[len(p.decisions)-1]
}

// --- steps, routes, instructions, repos ---

type shSteps struct {
	mu    sync.Mutex
	steps map[string]workflow.Step
}

func (s *shSteps) ListSteps(context.Context, string, int64) ([]workflow.Step, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]workflow.Step, 0, len(s.steps))
	for _, st := range s.steps {
		out = append(out, st)
	}
	return out, nil
}

type shRoutes struct {
	route Route
	err   error
}

func (r *shRoutes) GetRoute(context.Context, string, int64) (Route, error) {
	return r.route, r.err
}

type shInstr struct {
	mu   sync.Mutex
	list []workflow.Instruction
}

func (i *shInstr) ListInstructions(context.Context, string, int64) ([]workflow.Instruction, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]workflow.Instruction(nil), i.list...), nil
}

func (i *shInstr) add(text string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.list = append(i.list, workflow.Instruction{WorkspaceID: shWS, ExecutionID: shExec, Version: int64(len(i.list) + 1), Text: text})
}

type shRepos struct {
	registered []string
	last       string
}

func (r *shRepos) RegisteredRepos(context.Context, string) ([]string, error) {
	return r.registered, nil
}
func (r *shRepos) LastTaskRepo(context.Context, string, string) (string, error) { return r.last, nil }

// --- worker jobs ---

type shJobs struct {
	mu      sync.Mutex
	byStep  map[string]*WorkerJob
	created []string
	payload map[string]json.RawMessage
	caps    map[string][]string
}

func (j *shJobs) JobForStep(_ context.Context, _ string, _ int64, step string) (WorkerJob, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if job, ok := j.byStep[step]; ok {
		return *job, true, nil
	}
	return WorkerJob{}, false, nil
}

func (j *shJobs) CreateJob(_ context.Context, _ string, _ int64, step string, caps []string, payload json.RawMessage) (WorkerJob, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job := &WorkerJob{ID: fmt.Sprintf("job-%d", len(j.created)+1), State: JobWaiting}
	j.byStep[step] = job
	j.created = append(j.created, step)
	j.payload[step] = payload
	j.caps[step] = caps
	return *job, nil
}

func (j *shJobs) set(step string, fn func(*WorkerJob)) {
	j.mu.Lock()
	defer j.mu.Unlock()
	fn(j.byStep[step])
}

// --- owner ---

type shOwner struct {
	mu       sync.Mutex
	said     []string
	gates    []GateWait
	gateStep []string
	asked    []Gate
	sayErr   error
}

func (o *shOwner) Say(_ context.Context, _ Route, text string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sayErr != nil {
		return o.sayErr
	}
	o.said = append(o.said, text)
	return nil
}

func (o *shOwner) AskGate(_ context.Context, f Fence, _ Route, g Gate) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	id := int64(len(o.gates) + 1)
	o.gates = append(o.gates, GateWait{ApprovalID: id, Purpose: g.Purpose, SignalID: id})
	o.gateStep = append(o.gateStep, f.StepKey)
	o.asked = append(o.asked, g)
	return nil
}

func (o *shOwner) GateWaits(_ context.Context, _ string, _ int64, step string) ([]GateWait, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []GateWait
	for i, g := range o.gates {
		if o.gateStep[i] == step {
			out = append(out, g)
		}
	}
	return out, nil
}

func (o *shOwner) ConsumeGate(_ context.Context, _ Fence, signal int64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i := range o.gates {
		if o.gates[i].SignalID == signal {
			o.gates[i].Consumed = true
		}
	}
	return nil
}

// decide records the owner's decision on the newest gate.
func (o *shOwner) decide(decision string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.gates[len(o.gates)-1].Decision = decision
}

func (o *shOwner) lastSaid() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.said) == 0 {
		return ""
	}
	return o.said[len(o.said)-1]
}

func (o *shOwner) saidCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.said)
}

// --- in-process executor ---

type shExecutor struct {
	mu    sync.Mutex
	calls []TaskRun
	fn    func(TaskRun) (TaskOutcome, error)
}

func (e *shExecutor) RunTask(_ context.Context, run TaskRun) (TaskOutcome, error) {
	e.mu.Lock()
	e.calls = append(e.calls, run)
	e.mu.Unlock()
	if e.fn != nil {
		return e.fn(run)
	}
	return TaskOutcome{Succeeded: true}, nil
}

func (e *shExecutor) keys() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, c := range e.calls {
		out = append(out, c.Task.Key)
	}
	return out
}

// --- harness ---

type shH struct {
	t      *testing.T
	rt     *FakeRuntime
	plans  *shPlans
	steps  *shSteps
	instr  *shInstr
	routes *shRoutes
	repos  *shRepos
	jobs   *shJobs
	owner  *shOwner
	inv    *shExecutor
	s      *Steps
	now    time.Time
}

type shOpt func(*shH)

func shNoJobs(h *shH)  { h.s.Jobs = nil }
func shNoRepos(h *shH) { h.s.Repos = nil }
func shSettings(f func(*StepSettings)) shOpt {
	return func(h *shH) { f(&h.s.Settings) }
}

func newSH(t *testing.T, opts ...shOpt) *shH {
	t.Helper()
	h := &shH{
		t: t, rt: NewFakeRuntime(), plans: &shPlans{}, steps: &shSteps{steps: map[string]workflow.Step{}},
		instr: &shInstr{}, repos: &shRepos{registered: []string{"org/alpha"}}, owner: &shOwner{}, inv: &shExecutor{},
		jobs:   &shJobs{byStep: map[string]*WorkerJob{}, payload: map[string]json.RawMessage{}, caps: map[string][]string{}},
		routes: &shRoutes{route: Route{WorkspaceID: shWS, ExecutionID: shExec, IntegrationID: "tg", ChatID: 5, ThreadID: 9}},
		now:    t0,
	}
	h.instr.add("kiểm tra việc này")
	h.s = &Steps{
		Runtime:  func(context.Context, string) (AIRuntime, error) { return h.rt, nil },
		Template: *NewOrchestrator(nil, "test-model"), Registry: NewCapabilityRegistry(),
		Plans: h.plans, Steps: h.steps, Instructions: h.instr, Routes: h.routes, Repos: h.repos, Jobs: h.jobs,
		Owner: h.owner, Executors: map[string]TaskExecutor{ExecutorInvestigate: h.inv},
		Settings: DefaultStepSettings(), Now: func() time.Time { return h.now },
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

func shKind(key string) (string, int) {
	switch {
	case key == StepKeyPlan:
		return workflow.StepKindOrchestratorPlan, 1
	case key == StepKeyReport:
		return workflow.StepKindOrchestratorReport, 2*OrchestratedTaskSlots + 2
	}
	var i int
	if _, err := fmt.Sscanf(key, "run_%d", &i); err == nil {
		return workflow.StepKindOrchestratorRunTask, 2 * i
	}
	if _, err := fmt.Sscanf(key, "eval_%d", &i); err == nil {
		return workflow.StepKindOrchestratorEvaluate, 2*i + 1
	}
	panic("bad step key " + key)
}

// step executes one attempt of key. A step that waited or failed gets its
// last checkpoint back, like the engine does for a woken or retried step.
func (h *shH) stepAttempt(key string, attempt int) (workflow.StepResult, error) {
	h.t.Helper()
	kind, pos := shKind(key)
	h.steps.mu.Lock()
	prev := h.steps.steps[key]
	h.steps.mu.Unlock()
	var cp *workflow.Checkpoint
	if prev.State != workflow.StepSucceeded {
		cp = prev.Checkpoint
	}
	sc := workflow.StepContext{
		Execution:  workflow.Execution{ID: shExec, WorkspaceID: shWS, CreatedAt: t0},
		Definition: OrchestratedTaskDefinition(),
		Step:       workflow.Step{ID: int64(pos), ExecutionID: shExec, Key: key, Position: pos, Kind: kind},
		ClaimToken: "claim", JobID: 7, Attempt: attempt, Checkpoint: cp,
	}
	res, err := stepKind{s: h.s, kind: kind}.Execute(context.Background(), sc)
	st := workflow.Step{ID: int64(pos), ExecutionID: shExec, Key: key, Position: pos, Kind: kind, Checkpoint: res.Checkpoint}
	switch {
	case err != nil:
		st.State = workflow.StepFailed
	case res.WaitState != "":
		st.State = workflow.StepWaiting
	default:
		st.State = workflow.StepSucceeded
	}
	if res.Checkpoint != nil || err == nil {
		h.steps.mu.Lock()
		h.steps.steps[key] = st
		h.steps.mu.Unlock()
	}
	return res, err
}

func (h *shH) step(key string) (workflow.StepResult, error) {
	h.t.Helper()
	return h.stepAttempt(key, 1)
}

func (h *shH) must(key string) workflow.StepResult {
	h.t.Helper()
	res, err := h.step(key)
	if err != nil || res.WaitState != "" {
		h.t.Fatalf("step %s: wait=%q err=%v", key, res.WaitState, err)
	}
	return res
}

// flow runs plan, run/eval slots and report in order, up to and including
// the first step that waits or fails. It returns that step's key.
func (h *shH) flow() (string, workflow.StepResult, error) {
	h.t.Helper()
	order := []string{StepKeyPlan}
	for i := 1; i <= OrchestratedTaskSlots; i++ {
		order = append(order, RunStepKey(i), EvalStepKey(i))
	}
	order = append(order, StepKeyReport)
	for _, key := range order {
		res, err := h.step(key)
		if err != nil || res.WaitState != "" {
			return key, res, err
		}
	}
	return "", workflow.StepResult{}, nil
}

// --- scripting helpers ---

func (h *shH) plan(decision string, tasks ...map[string]any) {
	h.rt.Script("plan", reply(h.t, planJSON(decision, tasks...), 40, 20))
}

func (h *shH) eval(decision, reason string, extra ...any) {
	v := m("decision", decision, "reason_code", reason)
	for i := 0; i+1 < len(extra); i += 2 {
		v[extra[i].(string)] = extra[i+1]
	}
	h.rt.Script("evaluate", reply(h.t, v, 10, 5))
}

func (h *shH) calls(schema string) int {
	n := 0
	for _, r := range h.rt.Requests() {
		if r.SchemaName == schema {
			n++
		}
	}
	return n
}

func shRead(key, title string, deps ...string) map[string]any {
	return ptask(key, title, []string{CapRepoRead}, deps...)
}

func shWrite(key, title string, deps ...string) map[string]any {
	return ptask(key, title, []string{CapRepoWrite}, deps...)
}

func shStepErrCode(t *testing.T, err error) (workflow.ReasonCode, bool) {
	t.Helper()
	var se *workflow.StepError
	if !errors.As(err, &se) {
		t.Fatalf("not a StepError: %v", err)
	}
	return se.Code, se.Retryable
}

func shBudget(t *testing.T, res workflow.StepResult) BudgetSnapshot {
	t.Helper()
	if res.Checkpoint == nil {
		t.Fatal("no checkpoint")
	}
	snap, ok := decodeSnapshot(refValue(res.Checkpoint, CheckpointBudget))
	if !ok {
		t.Fatal("no budget snapshot in checkpoint")
	}
	return snap
}

func shHas(t *testing.T, s string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(s, p) {
			t.Fatalf("missing %q in:\n%s", p, s)
		}
	}
}
