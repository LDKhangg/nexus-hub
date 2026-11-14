package storage

import (
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// TC-B30 (REQ-5, REQ-11): a behavior run attempt's context snapshot,
// usage and run actions are written under the job fence; after the lease is
// lost, the new attempt sees the checkpointed context (so the adapter does
// not collect it or ask the model again), the old claimant can write
// nothing, and usage is counted once on the run and on the step.
func TestBehaviorRecoveryDoesNotRepeatModelOrAction(t *testing.T) {
	e := newWFEnv(t)
	br := e.runExecution(workspaceA, "recover", "task:recover", nil)
	job := e.claim(e.base)
	f := fenceOf(job, e.base)
	if _, err := e.behaviors.BeginDurableRunAttempt(e.ctx, f, br.run.ID, e.base.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	cp := workflow.Checkpoint{References: []workflow.CheckpointReference{
		{Kind: behavior.CheckpointRun, ReferenceID: strconv.FormatInt(br.run.ID, 10), OperationKey: behavior.RunOperationKey(br.run.ID)},
		{Kind: behavior.CheckpointRunContext, ReferenceID: strconv.FormatInt(br.run.ID, 10)},
	}}
	if err := e.behaviors.SetDurableRunContext(e.ctx, f, br.run.ID, json.RawMessage(`{"task":"t"}`), cp); err != nil {
		t.Fatal(err)
	}
	if err := e.behaviors.AddDurableRunUsage(e.ctx, f, br.run.ID, behavior.Usage{InputTokens: 40, OutputTokens: 4}); err != nil {
		t.Fatal(err)
	}
	if err := e.behaviors.SetDurableRunActions(e.ctx, f, br.run.ID, json.RawMessage(`{"actions":[{"i":0,"type":"telegram_message","status":"delivered"}]}`)); err != nil {
		t.Fatal(err)
	}
	// The worker dies; its lease ends and the job is claimed again.
	later := e.base.Add(16 * time.Minute)
	e.reconcile(later)
	again := e.claim(later.Add(time.Minute))
	if again.ExecutionID != br.ex.ID || again.StepKey != job.StepKey || again.Attempts != job.Attempts+1 {
		t.Fatalf("TC-B30: reclaimed %+v", again)
	}
	step := e.step(workspaceA, br.ex.ID, job.StepKey)
	if step.Checkpoint == nil || len(step.Checkpoint.References) != 2 || step.Checkpoint.References[1].Kind != behavior.CheckpointRunContext {
		t.Fatalf("TC-B30: the new attempt cannot see the stored context: %+v", step.Checkpoint)
	}
	// The old claimant is fenced out of every write.
	old := fenceOf(job, later.Add(2*time.Minute))
	for name, err := range map[string]error{
		"usage":   e.behaviors.AddDurableRunUsage(e.ctx, old, br.run.ID, behavior.Usage{InputTokens: 40}),
		"context": e.behaviors.SetDurableRunContext(e.ctx, old, br.run.ID, json.RawMessage(`{}`), cp),
		"actions": e.behaviors.SetDurableRunActions(e.ctx, old, br.run.ID, json.RawMessage(`{"actions":[]}`)),
		"finish":  e.behaviors.FinishDurableRun(e.ctx, old, br.run.ID, behavior.RunDone, ""),
		"items":   func() error { _, err := e.behaviors.AddDurableRunItems(e.ctx, old, br.run.ID, nil); return err }(),
	} {
		if !errors.Is(err, behavior.ErrWorkflowFence) {
			t.Errorf("TC-B30: stale %s write: %v", name, err)
		}
	}
	if err := e.store.AuthorizeAction(e.ctx, job.ID, job.WorkspaceID, job.ClaimToken, later.Add(2*time.Minute)); !errors.Is(err, workflow.ErrStaleClaim) {
		t.Fatalf("TC-B30: stale claim authorized an action: %v", err)
	}
	var in, out int
	var actions string
	_ = e.pool.QueryRow(e.ctx, `SELECT input_tokens, output_tokens, run_actions::text FROM behavior_runs WHERE id=$1`, br.run.ID).Scan(&in, &out, &actions)
	if in != 40 || out != 4 {
		t.Fatalf("TC-B30: run usage %d/%d, want 40/4", in, out)
	}
	if step.Usage.InputTokens != 40 || step.Usage.OutputTokens != 4 {
		t.Fatalf("TC-B30: step usage %+v, want 40/4", step.Usage)
	}
	if got := behavior.DecodeRunActions(json.RawMessage(actions)).Actions; len(got) != 1 {
		t.Fatalf("TC-B30: stored run actions lost: %s", actions)
	}
	// The new claimant continues under its own fence.
	if err := e.behaviors.AddDurableRunUsage(e.ctx, fenceOf(again, later.Add(2*time.Minute)), br.run.ID, behavior.Usage{InputTokens: 1}); err != nil {
		t.Fatalf("TC-B30: new claimant write: %v", err)
	}
}

// TC-B31 (REQ-8): a cancel between two actions stops the next one: the
// action gate refuses the old claim, and no durable write of the step lands
// after the cancel.
func TestBehaviorCancelPreventsNextAction(t *testing.T) {
	e := newWFEnv(t)
	br := e.runExecution(workspaceA, "cancel", "task:cancel", nil)
	job := e.claim(e.base)
	f := fenceOf(job, e.base)
	if _, err := e.behaviors.BeginDurableRunAttempt(e.ctx, f, br.run.ID, e.base.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := e.store.AuthorizeAction(e.ctx, job.ID, job.WorkspaceID, job.ClaimToken, e.base); err != nil {
		t.Fatalf("TC-B31: live claim refused: %v", err)
	}
	if _, err := e.store.CancelExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: br.ex.ID, IdempotencyKey: "op-cancel-b31"}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.AuthorizeAction(e.ctx, job.ID, job.WorkspaceID, job.ClaimToken, e.base.Add(2*time.Second)); !errors.Is(err, workflow.ErrStaleClaim) {
		t.Fatalf("TC-B31: next action authorized after cancel: %v", err)
	}
	f.At = e.base.Add(2 * time.Second)
	if err := e.behaviors.SetDurableRunActions(e.ctx, f, br.run.ID, json.RawMessage(`{"actions":[]}`)); !errors.Is(err, behavior.ErrWorkflowFence) {
		t.Fatalf("TC-B31: run actions written after cancel: %v", err)
	}
	if err := e.behaviors.FinishDurableRun(e.ctx, f, br.run.ID, behavior.RunDone, ""); !errors.Is(err, behavior.ErrWorkflowFence) {
		t.Fatalf("TC-B31: run finished done after cancel: %v", err)
	}
	assertState(t, e.execution(workspaceA, br.ex.ID), workflow.StateCancelled, "")
}

// TC-B32 (REQ-11, a36860b): durable run writes, job checkpoints, the
// reconciler and cancel take locks in one order: no deadlock under races.
func TestBehaviorDurableWritesLockOrder(t *testing.T) {
	e := newWFEnv(t)
	for round := 0; round < 8; round++ {
		br := e.runExecution(workspaceA, "locks", "task:locks-"+strconv.Itoa(round), nil)
		job := e.claim(e.base)
		f := fenceOf(job, e.base)
		if _, err := e.behaviors.BeginDurableRunAttempt(e.ctx, f, br.run.ID, e.base.Add(10*time.Minute)); err != nil {
			t.Fatal(err)
		}
		errs := make([]error, 4)
		parallel(4, func(i int) {
			switch i {
			case 0:
				errs[i] = e.behaviors.AddDurableRunUsage(e.ctx, f, br.run.ID, behavior.Usage{InputTokens: 1})
			case 1:
				errs[i] = e.store.ReconcileJobs(e.ctx, e.base.Add(time.Second))
			case 2:
				_, errs[i] = e.store.CancelExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: br.ex.ID, IdempotencyKey: "op-c-" + strconv.Itoa(round)})
			case 3:
				errs[i] = e.behaviors.SetDurableRunActions(e.ctx, f, br.run.ID, json.RawMessage(`{"actions":[]}`))
			}
		})
		for i, err := range errs {
			if isDeadlock(err) {
				t.Fatalf("TC-B32 round %d op %d: %v", round, i, err)
			}
			if err != nil && !errors.Is(err, behavior.ErrWorkflowFence) && !errors.Is(err, workflow.ErrNotEligible) {
				t.Fatalf("TC-B32 round %d op %d: unexpected %v", round, i, err)
			}
		}
	}
}
