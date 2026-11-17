package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Phase 7a Fix Brief B3 (REQ-4 topic side, 8, 11, 18, 19): /task, the
// local-task step, the topic notifier, Retry and the /worker card. Fakes only.

var b3Now = time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)

// ---- fakes ----

type b3Starter struct {
	mu    sync.Mutex
	repos []string
	reqs  []LocalTaskRequest
	err   error
}

func (s *b3Starter) StartLocalTask(_ context.Context, r LocalTaskRequest) (workflow.Execution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return workflow.Execution{}, s.err
	}
	s.reqs = append(s.reqs, r)
	return workflow.Execution{ID: 41, WorkspaceID: r.WorkspaceID}, nil
}
func (s *b3Starter) RegisteredRepos(context.Context, string) ([]string, error) {
	return s.repos, nil
}

// b3Links resolves custom task ids and counts the calls.
type b3Links struct {
	mu    sync.Mutex
	calls []string
}

func (l *b3Links) TaskTopicLink(context.Context, ChatRef, string) (string, error) {
	return "", ErrTopicUnavailable
}
func (l *b3Links) ExistingTopicLink(context.Context, ChatRef, string, string) (string, bool) {
	return "", false
}
func (l *b3Links) ResolveTask(_ context.Context, _ string, ref TaskRef) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, ref.ID)
	return "86resolved", nil
}

type b3Routes struct {
	mu       sync.Mutex
	routes   map[int64]*LocalTaskRoute
	versions []int64
}

func newB3Routes() *b3Routes {
	return &b3Routes{routes: map[int64]*LocalTaskRoute{41: {
		WorkspaceID: p4WS, ExecutionID: 41, IntegrationID: b1Bot, ChatID: b1Chat,
		Repo: "o/r", TaskRef: "FC-1", ClickUpTaskID: "86abc",
	}}}
}
func (r *b3Routes) GetRoute(_ context.Context, ws string, id int64) (LocalTaskRoute, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rt, ok := r.routes[id]
	if !ok || rt.WorkspaceID != ws {
		return LocalTaskRoute{}, ErrNotFound
	}
	return *rt, nil
}
func (r *b3Routes) SetProgressMessage(_ context.Context, _ string, id, msg int64, attempt int, thread int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[id].ProgressMessageID, r.routes[id].ProgressAttempt, r.routes[id].ThreadID = msg, attempt, thread
	return nil
}
func (r *b3Routes) SetWaitingNotice(_ context.Context, _ string, id int64, notice string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[id].WaitingNotice = notice
	return nil
}
func (r *b3Routes) SetInstructionVersion(_ context.Context, _ string, id, v int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[id].InstructionVersion = v
	r.versions = append(r.versions, v)
	return nil
}
func (r *b3Routes) route(id int64) LocalTaskRoute {
	r.mu.Lock()
	defer r.mu.Unlock()
	return *r.routes[id]
}

// b3Msgr is a Messenger that can also edit messages.
type b3Msgr struct {
	mu      sync.Mutex
	sent    []OutMessage
	edits   []b3Edit
	tries   int
	editErr error
	next    int64
	removed []int64
	toasts  []string
}
type b3Edit struct {
	id   int64
	text string
}

func (m *b3Msgr) SendMessage(_ context.Context, _ ChatRef, o OutMessage) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	m.sent = append(m.sent, o)
	return 700 + m.next, nil
}
func (m *b3Msgr) EditMessageText(_ context.Context, _ ChatRef, id int64, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tries++
	if m.editErr != nil {
		return m.editErr
	}
	m.edits = append(m.edits, b3Edit{id, text})
	return nil
}
func (m *b3Msgr) RemoveButtons(_ context.Context, _ ChatRef, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, id)
	return nil
}
func (m *b3Msgr) AnswerCallback(_ context.Context, _ ChatRef, _, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.toasts = append(m.toasts, text)
	return nil
}
func (m *b3Msgr) texts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.sent))
	for i, s := range m.sent {
		out[i] = s.Text
	}
	return out
}

type b3Waker struct {
	mu    sync.Mutex
	calls []string
}

func (w *b3Waker) WakeWorkerStep(_ context.Context, ws string, id int64, step string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, ws+"/"+step)
	return nil
}

type b3Telemetry struct{ recs []WorkerTelemetryRecord }

func (t *b3Telemetry) ListTelemetry(context.Context, string) ([]WorkerTelemetryRecord, error) {
	return t.recs, nil
}

type b3Queued struct {
	mu   sync.Mutex
	jobs []WorkerJob
}

func (q *b3Queued) WorkerJobQueued(_ context.Context, j WorkerJob) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.jobs = append(q.jobs, j)
}

// b3Jobs is the owner's job control (Retry, Cancel, /retry).
type b3Jobs struct {
	mu        sync.Mutex
	job       WorkerJob
	found     bool
	retryErr  error
	cancelErr error
	retries   []int
	cancels   int
}

func (j *b3Jobs) CancelJob(context.Context, string, string) (WorkerJob, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.cancels++
	return j.job, j.cancelErr
}
func (j *b3Jobs) GetJob(context.Context, string, string) (WorkerJob, error) {
	return j.job, nil
}
func (j *b3Jobs) JobForStep(context.Context, string, int64, string) (WorkerJob, bool, error) {
	return j.job, j.found, nil
}
func (j *b3Jobs) RetryJob(_ context.Context, _, _ string, attempt int) (WorkerJob, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.retries = append(j.retries, attempt)
	if j.retryErr != nil {
		return WorkerJob{}, j.retryErr
	}
	out := j.job
	out.State, out.Attempt = WorkerJobWaiting, attempt+1
	return out, nil
}

func b3Job(state string) WorkerJob {
	return WorkerJob{ID: "job-1", WorkspaceID: p4WS, ExecutionID: 41, StepID: StepKeyWorkerRun, Attempt: 1,
		RequiredCapabilities: []string{"repo.write"}, State: state}
}

// ---- /task ----

type b3TaskEnv struct {
	w     *InboxWorker
	m     *p4Messenger
	st    *b3Starter
	conv  *phase6BConversations
	links *b3Links
}

func b3TaskWorker(repos ...string) *b3TaskEnv {
	e := &b3TaskEnv{m: &p4Messenger{}, st: &b3Starter{repos: repos}, conv: &phase6BConversations{}, links: &b3Links{}}
	e.w = NewInboxWorker(InboxDeps{
		Messenger: e.m, Conversations: e.conv, LocalTasks: e.st, Links: e.links,
		Topics: &b1Topics{rows: []Topic{b1TaskTopic(b1Task, b1Thread)}},
	}, InboxSettings{})
	e.w.now = func() time.Time { return b3Now }
	return e
}

func (e *b3TaskEnv) task(args string, thread int64) outcome {
	return e.w.dispatch(context.Background(), InboundUpdate{
		WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: b1Chat, ThreadID: thread, UpdateID: 9001, MessageID: 31,
		Kind: UpdateCommand, Command: "/task", Args: args,
	}, "corr")
}

