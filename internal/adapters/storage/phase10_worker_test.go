package storage

import (
	"errors"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Phase 10 Batch A, K1 (REQ-11, REQ-12): job controls and the WAITING_FOR_WORKER state in PostgreSQL.

// TC-K3c (REQ-12, K1 ruling 3): Cancel on a FAILED or NEEDS_INPUT job (also a /task job) cancels it at once;
// the step wakes and the execution ends with worker_cancelled. A finished job stays a conflict.
func TestP10Store_CancelFailedOrNeedsInputJob(t *testing.T) {
	b := newB3Env(t)
	d, _ := b.enroll(t, workspaceA, "laptop", "repo.write")
	for _, state := range []string{behavior.WorkerJobFailed, behavior.WorkerJobNeedsInput} {
		t.Run(state, func(t *testing.T) {
			ex, j := b.endedJob(t, d, state)
			got, err := b.s.CancelJob(b.ctx, workspaceA, j.ID)
			if err != nil || got.State != behavior.WorkerJobCancelled {
				t.Fatalf("cancel %s job: %+v %v", state, got, err)
			}
			if _, err := b.s.CancelJob(b.ctx, workspaceB, j.ID); !errors.Is(err, behavior.ErrNotFound) {
				t.Fatalf("cancel from another workspace: %v", err)
			}
			if _, err := b.s.CancelJob(b.ctx, workspaceA, j.ID); !errors.Is(err, behavior.ErrConflict) {
				t.Fatalf("second cancel: %v", err)
			}
			b.sync(t)
			b.runStep(t, true)
			if st := b.execState(ex); st.State != workflow.StateFailed || string(st.Reason) != behavior.CodeWorkerCancelled {
				t.Fatalf("execution %s reason %q, want FAILED worker_cancelled", st.State, st.Reason)
			}
		})
	}
	// A succeeded job cannot be cancelled.
	ex, j := b.waitingTask(t, "succeed")
	l := b.mustLease(t, d)
	if _, _, err := b.s.ReportResult(b.ctx, d, j.ID, l.LeaseToken, "k-ok", okResult("succeeded")); err != nil {
		t.Fatal(err)
	}
	_ = ex
	if _, err := b.s.CancelJob(b.ctx, workspaceA, j.ID); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("cancel of a succeeded job: %v", err)
	}
}

// TC-K3a (REQ-12): the orchestrated route is stored per execution, validated, and a later job resets the progress message.
func TestP10Store_EnsureOrchestratedRoute(t *testing.T) {
	b := newB3Env(t)
	ex := b.start(t, "việc cần làm")
	mustExec(t, b.ctx, b.pool, `UPDATE worker_task_routes SET progress_message_id=55, progress_job_attempt=2, waiting_notice='x' WHERE execution_id=$1`, ex.ID)
	route := behavior.LocalTaskRoute{WorkspaceID: workspaceA, ExecutionID: ex.ID, IntegrationID: integrationA, ChatID: 7001, ThreadID: 5,
		Repo: "acme/api", TaskRef: "FC-9", ClickUpTaskID: "86new", InstructionVersion: 3}
	if err := b.tasks.EnsureOrchestratedRoute(b.ctx, route); err != nil {
		t.Fatal(err)
	}
	got, err := b.tasks.GetRoute(b.ctx, workspaceA, ex.ID)
	if err != nil || got.Repo != "acme/api" || got.ProgressMessageID != 0 || got.ProgressAttempt != 0 || got.WaitingNotice != "" || got.InstructionVersion != 3 {
		t.Fatalf("route %+v %v (a new job starts a new progress message)", got, err)
	}
	if n := b.wf.count(`SELECT count(*) FROM worker_task_routes WHERE execution_id=$1`, ex.ID); n != 1 {
		t.Fatalf("%d routes for one execution", n)
	}
	for name, mod := range map[string]func(*behavior.LocalTaskRoute){
		"bad repo":         func(r *behavior.LocalTaskRoute) { r.Repo = "no-slash" },
		"no execution":     func(r *behavior.LocalTaskRoute) { r.ExecutionID = 0 },
		"no chat":          func(r *behavior.LocalTaskRoute) { r.ChatID = 0 },
		"negative thread":  func(r *behavior.LocalTaskRoute) { r.ThreadID = -1 },
		"negative version": func(r *behavior.LocalTaskRoute) { r.InstructionVersion = -1 },
		"bad workspace":    func(r *behavior.LocalTaskRoute) { r.WorkspaceID = "nope" },
	} {
		r := route
		mod(&r)
		if err := b.tasks.EnsureOrchestratedRoute(b.ctx, r); !errors.Is(err, behavior.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TC-K2f (REQ-11): migration 00033 accepts WAITING_FOR_WORKER; Down rewrites it (executions and history) to
// WAITING_FOR_USER and the old checks hold; Up accepts it again.
func TestP10Migration00033DownUp(t *testing.T) {
	e := newB2Env(t)
	vid := e.enabledVersion(workspaceA, "p10").ID
	ex := e.start(workspaceA, vid, "p10-wait", chainDef("a"))
	mustExec(t, e.ctx, e.pool, `UPDATE workflow_executions SET state='WAITING_FOR_WORKER' WHERE id=$1`, ex.ID)
	mustExec(t, e.ctx, e.pool, `ALTER TABLE execution_events DISABLE TRIGGER execution_events_no_update`)
	mustExec(t, e.ctx, e.pool, `UPDATE execution_events SET to_state='WAITING_FOR_WORKER' WHERE id=(SELECT max(id) FROM execution_events WHERE execution_id=$1)`, ex.ID)
	mustExec(t, e.ctx, e.pool, `ALTER TABLE execution_events ENABLE TRIGGER execution_events_no_update`)
	mustExec(t, e.ctx, e.pool, downSection(t, "00033_workflow_waiting_for_worker.sql"))
	if st := e.execution(workspaceA, ex.ID); st.State != workflow.StateWaitingForUser {
		t.Fatalf("Down left the execution in %s", st.State)
	}
	if n := e.count(`SELECT count(*) FROM execution_events WHERE from_state='WAITING_FOR_WORKER' OR to_state='WAITING_FOR_WORKER'`); n != 0 {
		t.Fatalf("Down left %d history rows", n)
	}
	_, err := e.pool.Exec(e.ctx, `UPDATE workflow_executions SET state='WAITING_FOR_WORKER' WHERE id=$1`, ex.ID)
	requirePGCode(t, err, "23514")
	mustExec(t, e.ctx, e.pool, upSection(t, "00033_workflow_waiting_for_worker.sql"))
	mustExec(t, e.ctx, e.pool, `UPDATE workflow_executions SET state='WAITING_FOR_WORKER' WHERE id=$1`, ex.ID)
	if st := e.execution(workspaceA, ex.ID); st.State != workflow.StateWaitingForWorker {
		t.Fatalf("after Up the execution is %s", st.State)
	}
	_, err = e.pool.Exec(e.ctx, `UPDATE workflow_executions SET state='WAITING_FOR_NOBODY' WHERE id=$1`, ex.ID)
	requirePGCode(t, err, "23514")
}
