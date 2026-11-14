package storage

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Batch B fixtures: a stored behavior run bound to a durable execution, its
// items, and blocking approvals created under the step's job fence.

type bRun struct {
	version behavior.Version
	run     behavior.Run
	ex      workflow.Execution
}

// runExecution publishes name (if needed), stores a run of it and starts the
// run's execution with def (chainDef when nil). The execution references the
// run as its legacy run, like behavior.WorkflowStartRequest.
func (e *wfEnv) runExecution(ws, name, subject string, def *workflow.Definition) bRun {
	e.t.Helper()
	v, err := e.behaviors.GetVersion(e.ctx, ws, name, 0)
	if err != nil {
		v = e.enabledVersion(ws, name)
	}
	run, created, err := e.behaviors.CreateRun(e.ctx, ws, behavior.NewRun{
		BehaviorID: v.BehaviorID, VersionID: v.ID, Version: v.Version, TriggerKind: behavior.TriggerCommentAdded,
		Trigger: []byte(`{}`), Subject: subject, Revision: "r1", RunAt: time.Now().Add(-time.Minute),
	})
	if err != nil || !created {
		e.t.Fatalf("create run %s: created=%v err=%v", subject, created, err)
	}
	d := chainDef("s1")
	if def != nil {
		d = *def
	}
	req := behavior.WorkflowStartRequest(run, startable(d))
	req.At = e.base.Add(-time.Second)
	ex, err := e.store.StartExecution(e.ctx, req)
	if err != nil {
		e.t.Fatalf("start run execution: %v", err)
	}
	return bRun{version: v, run: run, ex: ex}
}

func (e *wfEnv) addItem(ws string, runID int64, key string, class behavior.Class) behavior.Item {
	e.t.Helper()
	it, _, err := e.behaviors.AddItem(e.ctx, ws, behavior.NewItem{
		RunID: runID, Key: key, Label: "label " + key, Text: "text " + key, Confidence: 0.9,
		Class: class, Rule: "test", Status: behavior.ItemDrafted,
	})
	if err != nil {
		e.t.Fatalf("add item %s: %v", key, err)
	}
	return it
}

func fenceOf(job *workflow.Job, at time.Time) behavior.RunFence {
	return behavior.RunFence{JobID: job.ID, WorkspaceID: job.WorkspaceID, ExecutionID: job.ExecutionID,
		StepKey: job.StepKey, Attempt: job.Attempts, ClaimToken: job.ClaimToken, At: at}
}

func nonce(tag string) string {
	return strings.ReplaceAll(fmt.Sprintf("n-%s-%s", tag, uuid.NewString()), "-", "_")[:40]
}

// waitApproval creates a blocking approval of kind for the item under the
// job's fence.
func (e *wfEnv) waitApproval(job *workflow.Job, at time.Time, kind behavior.ApprovalKind, runID, itemID int64) behavior.Approval {
	e.t.Helper()
	a, err := e.behaviors.CreateWaitApproval(e.ctx, fenceOf(job, at), behavior.NewApproval{
		Kind: kind, RunID: runID, ItemID: itemID, Action: "auto", Resource: "task", Nonce: nonce("w"),
		ExpiresAt: at.Add(24 * time.Hour),
	})
	if err != nil {
		e.t.Fatalf("create wait approval: %v", err)
	}
	return a
}

// waitOn ends the job's attempt in a wait on the given approvals.
func (e *wfEnv) waitOn(job *workflow.Job, at time.Time, state workflow.State, approvals ...behavior.Approval) error {
	e.t.Helper()
	req := resultReq(job, at, nil, workflow.Usage{})
	req.WaitState, req.Reason = state, workflow.ReasonStepWaiting
	for _, a := range approvals {
		req.WaitReferences = append(req.WaitReferences, workflow.CheckpointReference{Kind: behavior.CheckpointApprovalWait, ReferenceID: fmt.Sprint(a.ID)})
	}
	return e.store.WaitJob(e.ctx, req)
}

func (e *wfEnv) decide(ws string, a behavior.Approval, d behavior.Decision, at time.Time) behavior.Approval {
	e.t.Helper()
	out, err := e.behaviors.Decide(e.ctx, ws, a.Nonce, d, "", "owner", at)
	if err != nil {
		e.t.Fatalf("decide approval %d: %v", a.ID, err)
	}
	return out
}

func (e *wfEnv) signalCount(executionID int64) int {
	return e.count(`SELECT count(*) FROM workflow_signals WHERE execution_id=$1`, executionID)
}

func isDeadlock(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && (pg.Code == "40P01" || pg.Code == "40001")
}

func (e *wfEnv) reconcile(at time.Time) {
	e.t.Helper()
	if err := e.store.ReconcileJobs(e.ctx, at); err != nil {
		e.t.Fatalf("reconcile at %v: %v", at, err)
	}
}

func assertState(t *testing.T, ex workflow.Execution, state workflow.State, reason workflow.ReasonCode) {
	t.Helper()
	if ex.State != state || (reason != "" && ex.Reason != reason) {
		t.Fatalf("execution %d: %s/%s, want %s/%s", ex.ID, ex.State, ex.Reason, state, reason)
	}
}