func (e *b3TaskEnv) started() []LocalTaskRequest {
	e.st.mu.Lock()
	defer e.st.mu.Unlock()
	return append([]LocalTaskRequest(nil), e.st.reqs...)
}

// B3-T1 (REQ-4): /task inside a task topic starts a local task for the
// topic's task, answers with the WF reference, and stores the text as a
// Phase 6 instruction.
func TestB3Task_InTopicStartsLocalTask(t *testing.T) {
	e := b3TaskWorker("o/r")
	out := e.task("fix the login bug", b1Thread)
	if out.code != "" {
		t.Fatalf("code %q, replies %v", out.code, e.m.texts())
	}
	reqs := e.started()
	if len(reqs) != 1 || reqs[0].ClickUpTaskID != b1Task || reqs[0].Repo != "o/r" || reqs[0].UpdateID != 9001 {
		t.Fatalf("start request %+v", reqs)
	}
	if got := e.m.texts(); len(got) != 1 || !strings.HasPrefix(got[0], "WF-41: ") || strings.Contains(got[0], "fix the login bug") {
		t.Fatalf("reply %v", got)
	}
	ins := e.conv.instructions[41]
	if len(ins) != 1 || ins[0].Text != "fix the login bug" || ins[0].TelegramUpdateID != 9001 {
		t.Fatalf("instruction row %+v", ins)
	}
	if len(e.conv.resources) != 1 || e.conv.resources[0].Kind != "telegram.message" {
		t.Fatalf("message not bound as resource: %+v", e.conv.resources)
	}
}

// B3-T2 (REQ-4, REQ-19): the instruction never enters workflow JSON: the
// start request, the definition and the step result carry no text.
func TestB3Task_InstructionNeverInWorkflowJSON(t *testing.T) {
	const secretText = "rotate the zebra-unicorn credentials"
	e := b3TaskWorker("o/r")
	e.task(secretText, b1Thread)
	req := e.started()[0]
	raw, _ := json.Marshal(LocalTaskStartRequest(req, 7, "corr", b3Now))
	if strings.Contains(string(raw), "zebra-unicorn") {
		t.Fatalf("instruction text in the workflow start request: %s", raw)
	}
	snap, _ := json.Marshal(NewLocalTaskSnapshot())
	if strings.Contains(string(snap), "zebra-unicorn") {
		t.Fatal("instruction text in the local task snapshot")
	}
	if got := e.conv.instructions[41]; len(got) != 1 || got[0].Text != secretText {
		t.Fatalf("instruction not stored as a Phase 6 instruction: %+v", got)
	}
	// The start trigger is per inbound update: a redelivery cannot start twice.
	if a, b := req.TriggerKey(), req.TriggerKey(); a != b || !strings.Contains(a, "9001") {
		t.Fatalf("trigger key %q", a)
	}
}

// B3-T3 (REQ-4): outside a topic /task needs a task id; a custom id is
// resolved through the task resolver.
func TestB3Task_OutsideTopicNeedsAndResolvesTaskID(t *testing.T) {
	e := b3TaskWorker("o/r")
	out := e.task("FC-6583 update the docs", 0)
	if out.code != "" {
		t.Fatalf("code %q: %v", out.code, e.m.texts())
	}
	reqs := e.started()
	if len(reqs) != 1 || reqs[0].TaskRef != "FC-6583" || reqs[0].ClickUpTaskID != "86resolved" {
		t.Fatalf("custom id not resolved: %+v", reqs)
	}
	if len(e.links.calls) != 1 || e.links.calls[0] != "FC-6583" {
		t.Fatalf("resolver calls %v", e.links.calls)
	}
	if got := e.conv.instructions[41]; len(got) != 1 || got[0].Text != "update the docs" {
		t.Fatalf("instruction %+v", got)
	}
	// A plain ClickUp id needs no resolver call.
	e2 := b3TaskWorker("o/r")
	e2.task("86plain do it", 0)
	if r := e2.started(); len(r) != 1 || r[0].ClickUpTaskID != "86plain" || len(e2.links.calls) != 0 {
		t.Fatalf("plain id: %+v calls %v", r, e2.links.calls)
	}
}

// B3-T4 (REQ-4): no id outside a topic, a foreign link, or no text -> usage;
// nothing is started.
func TestB3Task_UsageWhenNoTaskOrText(t *testing.T) {
	for name, args := range map[string]string{
		"empty":        "",
		"foreign link": "https://evil.example/t/86abc do it",
	} {
		e := b3TaskWorker("o/r")
		out := e.task(args, 0)
		if out.code != "usage" || len(e.started()) != 0 {
			t.Fatalf("%s: code %q started %v replies %v", name, out.code, e.started(), e.m.texts())
		}
		if got := e.m.texts(); len(got) != 1 || !strings.Contains(got[0], "/task") {
			t.Fatalf("%s: reply %v", name, got)
		}
	}
	// A task id without text is refused; the topic with no text too.
	e := b3TaskWorker("o/r")
	if out := e.task("FC-1", 0); out.code != "usage" || len(e.started()) != 0 {
		t.Fatalf("id without text: %q %v", out.code, e.started())
	}
	if out := e.task("   ", b1Thread); out.code != "usage" || len(e.started()) != 0 {
		t.Fatalf("topic without text: %q %v", out.code, e.started())
	}
}

// B3-T5 (REQ-4): instruction bounds and cleaning.
func TestB3Task_InstructionBoundsAndCleaning(t *testing.T) {
	e := b3TaskWorker("o/r")
	if out := e.task(strings.Repeat("é", 2001), b1Thread); out.code != "usage" || len(e.started()) != 0 {
		t.Fatalf("2001 runes accepted: %q", out.code)
	}
	if out := e.task(strings.Repeat("é", 2000), b1Thread); out.code != "" || len(e.started()) != 1 {
		t.Fatalf("2000 runes refused: %q", out.code)
	}
	e2 := b3TaskWorker("o/r")
	e2.task("a\x07b\x00c", b1Thread)
	if got := e2.conv.instructions[41]; len(got) != 1 || got[0].Text != "abc" {
		t.Fatalf("control characters kept: %+v", got)
	}
}

