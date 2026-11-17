package storage

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	httpdelivery "github.com/LDKhangg/nexus-hub/internal/delivery/http"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/labstack/echo/v4"
)

// Phase 7a Fix Brief B3 (storage side): local tasks over the real stores.
// CI only; DB tests use the dedicated loopback test database.

// b3Env is a cut-over workspace with the worker store, the local-task store,
// the conversation store and a workflow worker that runs the worker.run step.
type b3Env struct {
	*w7Env
	wf     *wfEnv
	tasks  *WorkerTaskStore
	conv   *ConversationStore
	worker *workflow.Worker
	convID int64
	update int64
}

func newB3Env(t *testing.T) *b3Env {
	t.Helper()
	e := newWFEnv(t)
	if _, err := e.store.Cutover(e.ctx, workspaceA, true); err != nil {
		t.Fatalf("cutover: %v", err)
	}
	ws, err := NewWorkerStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	w7 := &w7Env{ctx: e.ctx, pool: e.pool, s: ws, clock: time.Now().UTC().Truncate(time.Second)}
	ws.now = func() time.Time { return w7.clock }
	tasks, err := NewWorkerTaskStore(e.pool, ws, e.store)
	if err != nil {
		t.Fatal(err)
	}
	conv := phase6ConversationStore(t, e)
	c, err := conv.EnsureConversation(e.ctx, workflow.Conversation{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: 7001})
	if err != nil {
		t.Fatal(err)
	}
	reg := workflow.NewRegistry()
	step := &behavior.WorkerRunStep{Jobs: ws, Routes: tasks, Instructions: conv}
	if err := step.Register(reg); err != nil {
		t.Fatal(err)
	}
	clock := &manualClock{now: time.Now().Add(time.Minute)}
	return &b3Env{w7Env: w7, wf: e, tasks: tasks, conv: conv, convID: c.ID,
		worker: workflow.NewWorker(e.store, reg, clock, workflow.WorkerSettings{Workers: 1, PollInterval: time.Millisecond})}
}

// start starts a local task and stores its instruction like /task does.
func (b *b3Env) start(t *testing.T, text string) workflow.Execution {
	t.Helper()
	b.update++
	ex, err := b.tasks.StartLocalTask(b.ctx, behavior.LocalTaskRequest{
		WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: 7001, UpdateID: b.update,
		Repo: "o/r", TaskRef: "FC-1", ClickUpTaskID: "86abc",
	})
	if err != nil {
		t.Fatalf("start local task: %v", err)
	}
	if _, err := b.conv.AppendInstruction(b.ctx, workflow.InstructionRequest{WorkspaceID: workspaceA, ExecutionID: ex.ID,
		Text: text, ConversationID: b.convID, TelegramUpdateID: b.update}); err != nil {
		t.Fatalf("append instruction: %v", err)
	}
	return ex
}

func (b *b3Env) followUp(t *testing.T, ex workflow.Execution, text string) {
	t.Helper()
	b.update++
	if _, err := b.conv.AppendInstruction(b.ctx, workflow.InstructionRequest{WorkspaceID: workspaceA, ExecutionID: ex.ID,
		Text: text, ConversationID: b.convID, TelegramUpdateID: b.update}); err != nil {
		t.Fatalf("follow-up: %v", err)
	}
}

// runStep processes one due workflow job and checks whether there was one.
func (b *b3Env) runStep(t *testing.T, wantWork bool) {
	t.Helper()
	worked, err := b.worker.ProcessNext(b.ctx)
	if err != nil || worked != wantWork {
		t.Fatalf("ProcessNext = %v, %v; want work=%v", worked, err, wantWork)
	}
}

func (b *b3Env) sync(t *testing.T) {
	t.Helper()
	if err := b.tasks.SyncWorkerTasks(b.ctx); err != nil {
		t.Fatalf("SyncWorkerTasks: %v", err)
	}
}

func (b *b3Env) jobOf(t *testing.T, ex workflow.Execution) behavior.WorkerJob {
	t.Helper()
	j, ok, err := b.s.JobForStep(b.ctx, workspaceA, ex.ID, behavior.StepKeyWorkerRun)
	if err != nil || !ok {
		t.Fatalf("job of execution %d: ok=%v err=%v", ex.ID, ok, err)
	}
	return j
}

func (b *b3Env) execState(ex workflow.Execution) workflow.Execution {
	return b.wf.execution(workspaceA, ex.ID)
}

