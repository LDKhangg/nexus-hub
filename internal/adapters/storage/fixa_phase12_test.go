package storage

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/google/uuid"
)

// runExecutionAt is runExecution with the trigger kind and the due time of
// the first job chosen by the test.
func (e *wfEnv) runExecutionAt(ws, name, subject, kind string, at time.Time) bRun {
	e.t.Helper()
	v, err := e.behaviors.GetVersion(e.ctx, ws, name, 0)
	if err != nil {
		v = e.enabledVersion(ws, name)
	}
	run, created, err := e.behaviors.CreateRun(e.ctx, ws, behavior.NewRun{
		BehaviorID: v.BehaviorID, VersionID: v.ID, Version: v.Version, TriggerKind: kind,
		Trigger: []byte(`{}`), Subject: subject, Revision: "r1", RunAt: time.Now().Add(-time.Minute),
	})
	if err != nil || !created {
		e.t.Fatalf("create run %s: created=%v err=%v", subject, created, err)
	}
	req := behavior.WorkflowStartRequest(run, startable(chainDef("s1")))
	req.At = at
	ex, err := e.store.StartExecution(e.ctx, req)
	if err != nil {
		e.t.Fatalf("start run execution %s: %v", subject, err)
	}
	return bRun{version: v, run: run, ex: ex}
}

// R-A-2 (REQ-7): urgent jobs (resume, local_task, orchestrated_task, the run
// of an owner Telegram command) are claimed before older non-urgent ones, even
// when the backlog is much older; FIFO holds inside each class; a run of any
// other trigger is not urgent.
func TestPhase12ClaimOrderIsUrgentFirstThenFIFO(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "p12-order")
	oldest := e.runExecutionAt(workspaceA, "p12-order", "p12:backlog-1", behavior.TriggerCommentAdded, e.base.Add(-time.Hour))
	older := e.runExecutionAt(workspaceA, "p12-order", "p12:backlog-2", behavior.TriggerChatMessage, e.base.Add(-30*time.Minute))
	command := e.runExecutionAt(workspaceA, "p12-order", "p12:command", behavior.TriggerTelegramCommand, e.base.Add(-time.Second))
	local := e.start(workspaceA, v.ID, "p12-order-local-"+uuid.NewString(), chainDef("a"))
	orch := e.start(workspaceA, v.ID, "p12-order-orch-"+uuid.NewString(), chainDef("a"))
	resume := e.start(workspaceA, v.ID, "p12-order-resume-"+uuid.NewString(), chainDef("a"))
	mustExec(t, e.ctx, e.pool, `UPDATE workflow_executions SET source_kind='local_task' WHERE id=$1`, local.ID)
	mustExec(t, e.ctx, e.pool, `UPDATE workflow_executions SET source_kind='orchestrated_task' WHERE id=$1`, orch.ID)
	mustExec(t, e.ctx, e.pool, `UPDATE jobs SET continues_attempt=true WHERE execution_id=$1`, resume.ID)

	var order []int64
	for i := 0; i < 6; i++ {
		order = append(order, e.claim(e.base).ExecutionID)
	}
	urgent := map[int64]bool{local.ID: true, orch.ID: true, resume.ID: true, command.ex.ID: true}
	for i := 0; i < 4; i++ {
		if !urgent[order[i]] {
			t.Fatalf("R-A-2: claim %d is execution %d, want an urgent one first (order %v)", i+1, order[i], order)
		}
	}
	if order[4] != oldest.ex.ID || order[5] != older.ex.ID {
		t.Fatalf("R-A-2: backlog order %v, want %d then %d (FIFO, non-urgent triggers stay in the backlog)", order[4:], oldest.ex.ID, older.ex.ID)
	}
}