// B3-T6 (REQ-4): repository choice. One registered repo is automatic; several
// need owner/repo first; none gives an explanation.
func TestB3Task_RepoChoice(t *testing.T) {
	one := b3TaskWorker("acme/api")
	one.task("do it", b1Thread)
	if r := one.started(); len(r) != 1 || r[0].Repo != "acme/api" {
		t.Fatalf("one repo: %+v", r)
	}

	many := b3TaskWorker("acme/api", "acme/web")
	if out := many.task("do it", b1Thread); out.code != "usage" || len(many.started()) != 0 {
		t.Fatalf("several repos without a choice started: %q %v", out.code, many.started())
	}
	if got := many.m.texts(); len(got) != 1 || !strings.Contains(got[0], "acme/api") || !strings.Contains(got[0], "acme/web") {
		t.Fatalf("choice not listed: %v", got)
	}
	if out := many.task("acme/unknown do it", b1Thread); out.code != "usage" || len(many.started()) != 0 {
		t.Fatalf("unregistered repo accepted: %q", out.code)
	}
	many.task("ACME/Web add a test", b1Thread)
	r := many.started()
	if len(r) != 1 || r[0].Repo != "acme/web" {
		t.Fatalf("explicit repo: %+v", r)
	}
	if got := many.conv.instructions[41]; len(got) != 1 || got[0].Text != "add a test" {
		t.Fatalf("repo word must not be part of the instruction: %+v", got)
	}

	none := b3TaskWorker()
	if out := none.task("do it", b1Thread); out.code != "no_repo" || len(none.started()) != 0 {
		t.Fatalf("no repo: %q", out.code)
	}
	if got := none.m.texts(); len(got) != 1 || !strings.Contains(got[0], "github-repo") {
		t.Fatalf("no explanation: %v", got)
	}
}

// B3-T7 (REQ-4): /task without the durable runtime, and with a failing
// instruction store.
func TestB3Task_UnavailableAndStorageFailure(t *testing.T) {
	m := &p4Messenger{}
	w := NewInboxWorker(InboxDeps{Messenger: m}, InboxSettings{})
	out := w.dispatch(context.Background(), InboundUpdate{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: b1Chat,
		Kind: UpdateCommand, Command: "/task", Args: "FC-1 do"}, "c")
	if out.code != "not_available" {
		t.Fatalf("code %q", out.code)
	}
	e := b3TaskWorker("o/r")
	e.st.err = ErrNotAvailable
	if out := e.task("do it", b1Thread); out.code != "not_available" {
		t.Fatalf("starter unavailable: %q", out.code)
	}
	e2 := b3TaskWorker("o/r")
	e2.conv.appendErr = errors.New("boom")
	if out := e2.task("do it", b1Thread); out.code != "storage" {
		t.Fatalf("instruction store failure: %q", out.code)
	}
	for _, s := range e2.m.texts() {
		if strings.Contains(s, "boom") {
			t.Fatalf("error text leaked: %q", s)
		}
	}
}

// ---- local-task step ----

type b3StepJobs struct {
	mu    sync.Mutex
	jobs  []WorkerJob
	made  int
	caps  []string
	pay   json.RawMessage
	state string
}

func (s *b3StepJobs) CreateJob(_ context.Context, ws string, id int64, step string, caps []string, payload json.RawMessage) (WorkerJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.made++
	s.caps, s.pay = caps, payload
	j := WorkerJob{ID: "job-1", WorkspaceID: ws, ExecutionID: id, StepID: step, State: WorkerJobWaiting, Attempt: 1, Payload: payload}
	s.jobs = append(s.jobs, j)
	return j, nil
}
func (s *b3StepJobs) JobForStep(context.Context, string, int64, string) (WorkerJob, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.jobs) == 0 {
		return WorkerJob{}, false, nil
	}
	j := s.jobs[len(s.jobs)-1]
	if s.state != "" {
		j.State = s.state
	}
	return j, true, nil
}

func b3Step(t *testing.T, instructions ...string) (*WorkerRunStep, *b3StepJobs, *b3Routes, *b3Queued, workflow.StepContext) {
	t.Helper()
	conv := &phase6BConversations{}
	for i, text := range instructions {
		if _, err := conv.AppendInstruction(context.Background(), workflow.InstructionRequest{WorkspaceID: p4WS, ExecutionID: 41, Text: text, TelegramUpdateID: int64(100 + i)}); err != nil {
			t.Fatal(err)
		}
	}
	jobs, routes, queued := &b3StepJobs{}, newB3Routes(), &b3Queued{}
	step := &WorkerRunStep{Jobs: jobs, Routes: routes, Instructions: conv, Queued: queued}
	sc := workflow.StepContext{Execution: workflow.Execution{ID: 41, WorkspaceID: p4WS}, Step: workflow.Step{Key: StepKeyWorkerRun}}
	return step, jobs, routes, queued, sc
}

func b3StepErr(t *testing.T, err error) *workflow.StepError {
	t.Helper()
	var se *workflow.StepError
	if !errors.As(err, &se) {
		t.Fatalf("want StepError, got %v", err)
	}
	return se
}

// B3-S1 (REQ-4): the step creates one job with the capabilities, the
// instruction and the task, tells the topic once, and waits.
func TestB3Step_CreatesOneJobAndWaits(t *testing.T) {
	step, jobs, routes, queued, sc := b3Step(t, "first part", "second part")
	res, err := step.Execute(context.Background(), sc)
	if err != nil || res.WaitState != workflow.StateWaitingForUser || len(res.WaitReferences) != 1 ||
		res.WaitReferences[0].Kind != CheckpointWorkerJob || res.WaitReferences[0].ReferenceID != "job-1" {
		t.Fatalf("result %+v err %v", res, err)
	}
	if jobs.made != 1 || len(jobs.caps) != 1 || jobs.caps[0] != "repo.write" {
		t.Fatalf("job made %d caps %v", jobs.made, jobs.caps)
	}
	var p localJobPayload
	if err := json.Unmarshal(jobs.pay, &p); err != nil {
		t.Fatal(err)
	}
	if p.Repo != "o/r" || p.TaskID != "FC-1" || p.ClickUpTaskID != "86abc" ||
		p.Instruction != "first part\n\nsecond part" || p.InstructionVersion != 2 {
		t.Fatalf("payload %+v", p)
	}
	if routes.route(41).InstructionVersion != 2 {
		t.Fatalf("route instruction version %d", routes.route(41).InstructionVersion)
	}
	// A second run follows the same job: no new job, no second topic notice.
	if _, err := step.Execute(context.Background(), sc); err != nil {
		t.Fatal(err)
	}
	if jobs.made != 1 || len(queued.jobs) != 1 {
		t.Fatalf("second run created %d jobs, queued %d times", jobs.made, len(queued.jobs))
	}
}