// waitingTask starts a task and runs its step so that its job waits.
func (b *b3Env) waitingTask(t *testing.T, text string) (workflow.Execution, behavior.WorkerJob) {
	t.Helper()
	ex := b.start(t, text)
	b.runStep(t, true)
	if st := b.execState(ex); st.State != workflow.StateWaitingForUser {
		t.Fatalf("execution %d is %s, want WAITING_FOR_USER", ex.ID, st.State)
	}
	j := b.jobOf(t, ex)
	if j.State != behavior.WorkerJobWaiting || j.Attempt != 0 {
		t.Fatalf("job %+v", j)
	}
	return ex, j
}

// endedJob drives a task's job to FAILED, NEEDS_INPUT or INTERRUPTED.
func (b *b3Env) endedJob(t *testing.T, d behavior.WorkerDevice, state string) (workflow.Execution, behavior.WorkerJob) {
	t.Helper()
	ex, j := b.waitingTask(t, "work for "+state)
	l := b.mustLease(t, d)
	if l.Job.ID != j.ID {
		t.Fatalf("leased %s, want %s", l.Job.ID, j.ID)
	}
	switch state {
	case behavior.WorkerJobFailed:
		_, _, err := b.s.ReportResult(b.ctx, d, j.ID, l.LeaseToken, "key-"+j.ID, okResult("failed"))
		if err != nil {
			t.Fatal(err)
		}
	case behavior.WorkerJobNeedsInput:
		_, _, err := b.s.ReportResult(b.ctx, d, j.ID, l.LeaseToken, "key-"+j.ID, okResult("needs_input"))
		if err != nil {
			t.Fatal(err)
		}
	case behavior.WorkerJobInterrupted:
		b.advance(11 * time.Minute)
		if n, err := b.s.ExpireLeases(b.ctx); err != nil || n < 1 {
			t.Fatalf("ExpireLeases = %d, %v", n, err)
		}
	}
	got := b.jobOf(t, ex)
	if got.State != state {
		t.Fatalf("job is %s, want %s", got.State, state)
	}
	return ex, got
}