// R-A-2 (REQ-7): the urgent-first order keeps SKIP LOCKED: an urgent job locked
// by another transaction does not block the claim of the next job, and is
// claimed as soon as the lock is gone.
func TestPhase12UrgentJobLockedByOtherTxDoesNotBlockClaim(t *testing.T) {
	e := newWFEnv(t)
	backlog := e.runExecutionAt(workspaceA, "p12-skip", "p12:backlog", behavior.TriggerCommentAdded, e.base.Add(-time.Hour))
	command := e.runExecutionAt(workspaceA, "p12-skip", "p12:command", behavior.TriggerTelegramCommand, e.base.Add(-time.Second))
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(e.ctx, `SELECT id FROM jobs WHERE execution_id=$1 FOR UPDATE`, command.ex.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan *workflow.Job, 1)
	errc := make(chan error, 1)
	go func() {
		j, err := e.store.ClaimJob(e.ctx, e.base)
		done <- j
		errc <- err
	}()
	select {
	case j := <-done:
		if err := <-errc; err != nil || j == nil || j.ExecutionID != backlog.ex.ID {
			_ = tx.Rollback(e.ctx)
			t.Fatalf("R-A-2: claim while the urgent job is locked: %#v %v", j, err)
		}
	case <-time.After(10 * time.Second):
		_ = tx.Rollback(e.ctx)
		t.Fatal("R-A-2: the claim blocked on a locked urgent job")
	}
	if err := tx.Rollback(e.ctx); err != nil {
		t.Fatal(err)
	}
	if j := e.claim(e.base); j.ExecutionID != command.ex.ID {
		t.Fatalf("R-A-2: after unlock claimed %d, want the urgent %d", j.ExecutionID, command.ex.ID)
	}
}

// TC-9 (REQ-3, known provider limit): Telegram send has no idempotency key.
// When the hub dies between the send and the mark-sent write, the notice is
// sent AGAIN once after restart (at least once, never lost). This test pins
// that owner-chosen behaviour; a future provider-side dedup must update it.
func TestPhase12TelegramSendIsAtLeastOnceAcrossCrash(t *testing.T) {
	ctx, pool, r := newNoticeDB(t)
	if _, err := r.Accept(ctx, []domain.NormalizedEvent{noticeEvent("p12-alo", "task-1")}); err != nil {
		t.Fatal(err)
	}
	// First process: the send succeeds, then the process is cut before MarkSent.
	hard, hardCancel := context.WithCancel(ctx)
	defer hardCancel()
	crashing := &phase12Sender{onSend: func(context.Context) { hardCancel() }}
	w1 := relay.NewNoticeWorker(r, crashing, relay.WorkerSettings{PollInterval: 10 * time.Millisecond})
	if err := w1.RunDrain(hard, hard); err != nil {
		t.Fatal(err)
	}
	if n := crashing.calls.Load(); n != 1 {
		t.Fatalf("TC-9: first process sent %d times", n)
	}
	if n := outboxSent(t, ctx, r); n != 0 {
		t.Fatalf("TC-9: setup: notice already marked sent")
	}
	// Restart after the claim lease lapsed.
	mustExec(t, ctx, pool, `UPDATE notification_outbox SET locked_until = now() - interval '1 second'`)
	restarted, err := NewNoticeRepository(pool, r.newID, DebounceSettings{})
	if err != nil {
		t.Fatal(err)
	}
	second := &phase12Sender{}
	w2 := relay.NewNoticeWorker(restarted, second, relay.WorkerSettings{PollInterval: 10 * time.Millisecond})
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = w2.Run(wctx) }()
	waitFor(t, "the resend", func() bool { return outboxSent(t, ctx, restarted) == 1 })
	time.Sleep(200 * time.Millisecond)
	cancel()
	wg.Wait()
	if n := second.calls.Load(); n != 1 {
		t.Fatalf("TC-9: restart sent %d times, want exactly one resend (at-least-once)", n)
	}
	if n := countRows(t, ctx, pool, "notification_outbox"); n != 1 {
		t.Fatalf("TC-9: %d outbox rows", n)
	}
}