// B3-S2 (REQ-4): job state decides the step: SUCCEEDED completes it,
// CANCELLED fails it with worker_cancelled, every other state keeps waiting.
func TestB3Step_JobStatesDecideTheStep(t *testing.T) {
	step, jobs, _, _, sc := b3Step(t, "do it")
	if _, err := step.Execute(context.Background(), sc); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{WorkerJobWaiting, WorkerJobLeased, WorkerJobRunning, WorkerJobCancelling,
		WorkerJobFailed, WorkerJobNeedsInput, WorkerJobInterrupted} {
		jobs.state = state
		res, err := step.Execute(context.Background(), sc)
		if err != nil || res.WaitState != workflow.StateWaitingForUser || res.Checkpoint != nil {
			t.Fatalf("%s: step must keep waiting: %+v %v", state, res, err)
		}
	}
	jobs.state = WorkerJobSucceeded
	res, err := step.Execute(context.Background(), sc)
	if err != nil || res.WaitState != "" || res.Checkpoint == nil || len(res.Checkpoint.References) != 1 ||
		res.Checkpoint.References[0].ReferenceID != "job-1" {
		t.Fatalf("succeeded: %+v %v", res, err)
	}
	raw, _ := json.Marshal(res)
	if strings.Contains(string(raw), "do it") {
		t.Fatalf("instruction in the step checkpoint: %s", raw)
	}
	jobs.state = WorkerJobCancelled
	_, err = step.Execute(context.Background(), sc)
	if se := b3StepErr(t, err); string(se.Code) != CodeWorkerCancelled || se.Retryable {
		t.Fatalf("cancelled: %+v", se)
	}
	if jobs.made != 1 {
		t.Fatalf("jobs made %d", jobs.made)
	}
}

// B3-S3 (REQ-4): no instruction yet -> retry later; no route -> fail.
func TestB3Step_PendingInstructionAndMissingRoute(t *testing.T) {
	step, jobs, _, _, sc := b3Step(t)
	_, err := step.Execute(context.Background(), sc)
	if se := b3StepErr(t, err); string(se.Code) != CodeInstructionPending || !se.Retryable || jobs.made != 0 {
		t.Fatalf("pending: %+v made %d", se, jobs.made)
	}
	step2, jobs2, routes2, _, sc2 := b3Step(t, "do it")
	delete(routes2.routes, 41)
	_, err = step2.Execute(context.Background(), sc2)
	if se := b3StepErr(t, err); string(se.Code) != CodeLocalTaskRoute || se.Retryable || jobs2.made != 0 {
		t.Fatalf("no route: %+v made %d", se, jobs2.made)
	}
}

// ---- follow-up instructions ----

func b3Instr(t *testing.T, routeVersion int64, texts ...string) (*LocalTaskInstructions, *b3Routes) {
	t.Helper()
	conv := &phase6BConversations{}
	for i, text := range texts {
		if _, err := conv.AppendInstruction(context.Background(), workflow.InstructionRequest{WorkspaceID: p4WS, ExecutionID: 41, Text: text, TelegramUpdateID: int64(200 + i)}); err != nil {
			t.Fatal(err)
		}
	}
	routes := newB3Routes()
	routes.routes[41].InstructionVersion = routeVersion
	return &LocalTaskInstructions{Routes: routes, Reader: conv}, routes
}

func b3IDs(list []WorkerInstruction) string {
	var ids []string
	for _, i := range list {
		ids = append(ids, i.ID)
	}
	return strings.Join(ids, ",")
}