// B3-DB1 (REQ-4): the check constraints accept the new kinds and an
// approval of kind worker_job and workflow_target can be stored.
func TestB3Store_ConstraintsAcceptNewKinds(t *testing.T) {
	e := newWFEnv(t)
	for name, want := range map[string][]string{
		"approvals_kind_check":          {"'worker_job'", "'workflow_target'"},
		"behaviors_internal_kind_check": {"'local_task'", "'agent_comment_pipeline'"},
	} {
		var def string
		if err := e.pool.QueryRow(e.ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname=$1`, name).Scan(&def); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, w := range want {
			if !strings.Contains(def, w) {
				t.Fatalf("%s does not accept %s: %s", name, w, def)
			}
		}
	}
	for _, kind := range []behavior.ApprovalKind{behavior.ApprovalWorkerJob, behavior.ApprovalWorkflowTarget} {
		a, err := e.behaviors.CreateApproval(e.ctx, workspaceA, behavior.NewApproval{
			Kind: kind, Action: "retry_or_cancel", Resource: "worker_job:x", Payload: []byte(`{"job_id":"x"}`),
			Nonce: nonce("k"), ExpiresAt: time.Now().Add(time.Hour),
		})
		if err != nil || a.Kind != kind {
			t.Fatalf("approval kind %s: %+v %v", kind, a, err)
		}
	}
	if _, err := e.behaviors.CreateApproval(e.ctx, workspaceA, behavior.NewApproval{
		Kind: "bogus", Action: "a", Resource: "r", Nonce: nonce("b"), ExpiresAt: time.Now().Add(time.Hour),
	}); err == nil {
		t.Fatal("unknown approval kind accepted")
	}
}

// B3-DB2 (REQ-4, REQ-19): /task creates a local_task behavior row, a route,
// and keeps the instruction text out of workflow, behavior, route and audit
// rows. The text lives only in the instruction row and the job payload.
func TestB3Store_InstructionTextOnlyWhereItBelongs(t *testing.T) {
	b := newB3Env(t)
	const secret = "zebra-unicorn-credentials"
	ex, _ := b.waitingTask(t, secret)
	for _, tbl := range []string{"workflow_executions", "execution_steps", "execution_events", "workflow_waits",
		"workflow_signals", "behaviors", "behavior_versions", "worker_task_routes", "worker_events"} {
		var n int
		if err := b.pool.QueryRow(b.ctx, `SELECT count(*) FROM public.`+tbl+` r WHERE r::text LIKE $1`, "%"+secret+"%").Scan(&n); err != nil {
			t.Fatalf("%s: %v", tbl, err)
		}
		if n != 0 {
			t.Fatalf("instruction text found in %s", tbl)
		}
	}
	for tbl, want := range map[string]int{"workflow_instructions": 1, "worker_jobs": 1} {
		var n int
		if err := b.pool.QueryRow(b.ctx, `SELECT count(*) FROM public.`+tbl+` r WHERE r::text LIKE $1`, "%"+secret+"%").Scan(&n); err != nil || n != want {
			t.Fatalf("%s holds the text %d times (want %d): %v", tbl, n, want, err)
		}
	}
	var kinds int
	if err := b.pool.QueryRow(b.ctx, `SELECT count(*) FROM public.behaviors WHERE internal_kind='local_task'`).Scan(&kinds); err != nil || kinds != 1 {
		t.Fatalf("local_task behavior rows %d: %v", kinds, err)
	}
	route, err := b.tasks.GetRoute(b.ctx, workspaceA, ex.ID)
	if err != nil || route.Repo != "o/r" || route.TaskRef != "FC-1" || route.ClickUpTaskID != "86abc" || route.InstructionVersion != 1 {
		t.Fatalf("route %+v %v", route, err)
	}
	if _, err := b.tasks.GetRoute(b.ctx, workspaceB, ex.ID); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("route visible from another workspace: %v", err)
	}
	// The same inbound update never starts a second execution.
	again, err := b.tasks.StartLocalTask(b.ctx, behavior.LocalTaskRequest{WorkspaceID: workspaceA, IntegrationID: integrationA,
		ChatID: 7001, UpdateID: b.update, Repo: "o/r", TaskRef: "FC-1", ClickUpTaskID: "86abc"})
	if err != nil || again.ID != ex.ID {
		t.Fatalf("redelivered /task started %v (%v), want %d", again.ID, err, ex.ID)
	}
}

// B3-DB3 (REQ-4): the step follows the job. SUCCEEDED completes the
// execution, CANCELLED fails it with worker_cancelled, and FAILED,
// NEEDS_INPUT and INTERRUPTED keep it waiting. The sweeper wakes a step whose
// wake-up was missed.
func TestB3Store_StepFollowsJobStates(t *testing.T) {
	b := newB3Env(t)
	d, _ := b.enroll(t, workspaceA, "laptop", "repo.write")

	ex, j := b.waitingTask(t, "succeed")
	l := b.mustLease(t, d)
	if _, _, err := b.s.ReportResult(b.ctx, d, j.ID, l.LeaseToken, "k-ok", okResult("succeeded")); err != nil {
		t.Fatal(err)
	}
	b.runStep(t, false) // no observer ran: the step has not been woken yet
	if st := b.execState(ex); st.State != workflow.StateWaitingForUser {
		t.Fatalf("execution moved without a wake-up: %s", st.State)
	}
	b.sync(t)
	b.runStep(t, true)
	if st := b.execState(ex); st.State != workflow.StateCompleted {
		t.Fatalf("succeeded job: execution %s (%s)", st.State, st.Reason)
	}
	b.sync(t) // idempotent
	b.runStep(t, false)

	ex2, j2 := b.waitingTask(t, "cancel")
	if _, err := b.s.CancelJob(b.ctx, workspaceA, j2.ID); err != nil {
		t.Fatal(err)
	}
	b.sync(t)
	b.runStep(t, true)
	if st := b.execState(ex2); st.State != workflow.StateFailed || string(st.Reason) != behavior.CodeWorkerCancelled {
		t.Fatalf("cancelled job: execution %s reason %q", st.State, st.Reason)
	}

	for _, state := range []string{behavior.WorkerJobFailed, behavior.WorkerJobNeedsInput, behavior.WorkerJobInterrupted} {
		exN, _ := b.endedJob(t, d, state)
		b.sync(t)
		b.runStep(t, false)
		if st := b.execState(exN); st.State != workflow.StateWaitingForUser {
			t.Fatalf("%s job: execution %s, want it to keep waiting", state, st.State)
		}
		if got := b.jobOf(t, exN); got.State != state {
			t.Fatalf("%s job changed to %s by the sweeper", state, got.State)
		}
	}
}

// B3-DB4 (REQ-11): RetryJob queues FAILED, NEEDS_INPUT and INTERRUPTED jobs
// again and audits it; the next lease is the next attempt; a stale attempt, a
// job that is not retryable, an unknown job and another workspace are refused.
func TestB3Store_RetryJob(t *testing.T) {
	b := newB3Env(t)
	d, _ := b.enroll(t, workspaceA, "laptop", "repo.write")
	for _, state := range []string{behavior.WorkerJobFailed, behavior.WorkerJobNeedsInput, behavior.WorkerJobInterrupted} {
		ex, j := b.endedJob(t, d, state)
		if _, err := b.s.RetryJob(b.ctx, workspaceA, j.ID, j.Attempt+1); !errors.Is(err, behavior.ErrConflict) || errors.Is(err, behavior.ErrWorkerExecutionEnded) {
			t.Fatalf("%s: stale attempt: %v", state, err)
		}
		if _, err := b.s.RetryJob(b.ctx, workspaceB, j.ID, j.Attempt); !errors.Is(err, behavior.ErrNotFound) {
			t.Fatalf("%s: foreign workspace: %v", state, err)
		}
		retried, err := b.s.RetryJob(b.ctx, workspaceA, j.ID, j.Attempt)
		if err != nil || retried.State != behavior.WorkerJobWaiting || retried.DeviceID != "" || retried.Result != nil {
			t.Fatalf("%s: retry %+v %v", state, retried, err)
		}
		if !hasEvent(b.events(t), "retried:") {
			t.Fatalf("%s: retry not audited: %v", state, b.events(t))
		}
		if _, err := b.s.RetryJob(b.ctx, workspaceA, j.ID, j.Attempt); !errors.Is(err, behavior.ErrConflict) {
			t.Fatalf("%s: second retry of a waiting job: %v", state, err)
		}
		l := b.mustLease(t, d)
		if l.Job.ID != j.ID || l.Job.Attempt != j.Attempt+1 {
			t.Fatalf("%s: next lease is attempt %d, want %d", state, l.Job.Attempt, j.Attempt+1)
		}
		if st := b.execState(ex); st.State != workflow.StateWaitingForUser {
			t.Fatalf("%s: execution %s after retry", state, st.State)
		}
		// Finish the job so the device is free for the next state.
		if _, _, err := b.s.ReportResult(b.ctx, d, j.ID, l.LeaseToken, "end-"+state, okResult("succeeded")); err != nil {
			t.Fatal(err)
		}
		if _, err := b.s.RetryJob(b.ctx, workspaceA, j.ID, l.Job.Attempt); !errors.Is(err, behavior.ErrConflict) {
			t.Fatalf("%s: retry of a succeeded job: %v", state, err)
		}
	}
	if _, err := b.s.RetryJob(b.ctx, workspaceA, "garbage", 1); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("garbage job id: %v", err)
	}
	if _, err := b.s.RetryJob(b.ctx, workspaceA, "00000000-0000-0000-0000-000000000000", 1); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("unknown job: %v", err)
	}
}

// B3-DB5 (REQ-11): a retry re-delivers a follow-up given after the payload
// was built (the route's delivered version goes back to the payload's).
func TestB3Store_RetryRedeliversFollowups(t *testing.T) {
	b := newB3Env(t)
	d, _ := b.enroll(t, workspaceA, "laptop", "repo.write")
	ex, j := b.endedJob(t, d, behavior.WorkerJobFailed)
	b.followUp(t, ex, "also update the changelog")
	if err := b.tasks.SetInstructionVersion(b.ctx, workspaceA, ex.ID, 2); err != nil {
		t.Fatal(err)
	}
	// The delivered version only moves forward.
	if err := b.tasks.SetInstructionVersion(b.ctx, workspaceA, ex.ID, 1); err != nil {
		t.Fatal(err)
	}
	if r, _ := b.tasks.GetRoute(b.ctx, workspaceA, ex.ID); r.InstructionVersion != 2 {
		t.Fatalf("delivered version went backwards: %d", r.InstructionVersion)
	}
	if _, err := b.s.RetryJob(b.ctx, workspaceA, j.ID, j.Attempt); err != nil {
		t.Fatal(err)
	}
	if r, _ := b.tasks.GetRoute(b.ctx, workspaceA, ex.ID); r.InstructionVersion != 1 {
		t.Fatalf("retry kept version %d; the follow-up would be lost", r.InstructionVersion)
	}
	src := &behavior.LocalTaskInstructions{Routes: b.tasks, Reader: b.conv}
	got, err := src.PendingWorkerInstructions(b.ctx, b.jobOf(t, ex))
	if err != nil || len(got) != 1 || got[0].ID != "v2" || got[0].Text != "also update the changelog" {
		t.Fatalf("pending after retry: %+v %v", got, err)
	}
}

// B3-DB6 (REQ-11): once the workflow execution ended, Retry is refused with
// ErrWorkerExecutionEnded (which is an ErrConflict) and the job is untouched.
func TestB3Store_RetryOnEndedExecution(t *testing.T) {
	b := newB3Env(t)
	d, _ := b.enroll(t, workspaceA, "laptop", "repo.write")
	ex, j := b.endedJob(t, d, behavior.WorkerJobFailed)
	if _, err := b.wf.store.CancelExecution(b.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: ex.ID, IdempotencyKey: "b3-cancel"}); err != nil {
		t.Fatal(err)
	}
	if st := b.execState(ex); st.State != workflow.StateCancelled {
		t.Fatalf("execution %s", st.State)
	}
	_, err := b.s.RetryJob(b.ctx, workspaceA, j.ID, j.Attempt)
	if !errors.Is(err, behavior.ErrWorkerExecutionEnded) || !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("retry on an ended execution: %v", err)
	}
	if got := b.jobOf(t, ex); got.State != behavior.WorkerJobFailed {
		t.Fatalf("job changed to %s", got.State)
	}
	if hasEvent(b.events(t), "retried:") {
		t.Fatal("refused retry was audited as a retry")
	}
}

// B3-DB7 (REQ-4): the sweeper cancels the unfinished jobs of ended
// executions (waiting, leased, interrupted) and leaves live ones alone.
func TestB3Store_SyncCancelsJobsOfEndedExecutions(t *testing.T) {
	b := newB3Env(t)
	one, _ := b.enroll(t, workspaceA, "one", "repo.write")
	two, _ := b.enroll(t, workspaceA, "two", "repo.write")
	cancel := func(ex workflow.Execution, key string) {
		t.Helper()
		if _, err := b.wf.store.CancelExecution(b.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: ex.ID, IdempotencyKey: key}); err != nil {
			t.Fatal(err)
		}
	}

	waitEx, _ := b.waitingTask(t, "waiting job")
	cancel(waitEx, "w")
	b.sync(t)
	if got := b.jobOf(t, waitEx); got.State != behavior.WorkerJobCancelled {
		t.Fatalf("waiting job of an ended execution: %s", got.State)
	}

	leasedEx, leasedJob := b.waitingTask(t, "leased job")
	if l := b.mustLease(t, one); l.Job.ID != leasedJob.ID {
		t.Fatalf("leased %s", l.Job.ID)
	}
	cancel(leasedEx, "l")
	b.sync(t)
	if got := b.jobOf(t, leasedEx); got.State != behavior.WorkerJobCancelling {
		t.Fatalf("leased job of an ended execution: %s, want CANCELLING", got.State)
	}

	intEx, intJob := b.waitingTask(t, "interrupted job")
	if l := b.mustLease(t, two); l.Job.ID != intJob.ID {
		t.Fatalf("leased %s", l.Job.ID)
	}
	b.advance(11 * time.Minute)
	if _, err := b.s.ExpireLeases(b.ctx); err != nil {
		t.Fatal(err)
	}
	if got := b.jobOf(t, intEx); got.State != behavior.WorkerJobInterrupted {
		t.Fatalf("job is %s", got.State)
	}
	cancel(intEx, "i")

	liveEx, _ := b.waitingTask(t, "live job")
	b.sync(t)
	if got := b.jobOf(t, intEx); got.State != behavior.WorkerJobCancelled {
		t.Fatalf("interrupted job of an ended execution: %s", got.State)
	}
	if got := b.jobOf(t, liveEx); got.State != behavior.WorkerJobWaiting {
		t.Fatalf("job of a live execution touched: %s", got.State)
	}
	b.sync(t) // idempotent
}

// B3-DB8 (REQ-4, REQ-11): follow-ups ride on the heartbeat until the worker
// acknowledges them; malformed acknowledgements are refused with 400 and
// move nothing.
func TestB3Store_FollowupsOverHeartbeat(t *testing.T) {
	b := newB3Env(t)
	h := httpdelivery.NewWorkerHandler(b.s, b.s).WithLeases(b.s, &behavior.LocalTaskInstructions{Routes: b.tasks, Reader: b.conv})
	ec := echo.New()
	h.Register(ec)
	srv := httptest.NewServer(ec)
	t.Cleanup(srv.Close)
	hub := &w7Hub{w7Env: b.w7Env, srv: srv}

	_, tok := b.enroll(t, workspaceA, "laptop", "repo.write")
	ex, j := b.waitingTask(t, "initial instruction")
	r := hub.poll(t, tok, 0, nil)
	if r.code != 200 || r.body["job_id"] != j.ID || !strings.Contains(r.raw, "initial instruction") {
		t.Fatalf("poll: %d %s", r.code, r.raw)
	}
	lt, _ := r.body["lease_token"].(string)
	hbPath := "/worker/v1/jobs/" + j.ID + "/heartbeat"
	pending := func(body map[string]any) []string {
		t.Helper()
		resp := hub.do(t, hbPath, tok, body)
		if resp.code != 200 {
			t.Fatalf("heartbeat %v: %d %s", body, resp.code, resp.raw)
		}
		list, _ := resp.body["instructions"].([]any)
		var out []string
		for _, it := range list {
			m := it.(map[string]any)
			out = append(out, m["id"].(string)+"="+m["text"].(string))
		}
		return out
	}
	if got := pending(map[string]any{"lease_token": lt}); len(got) != 0 {
		t.Fatalf("instruction already in the payload sent again: %v", got)
	}
	b.followUp(t, ex, "add a test")
	b.followUp(t, ex, "and update docs")
	want := "v2=add a test,v3=and update docs"
	for i := 0; i < 2; i++ {
		if got := strings.Join(pending(map[string]any{"lease_token": lt}), ","); got != want {
			t.Fatalf("heartbeat %d returned %q, want %q (resent until acked)", i, got, want)
		}
	}
	// Bad acknowledgements: 400, and nothing is acknowledged.
	long := make([]string, 65)
	for i := range long {
		long[i] = "v1"
	}
	for name, ids := range map[string][]string{
		"65 ids": long, "zero": {"v0"}, "bare number": {"2"}, "suffix": {"v2x"}, "upper": {"V2"},
		"19 digits": {"v1234567890123456789"}, "empty": {""}, "negative": {"v-2"},
	} {
		if resp := hub.do(t, hbPath, tok, map[string]any{"lease_token": lt, "ack_instruction_ids": ids}); resp.code != 400 {
			t.Fatalf("%s: status %d %s", name, resp.code, resp.raw)
		}
	}
	if route, _ := b.tasks.GetRoute(b.ctx, workspaceA, ex.ID); route.InstructionVersion != 1 {
		t.Fatalf("a refused acknowledgement moved the version to %d", route.InstructionVersion)
	}
	// 64 ids is the limit and is accepted; an id of no instruction moves nothing.
	ok := make([]string, 64)
	for i := range ok {
		ok[i] = "v99"
	}
	if got := strings.Join(pending(map[string]any{"lease_token": lt, "ack_instruction_ids": ok}), ","); got != want {
		t.Fatalf("unknown ids acked something: %q", got)
	}
	// Acknowledging v2 leaves v3.
	if got := strings.Join(pending(map[string]any{"lease_token": lt, "ack_instruction_ids": []string{"v2"}}), ","); got != "v3=and update docs" {
		t.Fatalf("after ack v2: %q", got)
	}
	if got := pending(map[string]any{"lease_token": lt, "ack_instruction_ids": []string{"v3", "v2"}}); len(got) != 0 {
		t.Fatalf("after ack v3: %v", got)
	}
	if route, _ := b.tasks.GetRoute(b.ctx, workspaceA, ex.ID); route.InstructionVersion != 3 {
		t.Fatalf("route version %d, want 3", route.InstructionVersion)
	}
	// A stale ack never moves the version back.
	pending(map[string]any{"lease_token": lt, "ack_instruction_ids": []string{"v1"}})
	if route, _ := b.tasks.GetRoute(b.ctx, workspaceA, ex.ID); route.InstructionVersion != 3 {
		t.Fatalf("stale ack moved the version to %d", route.InstructionVersion)
	}
}