// B3-F1 (REQ-4, REQ-11): follow-ups newer than the delivered version come
// back with every heartbeat until acknowledged, oldest first.
func TestB3Followup_ResentUntilAcked(t *testing.T) {
	src, routes := b3Instr(t, 1, "initial", "second", "third")
	job := b3Job(WorkerJobRunning)
	for i := 0; i < 3; i++ {
		got, err := src.PendingWorkerInstructions(context.Background(), job)
		if err != nil || b3IDs(got) != "v2,v3" || got[0].Text != "second" {
			t.Fatalf("poll %d: %+v %v", i, got, err)
		}
	}
	if err := src.AckWorkerInstructions(context.Background(), job, []string{"v2"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := src.PendingWorkerInstructions(context.Background(), job); b3IDs(got) != "v3" {
		t.Fatalf("after ack v2: %+v", got)
	}
	// Unknown, malformed and bare-number ids move nothing.
	_ = src.AckWorkerInstructions(context.Background(), job, []string{"v99", "x3", "3", "", "v-1"})
	if routes.route(41).InstructionVersion != 2 {
		t.Fatalf("junk ack moved the version to %d", routes.route(41).InstructionVersion)
	}
	if err := src.AckWorkerInstructions(context.Background(), job, []string{"v3", "v2"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := src.PendingWorkerInstructions(context.Background(), job); len(got) != 0 {
		t.Fatalf("acked instructions still pending: %+v", got)
	}
}

// B3-F2 (REQ-4): a job with no local-task route has no instructions.
func TestB3Followup_NoRouteNoInstructions(t *testing.T) {
	src, routes := b3Instr(t, 0, "a")
	delete(routes.routes, 41)
	if got, err := src.PendingWorkerInstructions(context.Background(), b3Job(WorkerJobRunning)); err != nil || len(got) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
	if err := src.AckWorkerInstructions(context.Background(), b3Job(WorkerJobRunning), []string{"v1"}); err != nil {
		t.Fatal(err)
	}
	var nilSrc *LocalTaskInstructions
	if got, err := nilSrc.PendingWorkerInstructions(context.Background(), b3Job(WorkerJobRunning)); err != nil || got != nil {
		t.Fatalf("nil source: %+v %v", got, err)
	}
}

// ---- WorkerNotifier ----

type b3NotifierEnv struct {
	n      *WorkerNotifier
	m      *b3Msgr
	routes *b3Routes
	ap     *p4Approvals
	waker  *b3Waker
	clock  *time.Time
}

func b3Notifier(admin WorkerAdmin, tel WorkerTelemetryReader) *b3NotifierEnv {
	now := b3Now
	e := &b3NotifierEnv{m: &b3Msgr{}, routes: newB3Routes(), ap: newP4Approvals(), waker: &b3Waker{}, clock: &now}
	e.n = NewWorkerNotifier(WorkerNotifierDeps{
		Routes: e.routes, Messenger: e.m, Approvals: e.ap, Devices: admin, Telemetry: tel,
		Thresholds: DefaultWorkerThresholds, Waker: e.waker,
	})
	e.n.now = func() time.Time { return *e.clock }
	return e
}

func (e *b3NotifierEnv) drain() {
	for {
		select {
		case ev := <-e.n.queue:
			e.n.handle(context.Background(), ev)
		default:
			return
		}
	}
}

func (e *b3NotifierEnv) change(kind string, job WorkerJob) {
	e.n.WorkerJobChanged(context.Background(), kind, job)
	e.drain()
}

func b3Device(name string, seen time.Time, caps ...string) WorkerDevice {
	return WorkerDevice{ID: "dev-" + name, WorkspaceID: p4WS, Name: name, Capabilities: caps, LastSeenAt: &seen, CreatedAt: seen}
}

// B3-N1 (REQ-8): with no device online the topic hears "waiting for a local
// machine" exactly once; a lease clears it, so a later wait says it again.
func TestB3Notifier_WaitingOfflineOnce(t *testing.T) {
	e := b3Notifier(&fakeWorkerAdmin{}, nil)
	job := b3Job(WorkerJobWaiting)
	e.n.WorkerJobQueued(context.Background(), job)
	e.drain()
	e.n.WorkerJobQueued(context.Background(), job)
	e.drain()
	got := e.m.texts()
	if len(got) != 1 || !strings.Contains(got[0], "Đang chờ máy local online") || !strings.HasPrefix(got[0], "WF-41: ") {
		t.Fatalf("messages %v", got)
	}
	if e.routes.route(41).WaitingNotice != workerNoticeOffline {
		t.Fatalf("notice %q", e.routes.route(41).WaitingNotice)
	}
	// Taken by a device: the notice is cleared; a later wait notifies again.
	leased := b3Job(WorkerJobLeased)
	leased.DeviceID = "dev-x"
	e.change(WorkerChangeLeased, leased)
	if e.routes.route(41).WaitingNotice != "" {
		t.Fatalf("notice not cleared: %q", e.routes.route(41).WaitingNotice)
	}
	e.n.WorkerJobQueued(context.Background(), job)
	e.drain()
	if n := len(e.m.texts()); n != 3 {
		t.Fatalf("messages %v", e.m.texts())
	}
}

// B3-N2 (REQ-8): a free online device that covers the capabilities takes the
// job at its next poll: no waiting text. A device without the capability does
// not count as online for this job.
func TestB3Notifier_NoNoticeWhenFreeDeviceOnline(t *testing.T) {
	admin := &fakeWorkerAdmin{devices: []WorkerDevice{b3Device("laptop", b3Now.Add(-10*time.Second), "repo.write")}}
	e := b3Notifier(admin, nil)
	e.n.WorkerJobQueued(context.Background(), b3Job(WorkerJobWaiting))
	e.drain()
	if got := e.m.texts(); len(got) != 0 {
		t.Fatalf("notice with a free device online: %v", got)
	}
	admin2 := &fakeWorkerAdmin{devices: []WorkerDevice{b3Device("reader", b3Now.Add(-10*time.Second), "repo.read")}}
	e2 := b3Notifier(admin2, nil)
	e2.n.WorkerJobQueued(context.Background(), b3Job(WorkerJobWaiting))
	e2.drain()
	if got := e2.m.texts(); len(got) != 1 || !strings.Contains(got[0], "Đang chờ máy local online") {
		t.Fatalf("incapable device counted as online: %v", got)
	}
}

// B3-N3 (REQ-8): a device blocked by the Claude limit shows the reset time,
// once.
func TestB3Notifier_ThresholdBlockShowsResetTime(t *testing.T) {
	admin := &fakeWorkerAdmin{devices: []WorkerDevice{b3Device("laptop", b3Now.Add(-10*time.Second), "repo.write")}}
	left, reset := int64(3), time.Date(2026, 11, 17, 18, 30, 0, 0, time.UTC)
	tel := &b3Telemetry{recs: []WorkerTelemetryRecord{{DeviceID: "dev-laptop", ReceivedAt: b3Now,
		Snapshot: WorkerTelemetry{State: "idle", ClaudeSessionLeftPct: &left, ClaudeSessionResetAt: &reset}}}}
	e := b3Notifier(admin, tel)
	for i := 0; i < 2; i++ {
		e.n.WorkerJobQueued(context.Background(), b3Job(WorkerJobWaiting))
		e.drain()
	}
	got := e.m.texts()
	if len(got) != 1 || !strings.Contains(got[0], "2026-11-17 18:30 UTC") || !strings.Contains(got[0], "hạn mức Claude") {
		t.Fatalf("messages %v", got)
	}
	if e.routes.route(41).WaitingNotice != workerNoticeBlocked {
		t.Fatalf("notice %q", e.routes.route(41).WaitingNotice)
	}
}

// B3-N4 (REQ-8): the lease text names the device.
func TestB3Notifier_LeasedTextNamesDevice(t *testing.T) {
	admin := &fakeWorkerAdmin{devices: []WorkerDevice{b3Device("laptop", b3Now, "repo.write")}}
	e := b3Notifier(admin, nil)
	job := b3Job(WorkerJobLeased)
	job.DeviceID, job.Attempt = "dev-laptop", 2
	e.change(WorkerChangeLeased, job)
	got := e.m.texts()
	if len(got) != 1 || !strings.Contains(got[0], "laptop") || !strings.Contains(got[0], "WF-41") || !strings.Contains(got[0], "lần 2") {
		t.Fatalf("messages %v", got)
	}
}

func b3Progress(files int64) WorkerJob {
	j := b3Job(WorkerJobRunning)
	j.Progress = &WorkerProgress{Seq: files, Phase: "editing", Counts: WorkerProgressCounts{FilesChanged: files}}
	return j
}

// B3-N5 (REQ-8): progress is one message edited in place; an unchanged text
// is skipped; "message is not modified" counts as success; a refused edit
// falls back to one new message.
func TestB3Notifier_ProgressEditsOneMessage(t *testing.T) {
	e := b3Notifier(&fakeWorkerAdmin{}, nil)
	e.change(WorkerChangeProgress, b3Progress(1))
	if len(e.m.sent) != 1 || len(e.m.edits) != 0 {
		t.Fatalf("first progress: sent %d edits %d", len(e.m.sent), len(e.m.edits))
	}
	first := e.routes.route(41).ProgressMessageID
	if first == 0 || e.routes.route(41).ProgressAttempt != 1 {
		t.Fatalf("progress message not remembered: %+v", e.routes.route(41))
	}
	*e.clock = e.clock.Add(4 * time.Second)
	e.change(WorkerChangeProgress, b3Progress(2))
	if len(e.m.sent) != 1 || len(e.m.edits) != 1 || e.m.edits[0].id != first || !strings.Contains(e.m.edits[0].text, "2 tệp") {
		t.Fatalf("second progress: sent %d edits %+v", len(e.m.sent), e.m.edits)
	}
	*e.clock = e.clock.Add(4 * time.Second)
	e.change(WorkerChangeProgress, b3Progress(2)) // same text
	if e.m.tries != 1 {
		t.Fatalf("unchanged text was edited again (%d tries)", e.m.tries)
	}
	*e.clock = e.clock.Add(4 * time.Second)
	e.m.editErr = &relay.DeliveryError{Code: "message_not_modified"}
	e.change(WorkerChangeProgress, b3Progress(3))
	e.change(WorkerChangeProgress, b3Progress(3))
	if len(e.m.sent) != 1 || e.m.tries != 2 {
		t.Fatalf("not-modified must be success: sent %d tries %d", len(e.m.sent), e.m.tries)
	}
	*e.clock = e.clock.Add(4 * time.Second)
	e.m.editErr = errors.New("message gone")
	e.change(WorkerChangeProgress, b3Progress(4))
	if len(e.m.sent) != 2 || e.routes.route(41).ProgressMessageID == first {
		t.Fatalf("refused edit did not fall back to a new message: sent %d route %+v", len(e.m.sent), e.routes.route(41))
	}
}

// B3-N6 (REQ-8): a new attempt gets a new progress message.
func TestB3Notifier_NewAttemptNewProgressMessage(t *testing.T) {
	e := b3Notifier(&fakeWorkerAdmin{}, nil)
	e.change(WorkerChangeProgress, b3Progress(1))
	*e.clock = e.clock.Add(4 * time.Second)
	j := b3Progress(2)
	j.Attempt = 2
	e.change(WorkerChangeProgress, j)
	if len(e.m.sent) != 2 || len(e.m.edits) != 0 || e.routes.route(41).ProgressAttempt != 2 {
		t.Fatalf("sent %d edits %d route %+v", len(e.m.sent), len(e.m.edits), e.routes.route(41))
	}
}

// B3-N7 (REQ-8): updates inside the 3 s edit gap are held back and only the
// newest is flushed once the gap ends.
func TestB3Notifier_ProgressCoalescesWithinGap(t *testing.T) {
	e := b3Notifier(&fakeWorkerAdmin{}, nil)
	e.change(WorkerChangeProgress, b3Progress(1))
	e.change(WorkerChangeProgress, b3Progress(2))
	e.change(WorkerChangeProgress, b3Progress(3))
	if e.m.tries != 0 {
		t.Fatalf("edits inside the gap: %d", e.m.tries)
	}
	e.n.mu.Lock()
	held := e.n.deferred["job-1"]
	e.n.mu.Unlock()
	if held == nil || held.Progress.Counts.FilesChanged != 3 {
		t.Fatalf("newest progress not held: %+v", held)
	}
	*e.clock = e.clock.Add(workerProgressGap)
	e.n.flushDeferred("job-1")
	e.drain()
	if len(e.m.edits) != 1 || !strings.Contains(e.m.edits[0].text, "3 tệp") {
		t.Fatalf("flush must edit once with the latest text: %+v", e.m.edits)
	}
	e.n.flushDeferred("job-1")
	e.drain()
	if len(e.m.edits) != 1 {
		t.Fatalf("second flush edited again: %+v", e.m.edits)
	}
}

// B3-N8 (REQ-8): a held-back progress is dropped when the job finishes; no
// progress edit follows the result.
func TestB3Notifier_NoProgressEditAfterFinish(t *testing.T) {
	e := b3Notifier(&fakeWorkerAdmin{}, nil)
	e.change(WorkerChangeProgress, b3Progress(1))
	e.change(WorkerChangeProgress, b3Progress(2)) // held back
	done := b3Job(WorkerJobSucceeded)
	done.Result = &WorkerResult{Status: "succeeded", Summary: "done"}
	e.change(WorkerChangeFinished, done)
	*e.clock = e.clock.Add(10 * time.Second)
	e.n.flushDeferred("job-1")
	e.drain()
	if e.m.tries != 0 || len(e.m.edits) != 0 {
		t.Fatalf("progress edited after the result: %+v", e.m.edits)
	}
}

// B3-N9 (REQ-11): the result text has the PR link, a success wakes the
// workflow step, and so does a cancel; a failure keeps it waiting.
func TestB3Notifier_ResultTextAndWake(t *testing.T) {
	e := b3Notifier(&fakeWorkerAdmin{}, nil)
	ok := b3Job(WorkerJobSucceeded)
	ok.Result = &WorkerResult{Status: "succeeded", Summary: "Fixed login", Branch: "nexus/fc-1", PRURL: "https://github.com/o/r/pull/7", TestOutcome: "passed"}
	e.change(WorkerChangeFinished, ok)
	got := e.m.texts()
	if len(got) != 1 || !strings.Contains(got[0], "PR: https://github.com/o/r/pull/7") || !strings.Contains(got[0], "nexus/fc-1") ||
		!strings.Contains(got[0], "Fixed login") || len(e.m.sent[0].Buttons) != 0 {
		t.Fatalf("result message %v", got)
	}
	if len(e.waker.calls) != 1 || e.waker.calls[0] != p4WS+"/"+StepKeyWorkerRun {
		t.Fatalf("step not woken: %v", e.waker.calls)
	}
	cancelled := b3Job(WorkerJobCancelled)
	e.change(WorkerChangeFinished, cancelled)
	if len(e.waker.calls) != 2 {
		t.Fatalf("cancel did not wake the step: %v", e.waker.calls)
	}
	failed := b3Job(WorkerJobFailed)
	failed.Result = &WorkerResult{Status: "failed", ReasonCode: "tests_failed"}
	e.change(WorkerChangeFinished, failed)
	if len(e.waker.calls) != 2 {
		t.Fatalf("failure woke the step: %v", e.waker.calls)
	}
}

// B3-N10 (REQ-11): a failure carries Retry and Cancel buttons bound to the
// chat, the job and the attempt, valid for 24 hours; the approval holds ids
// only.
func TestB3Notifier_FailureButtons(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
		job  WorkerJob
	}{
		{"failed", WorkerChangeFinished, func() WorkerJob {
			j := b3Job(WorkerJobFailed)
			j.Result = &WorkerResult{Status: "failed", ReasonCode: "executor_timeout", Summary: "took too long"}
			return j
		}()},
		{"needs input", WorkerChangeFinished, b3Job(WorkerJobNeedsInput)},
		{"interrupted", WorkerChangeInterrupted, b3Job(WorkerJobInterrupted)},
	} {
		e := b3Notifier(&fakeWorkerAdmin{}, nil)
		e.change(tc.kind, tc.job)
		if len(e.m.sent) != 1 {
			t.Fatalf("%s: messages %v", tc.name, e.m.texts())
		}
		msg := e.m.sent[0]
		if len(msg.Buttons) != 1 || len(msg.Buttons[0]) != 2 || msg.Buttons[0][0].Text != btnWorkerRetry || msg.Buttons[0][1].Text != btnWorkerCancel {
			t.Fatalf("%s: buttons %+v", tc.name, msg.Buttons)
		}
		if len(e.ap.created) != 1 {
			t.Fatalf("%s: approvals %d", tc.name, len(e.ap.created))
		}
		a := e.ap.created[0]
		var p workerJobPayload
		if err := json.Unmarshal(a.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if a.Kind != ApprovalWorkerJob || a.Resource != "worker_job:job-1" || !a.ExpiresAt.Equal(b3Now.Add(24*time.Hour)) ||
			p.ChatID != b1Chat || p.JobID != "job-1" || p.Attempt != 1 || p.ExecutionID != 41 {
			t.Fatalf("%s: approval %+v payload %+v", tc.name, a, p)
		}
		retry, err := ParseCallbackData(msg.Buttons[0][0].Data)
		cancel, err2 := ParseCallbackData(msg.Buttons[0][1].Data)
		if err != nil || err2 != nil || retry.Kind != CallbackApprove || cancel.Kind != CallbackReject || retry.Nonce != a.Nonce || cancel.Nonce != a.Nonce {
			t.Fatalf("%s: callback data %+v %+v", tc.name, retry, cancel)
		}
		stored, _ := e.ap.GetApprovalByNonce(context.Background(), p4WS, a.Nonce)
		if stored.TelegramMessageID == 0 {
			t.Fatalf("%s: button message not bound to its approval", tc.name)
		}
	}
	// The failure text names the reason and the workflow, never a stack or path.
	e := b3Notifier(&fakeWorkerAdmin{}, nil)
	j := b3Job(WorkerJobFailed)
	j.Result = &WorkerResult{Status: "failed", ReasonCode: "executor_timeout"}
	e.change(WorkerChangeFinished, j)
	if got := e.m.texts()[0]; !strings.HasPrefix(got, "WF-41: ") || !strings.Contains(got, "quá thời gian") {
		t.Fatalf("failure text %q", got)
	}
}

// ---- Retry and Cancel buttons, /retry ----

type b3ButtonEnv struct {
	w       *InboxWorker
	m       *b3Msgr
	n       *b3NotifierEnv
	jobs    *b3Jobs
	queued  *b3Queued
	data    CallbackData
	msgID   int64
	buttons [][]Button
}

func b3Buttons(t *testing.T, state string) *b3ButtonEnv {
	t.Helper()
	n := b3Notifier(&fakeWorkerAdmin{}, nil)
	job := b3Job(state)
	job.Result = &WorkerResult{Status: "failed", ReasonCode: "tests_failed"}
	n.change(WorkerChangeFinished, job)
	if len(n.m.sent) != 1 || len(n.m.sent[0].Buttons) != 1 {
		t.Fatalf("no buttons: %v", n.m.texts())
	}
	e := &b3ButtonEnv{n: n, m: n.m, jobs: &b3Jobs{job: job, found: true}, queued: &b3Queued{}, buttons: n.m.sent[0].Buttons, msgID: 701}
	e.w = NewInboxWorker(InboxDeps{Messenger: n.m, Approvals: n.ap, WorkerJobs: e.jobs, WorkerNotifier: e.queued}, InboxSettings{})
	e.w.now = func() time.Time { return *n.clock }
	return e
}

func (e *b3ButtonEnv) press(index int, chat int64) outcome {
	return e.w.dispatch(context.Background(), InboundUpdate{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: chat,
		Kind: UpdateCallback, Command: e.buttons[0][index].Data, Args: "q1 701"}, "corr")
}

func (e *b3ButtonEnv) lastToast() string {
	e.m.mu.Lock()
	defer e.m.mu.Unlock()
	if len(e.m.toasts) == 0 {
		return ""
	}
	return e.m.toasts[len(e.m.toasts)-1]
}

// B3-R1 (REQ-11): Retry retries the attempt in the button, tells the topic
// the job waits again, decides the approval, and a second press is stale.
func TestB3Retry_ButtonRetriesOnce(t *testing.T) {
	e := b3Buttons(t, WorkerJobFailed)
	if out := e.press(0, b1Chat); out.code != "" {
		t.Fatalf("code %q toast %q", out.code, e.lastToast())
	}
	if len(e.jobs.retries) != 1 || e.jobs.retries[0] != 1 || e.lastToast() != "Đang thử lại." {
		t.Fatalf("retries %v toast %q", e.jobs.retries, e.lastToast())
	}
	if len(e.queued.jobs) != 1 || e.queued.jobs[0].State != WorkerJobFailed {
		// GetJob returns the stored job; the notifier is only told it waits again.
		t.Fatalf("topic not told: %+v", e.queued.jobs)
	}
	if len(e.m.removed) != 1 {
		t.Fatalf("buttons not removed: %v", e.m.removed)
	}
	if out := e.press(0, b1Chat); out.code != "stale" || len(e.jobs.retries) != 1 || e.lastToast() != msgStale {
		t.Fatalf("second press: %q retries %v toast %q", out.code, e.jobs.retries, e.lastToast())
	}
}

// B3-R2 (REQ-11): Cancel cancels the job; the buttons are bound to the chat;
// they expire after 24 hours.
func TestB3Retry_CancelBoundAndExpiring(t *testing.T) {
	e := b3Buttons(t, WorkerJobFailed)
	if out := e.press(0, b1Chat+1); out.code != "stale" || len(e.jobs.retries) != 0 {
		t.Fatalf("foreign chat: %q retries %v", out.code, e.jobs.retries)
	}
	if out := e.press(1, b1Chat); out.code != "" || e.jobs.cancels != 1 || e.lastToast() != "Đã huỷ." {
		t.Fatalf("cancel: %q cancels %d toast %q", out.code, e.jobs.cancels, e.lastToast())
	}
	e2 := b3Buttons(t, WorkerJobFailed)
	*e2.n.clock = e2.n.clock.Add(24*time.Hour + time.Second)
	if out := e2.press(0, b1Chat); out.code != "stale" || len(e2.jobs.retries) != 0 || e2.lastToast() != msgStale {
		t.Fatalf("expired button: %q retries %v toast %q", out.code, e2.jobs.retries, e2.lastToast())
	}
}

// B3-R3 (REQ-11): Retry on an ended execution gives the execution_ended
// toast and closes the buttons for good.
func TestB3Retry_ExecutionEndedButton(t *testing.T) {
	e := b3Buttons(t, WorkerJobFailed)
	e.jobs.retryErr = ErrWorkerExecutionEnded
	out := e.press(0, b1Chat)
	if out.code != "execution_ended" || e.lastToast() != msgWorkerExecutionEnded || len(e.m.removed) != 1 {
		t.Fatalf("code %q toast %q removed %v", out.code, e.lastToast(), e.m.removed)
	}
	if len(e.queued.jobs) != 0 {
		t.Fatal("topic told the job waits although it cannot")
	}
	e.jobs.retryErr = nil
	if out := e.press(0, b1Chat); out.code != "stale" || len(e.jobs.retries) != 1 {
		t.Fatalf("pressing again: %q retries %v", out.code, e.jobs.retries)
	}
}

func (e *b3ButtonEnv) retryCmd(args string) (outcome, string) {
	before := len(e.m.texts())
	out := e.w.dispatch(context.Background(), InboundUpdate{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: b1Chat,
		Kind: UpdateCommand, Command: "/retry", Args: args}, "corr")
	got := e.m.texts()
	if len(got) != before+1 {
		return out, ""
	}
	return out, got[len(got)-1]
}

// B3-R4 (REQ-11): /retry WF-n requeues only retryable states and prefixes
// every answer with the workflow reference.
func TestB3Retry_Command(t *testing.T) {
	for _, state := range []string{WorkerJobInterrupted, WorkerJobFailed, WorkerJobNeedsInput} {
		e := b3Buttons(t, WorkerJobFailed)
		e.jobs.job.State = state
		out, text := e.retryCmd("WF-41")
		if out.code != "" || text != "WF-41: đã xếp lại hàng chờ máy local." || len(e.jobs.retries) != 1 || len(e.queued.jobs) != 1 {
			t.Fatalf("%s: %q %q retries %v queued %d", state, out.code, text, e.jobs.retries, len(e.queued.jobs))
		}
	}
	for _, state := range []string{WorkerJobWaiting, WorkerJobLeased, WorkerJobRunning, WorkerJobSucceeded, WorkerJobCancelled} {
		e := b3Buttons(t, WorkerJobFailed)
		e.jobs.job.State = state
		_, text := e.retryCmd("WF-41")
		if !strings.HasPrefix(text, "WF-41: ") || !strings.Contains(text, "không cần thử lại") || len(e.jobs.retries) != 0 {
			t.Fatalf("%s: %q retries %v", state, text, e.jobs.retries)
		}
	}
	e := b3Buttons(t, WorkerJobFailed)
	e.jobs.retryErr = ErrWorkerExecutionEnded
	if out, text := e.retryCmd("WF-41"); out.code != "" || text != "WF-41: "+msgWorkerExecutionEnded {
		t.Fatalf("ended: %q %q", out.code, text)
	}
	e.jobs.retryErr = ErrConflict
	if out, text := e.retryCmd("WF-41"); out.code != "stale" || text != msgStale {
		t.Fatalf("conflict: %q %q", out.code, text)
	}
	e.jobs.found = false
	if out, text := e.retryCmd("WF-41"); out.code != "not_found" || text != msgNotFound {
		t.Fatalf("not found: %q %q", out.code, text)
	}
	if out, text := e.retryCmd("garbage"); out.code != "usage" || text != usageRetry {
		t.Fatalf("usage: %q %q", out.code, text)
	}
	if out, text := e.retryCmd(""); out.code != "usage" || text != usageRetry {
		t.Fatalf("no id outside a topic: %q %q", out.code, text)
	}
}

// ---- /worker card ----

// B3-C1 (REQ-18): the card shows state, CPU, RAM, disk, Claude limits,
// tokens, jobs, failures, busy hours and last seen.
func TestB3WorkerCard_AllFields(t *testing.T) {
	seen := b3Now.Add(-30 * time.Second)
	load, cores := 0.5, int64(8)
	ramUsed, ramTotal := int64(8<<30), int64(16<<30)
	diskUsed, diskTotal := int64(100<<30), int64(500<<30)
	weekly, reset := int64(40), time.Date(2026, 11, 17, 18, 30, 0, 0, time.UTC)
	admin := &fakeWorkerAdmin{devices: []WorkerDevice{{ID: "dev-1", Name: "laptop", State: "busy", LastSeenAt: &seen, CreatedAt: seen}}}
	tel := &b3Telemetry{recs: []WorkerTelemetryRecord{{DeviceID: "dev-1", ReceivedAt: seen, Snapshot: WorkerTelemetry{
		State: "busy", CPULoad: &load, CPUCores: &cores, RAMUsedBytes: &ramUsed, RAMTotalBytes: &ramTotal,
		DiskUsedBytes: &diskUsed, DiskTotalBytes: &diskTotal, ClaudeWeeklyLeftPct: &weekly, ClaudeWeeklyResetAt: &reset,
		Executor: "claude", ExecutorVersion: "2.1.0",
		Counters: WorkerCounters{Jobs: 3, Failures: 1, ExecutorTokens: 1500, BusySeconds: 7200},
	}}}}
	m := &captureMessenger{}
	w := NewInboxWorker(InboxDeps{Messenger: m, Workers: admin, WorkerTelemetry: tel}, InboxSettings{})
	w.now = func() time.Time { return b3Now }
	w.dispatch(context.Background(), InboundUpdate{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: b1Chat, Kind: UpdateCommand, Command: "/worker"}, "c")
	if len(m.texts) != 1 {
		t.Fatalf("replies %v", m.texts)
	}
	for _, want := range []string{
		"laptop", "đang bận", "CPU: tải 0.50 / 8 nhân", "RAM: 8.0 GB / 16.0 GB (50%)", "Ổ đĩa: 100.0 GB / 500.0 GB (20%)",
		"phiên không rõ", "tuần còn 40% (reset 11-17 18:30 UTC)", "Token: 1.5K", "Việc: 3", "Lỗi: 1", "Giờ chạy: 2.0",
		"claude 2.1.0", "Lần cuối: 2026-11-17 09:59 UTC",
	} {
		if !strings.Contains(m.texts[0], want) {
			t.Errorf("card lacks %q:\n%s", want, m.texts[0])
		}
	}
}

// B3-C2 (REQ-18): unknown telemetry reads "không rõ", never zero; a device
// that never connected says so; each device has its own card.
func TestB3WorkerCard_UnknownAndTwoDevices(t *testing.T) {
	seen := b3Now.Add(-5 * time.Second)
	admin := &fakeWorkerAdmin{devices: []WorkerDevice{
		{ID: "dev-1", Name: "alpha", LastSeenAt: &seen, CreatedAt: seen},
		{ID: "dev-2", Name: "beta", CreatedAt: seen},
	}}
	m := &captureMessenger{}
	w := NewInboxWorker(InboxDeps{Messenger: m, Workers: admin}, InboxSettings{})
	w.now = func() time.Time { return b3Now }
	w.dispatch(context.Background(), InboundUpdate{WorkspaceID: p4WS, IntegrationID: b1Bot, ChatID: b1Chat, Kind: UpdateCommand, Command: "/worker"}, "c")
	if len(m.texts) != 1 {
		t.Fatalf("replies %v", m.texts)
	}
	text := m.texts[0]
	if strings.Count(text, "Hạn mức Claude: không rõ") != 2 || !strings.Contains(text, "CPU: không rõ · RAM: không rõ · Ổ đĩa: không rõ") ||
		!strings.Contains(text, "alpha") || !strings.Contains(text, "beta") || !strings.Contains(text, "chưa từng kết nối") {
		t.Fatalf("card:\n%s", text)
	}
	// Telemetry that omits the Claude limits keeps them unknown.
	info := WorkerDeviceInfo{Name: "gamma", State: "online", Telemetry: &WorkerTelemetry{State: "idle"}}
	if got := FormatWorkerCard(info); !strings.Contains(got, "Hạn mức Claude: phiên không rõ · tuần không rõ") {
		t.Fatalf("partial card:\n%s", got)
	}
}

// ---- WorkerTaskSweeper ----

type b3Upkeep struct {
	mu    sync.Mutex
	calls int
	fail  bool
}

func (u *b3Upkeep) SyncWorkerTasks(context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls++
	if u.fail && u.calls == 1 {
		return errors.New("transient")
	}
	return nil
}
func (u *b3Upkeep) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

// B3-W1 (REQ-4): the task sweeper syncs at start and on every tick, survives
// an error, and stops with its context.
func TestB3TaskSweeper_RunsSurvivesErrorStops(t *testing.T) {
	up := &b3Upkeep{fail: true}
	sw := &WorkerTaskSweeper{Store: up, Interval: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sw.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for up.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if up.count() < 3 {
		t.Fatalf("sweeper synced %d times", up.count())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sweeper did not stop")
	}
}
