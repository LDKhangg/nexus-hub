package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

func schema2Workflow() *behavior.WorkflowDoc {
	return &behavior.WorkflowDoc{
		Steps: []behavior.WorkflowStepDoc{
			{Key: "collect", Kind: workflow.StepKindCollectContext},
			{Key: "model", Kind: workflow.StepKindModel, DependsOn: []string{"collect"}, WorkerCapabilities: []string{workflow.CapabilityInProcess}},
			{Key: "notify", Kind: workflow.StepKindNotify, DependsOn: []string{"model"}},
		},
		Retry:                &behavior.WorkflowRetryDoc{MaxAttempts: intp(3), DelaySeconds: []int{10, 40}},
		StepTimeoutSeconds:   intp(120),
		LeaseSeconds:         intp(300),
		WorkerCapabilities:   []string{workflow.CapabilityInProcess},
		NotificationHandlers: []string{workflow.NotificationHandlerTelegram},
		ApprovalHandlers:     []string{workflow.ApprovalHandlerBehavior},
	}
}

// TC-6 (REQ-1, REQ-2, REQ-3): a published schema 2 version keeps its graph,
// retry, timeout, constraints and handlers after the draft changes.
func TestWorkflowPublishPinsSettings(t *testing.T) {
	e := newWFEnv(t)
	v1 := e.publishEnabled(workspaceA, wfDoc2("flow", schema2Workflow()))
	def1 := e.definitionOf(workspaceA, "flow", 1)
	if def1.Retry.MaxAttempts != 3 || def1.StepTimeout != 2*time.Minute || def1.LeaseDuration != 5*time.Minute ||
		len(def1.Steps) != 3 || def1.NotificationHandlers[0] != "telegram.message" || def1.ApprovalHandlers[0] != "behavior.action" {
		t.Fatalf("TC-6: published definition = %#v", def1)
	}
	ex := e.start(workspaceA, v1.ID, "trigger-pin", def1)
	before, err := e.behaviors.GetVersion(e.ctx, workspaceA, "flow", 1)
	if err != nil {
		t.Fatal(err)
	}

	changed := schema2Workflow()
	changed.Steps = []behavior.WorkflowStepDoc{{Key: "only", Kind: workflow.StepKindNotify}}
	changed.Retry = &behavior.WorkflowRetryDoc{MaxAttempts: intp(2), DelaySeconds: []int{7}}
	changed.StepTimeoutSeconds = intp(30)
	changed.LeaseSeconds = intp(90)
	changed.NotificationHandlers = nil
	changed.ApprovalHandlers = nil
	e.saveDraft(workspaceA, wfDoc2("flow", changed))

	after, err := e.behaviors.GetVersion(e.ctx, workspaceA, "flow", 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(after.Document) != string(before.Document) || !after.PublishedAt.Equal(before.PublishedAt) || after.ID != before.ID {
		t.Fatal("TC-6: draft edit changed the published version row")
	}
	if got := e.definitionOf(workspaceA, "flow", 1); defJSON(t, got) != defJSON(t, def1) {
		t.Fatalf("TC-6: published settings changed:\n%s\n%s", defJSON(t, got), defJSON(t, def1))
	}
	v2 := e.publish(workspaceA, "flow")
	if v2.Version != 2 || v2.ID == v1.ID {
		t.Fatalf("second publish = %#v", v2)
	}
	def2 := e.definitionOf(workspaceA, "flow", 2)
	if def2.Retry.MaxAttempts != 2 || len(def2.Steps) != 1 {
		t.Fatalf("v2 definition = %#v", def2)
	}
	got := e.execution(workspaceA, ex.ID)
	if got.VersionID != v1.ID || defJSON(t, got.Definition) != defJSON(t, startable(def1)) {
		t.Fatalf("TC-6: live execution lost its pin: version=%d def=%s", got.VersionID, defJSON(t, got.Definition))
	}
	if steps := e.steps(workspaceA, ex.ID); len(steps) != 3 || steps[0].Key != "collect" {
		t.Fatalf("TC-6: execution steps changed: %#v", steps)
	}
}

// TC-7 (REQ-3): restore copies an old version into the draft only; publish
// creates a new version and never touches old versions or live executions.
func TestBehaviorRestoreCreatesNewVersion(t *testing.T) {
	e := newWFEnv(t)
	docs := []behavior.Doc{wfDoc("restore-me"), wfDoc("restore-me"), wfDoc("restore-me")}
	docs[0].Description = "version one"
	docs[1].Description = "version two"
	docs[2].Description = "version three 🚀"
	var versions []behavior.Version
	for i, d := range docs {
		if i == 0 {
			versions = append(versions, e.publishEnabled(workspaceA, d))
			continue
		}
		e.saveDraft(workspaceA, d)
		versions = append(versions, e.publish(workspaceA, d.Name))
	}
	def1 := e.definitionOf(workspaceA, "restore-me", 1)
	live := e.start(workspaceA, versions[0].ID, "live-v1", def1)
	snapshot := func() []behavior.Version {
		var out []behavior.Version
		for n := 1; n <= 3; n++ {
			v, err := e.behaviors.GetVersion(e.ctx, workspaceA, "restore-me", n)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, v)
		}
		return out
	}
	old := snapshot()
	liveBefore := e.fingerprint(live.ID)

	b, err := behavior.RestoreDraft(e.ctx, e.behaviors, workspaceA, "restore-me", 1)
	if err != nil {
		t.Fatal(err)
	}
	if b.LatestVersion != 3 {
		t.Fatalf("TC-7: restore published something: latest=%d", b.LatestVersion)
	}
	if string(b.Draft) != string(old[0].Document) || b.DraftSource != old[0].Source {
		t.Fatalf("TC-7: draft is not version 1:\n%s\n%s", b.Draft, old[0].Document)
	}
	if n := e.count(`SELECT count(*) FROM behavior_versions`); n != 3 {
		t.Fatalf("TC-7: restore created versions: %d", n)
	}
	latest, err := e.behaviors.GetVersion(e.ctx, workspaceA, "restore-me", 0)
	if err != nil || latest.Version != 3 {
		t.Fatalf("TC-7: latest-published meaning changed: %#v %v", latest.Version, err)
	}
	enabled, err := e.behaviors.EnabledVersions(e.ctx, workspaceA)
	if err != nil || len(enabled) != 1 || enabled[0].Version != 3 {
		t.Fatalf("TC-7: enabled selection after restore: %#v %v", enabled, err)
	}

	edited := wfDoc("restore-me")
	edited.Description = "version one, edited after restore"
	e.saveDraft(workspaceA, edited)
	v4 := e.publish(workspaceA, "restore-me")
	if v4.Version != 4 || v4.ID == versions[0].ID {
		t.Fatalf("TC-7: publish after restore = %#v", v4)
	}
	for i, v := range snapshot() {
		if string(v.Document) != string(old[i].Document) || v.ID != old[i].ID || !v.PublishedAt.Equal(old[i].PublishedAt) || v.Source != old[i].Source {
			t.Fatalf("TC-7: historical version %d changed", i+1)
		}
	}
	if e.fingerprint(live.ID) != liveBefore {
		t.Fatal("TC-7: restore/publish changed a live execution")
	}

	t.Run("TC-7/restore then publish unchanged is a new version with the old document", func(t *testing.T) {
		if _, err := behavior.RestoreDraft(e.ctx, e.behaviors, workspaceA, "restore-me", 2); err != nil {
			t.Fatal(err)
		}
		v5 := e.publish(workspaceA, "restore-me")
		got, err := e.behaviors.GetVersion(e.ctx, workspaceA, "restore-me", 5)
		if err != nil || v5.Version != 5 || string(got.Document) != string(old[1].Document) || got.ID == old[1].ID {
			t.Fatalf("v5=%#v err=%v", v5, err)
		}
	})

	t.Run("TC-7/bad versions are rejected without touching the draft", func(t *testing.T) {
		before, _ := e.behaviors.GetBehavior(e.ctx, workspaceA, "restore-me")
		for _, n := range []int{0, -1} {
			if _, err := behavior.RestoreDraft(e.ctx, e.behaviors, workspaceA, "restore-me", n); !errors.Is(err, behavior.ErrInvalid) {
				t.Fatalf("restore %d: %v", n, err)
			}
		}
		if _, err := behavior.RestoreDraft(e.ctx, e.behaviors, workspaceA, "restore-me", 99); !errors.Is(err, behavior.ErrNotFound) {
			t.Fatalf("restore missing: %v", err)
		}
		if _, err := behavior.RestoreDraft(e.ctx, e.behaviors, workspaceB, "restore-me", 1); !errors.Is(err, behavior.ErrNotFound) {
			t.Fatalf("restore across workspaces: %v", err)
		}
		after, _ := e.behaviors.GetBehavior(e.ctx, workspaceA, "restore-me")
		if string(after.Draft) != string(before.Draft) {
			t.Fatal("failed restore changed the draft")
		}
	})
}

// TC-9 (REQ-3): unpublished drafts and disabled behaviors start nothing;
// enabled-version selection returns only published, enabled versions.
func TestWorkflowUnpublishedOrDisabledCannotStart(t *testing.T) {
	e := newWFEnv(t)
	def := chainDef("a", "b")
	e.saveDraft(workspaceA, wfDoc("draft-only"))
	if err := e.behaviors.SetStatus(e.ctx, workspaceA, "draft-only", behavior.StatusEnabled); !errors.Is(err, behavior.ErrNotPublished) {
		t.Fatalf("TC-9: enabling an unpublished draft: %v", err)
	}
	for _, id := range []int64{1, 999999} {
		if _, err := e.store.StartExecution(e.ctx, e.startReq(workspaceA, id, fmt.Sprintf("draft-%d", id), def)); err == nil {
			t.Fatalf("TC-9: started from nonexistent version %d", id)
		}
	}
	if n := e.count(`SELECT count(*) FROM workflow_executions`); n != 0 {
		t.Fatalf("TC-9: executions created: %d", n)
	}

	e.saveDraft(workspaceA, wfDoc("toggled"))
	v := e.publish(workspaceA, "toggled")
	if _, err := e.store.StartExecution(e.ctx, e.startReq(workspaceA, v.ID, "never-enabled", def)); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("TC-9: start on never-enabled behavior: %v", err)
	}
	if err := e.behaviors.SetStatus(e.ctx, workspaceA, "toggled", behavior.StatusEnabled); err != nil {
		t.Fatal(err)
	}
	ex := e.start(workspaceA, v.ID, "while-enabled", def)
	if err := e.behaviors.SetStatus(e.ctx, workspaceA, "toggled", behavior.StatusDisabled); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.StartExecution(e.ctx, e.startReq(workspaceA, v.ID, "after-disable", def)); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("TC-9: start on disabled behavior: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM workflow_executions`); n != 1 {
		t.Fatalf("TC-9: want only the enabled-time execution, got %d", n)
	}
	if n := e.count(`SELECT count(*) FROM jobs`); n != 1 {
		t.Fatalf("TC-9: jobs for rejected starts: %d", n)
	}
	if got := e.execution(workspaceA, ex.ID); got.State != workflow.StateQueued {
		t.Fatalf("existing execution changed by disable: %s", got.State)
	}

	t.Run("TC-9/enabled selection is published and enabled only", func(t *testing.T) {
		e.publishEnabled(workspaceA, wfDoc("live"))
		draftEdit := wfDoc("live")
		draftEdit.Description = "unpublished edit"
		e.saveDraft(workspaceA, draftEdit)
		enabled, err := e.behaviors.EnabledVersions(e.ctx, workspaceA)
		if err != nil {
			t.Fatal(err)
		}
		if len(enabled) != 1 || enabled[0].Name != "live" || enabled[0].Version != 1 || strings.Contains(string(enabled[0].Document), "unpublished edit") {
			t.Fatalf("enabled = %#v", enabled)
		}
		if other, err := e.behaviors.EnabledVersions(e.ctx, workspaceB); err != nil || len(other) != 0 {
			t.Fatalf("workspace B sees %#v %v", other, err)
		}
	})
}

// TC-11 (REQ-4): repeated or same-state transitions add no history; events
// have increasing sequences and cannot be changed.
func TestExecutionHistoryIsAppendOnly(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "history")
	ex := e.start(workspaceA, v.ID, "hist-1", chainDef("a", "b"))
	req := workflow.TransitionRequest{WorkspaceID: workspaceA, ExecutionID: ex.ID, From: workflow.StateQueued, To: workflow.StateRunning,
		StepKey: "a", Attempt: 1, Reason: workflow.ReasonAccepted, IdempotencyKey: "move-1"}
	if _, err := e.store.TransitionExecution(e.ctx, req); err != nil {
		t.Fatal(err)
	}
	n := len(e.events(ex.ID))
	if _, err := e.store.TransitionExecution(e.ctx, req); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("TC-11: repeated transition: %v", err)
	}
	same := req
	same.From, same.To, same.IdempotencyKey = workflow.StateRunning, workflow.StateRunning, "same-state"
	got, err := e.store.TransitionExecution(e.ctx, same)
	if err != nil || got.State != workflow.StateRunning {
		t.Fatalf("TC-11: same-state transition: %v %v", got.State, err)
	}
	reused := req
	reused.From, reused.To = workflow.StateRunning, workflow.StateQueued
	if _, err := e.store.TransitionExecution(e.ctx, reused); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("TC-11: reused idempotency key: %v", err)
	}
	if st := e.execution(workspaceA, ex.ID).State; st != workflow.StateRunning {
		t.Fatalf("TC-11: rejected duplicate key still moved state to %s", st)
	}
	events := e.events(ex.ID)
	if len(events) != n {
		t.Fatalf("TC-11: history grew from %d to %d: %#v", n, len(events), events)
	}
	for i, ev := range events {
		if ev.Sequence != int64(i+1) {
			t.Fatalf("TC-11: sequence gap: %#v", events)
		}
	}

	t.Run("TC-11/illegal and stale transitions", func(t *testing.T) {
		bad := req
		bad.From, bad.To, bad.IdempotencyKey = workflow.StateCompleted, workflow.StateRunning, "illegal"
		if _, err := e.store.TransitionExecution(e.ctx, bad); !errors.Is(err, workflow.ErrInvalidInput) {
			t.Fatalf("COMPLETED->RUNNING: %v", err)
		}
		stale := req
		stale.From, stale.To, stale.IdempotencyKey = workflow.StateQueued, workflow.StateCancelled, "stale"
		if _, err := e.store.TransitionExecution(e.ctx, stale); !errors.Is(err, workflow.ErrConflict) {
			t.Fatalf("stale from-state: %v", err)
		}
		if len(e.events(ex.ID)) != n {
			t.Fatal("rejected transitions wrote history")
		}
	})

	t.Run("TC-11/events are immutable", func(t *testing.T) {
		_, err := e.pool.Exec(e.ctx, `UPDATE execution_events SET reason_code='tampered' WHERE execution_id=$1`, ex.ID)
		requirePGCode(t, err, "23514")
		_, err = e.pool.Exec(e.ctx, `DELETE FROM execution_events WHERE execution_id=$1`, ex.ID)
		requirePGCode(t, err, "23514")
		_, err = e.pool.Exec(e.ctx, `INSERT INTO execution_events(workspace_id,execution_id,sequence,kind) VALUES($1,$2,1,'transition')`, workspaceA, ex.ID)
		requirePGCode(t, err, "23505")
		if len(e.events(ex.ID)) != n {
			t.Fatal("history changed")
		}
	})
}

// TC-12 (REQ-4, REQ-5): start returns QUEUED and persists the pin, steps,
// first events and job atomically and in order.
func TestExecutionCheckpointAndHistoryAtomic(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "atomic")
	def := chainDef("a", "b")
	def.Steps = append(def.Steps, workflow.StepDefinition{Key: "c", Kind: workflow.StepKindNotify, DependsOn: []string{"a"}})
	deadline := e.base.Add(time.Hour)
	req := e.startReq(workspaceA, v.ID, "atomic-1", def)
	req.Deadline = &deadline
	ex, err := e.store.StartExecution(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if ex.State != workflow.StateQueued || ex.VersionID != v.ID || ex.TriggerKey != "atomic-1" || ex.WorkspaceID != workspaceA ||
		ex.References != req.References || ex.Deadline == nil || !ex.Deadline.Equal(deadline) || ex.ParentExecutionID != 0 {
		t.Fatalf("TC-12: start result = %#v", ex)
	}
	if defJSON(t, ex.Definition) != defJSON(t, startable(def)) || defJSON(t, e.execution(workspaceA, ex.ID).Definition) != defJSON(t, startable(def)) {
		t.Fatal("TC-12: definition not pinned")
	}
	steps := e.steps(workspaceA, ex.ID)
	if len(steps) != 3 {
		t.Fatalf("steps = %#v", steps)
	}
	for i, s := range steps {
		if s.Position != i || s.Key != def.Steps[i].Key || s.Kind != def.Steps[i].Kind || s.State != workflow.StepPending ||
			s.Attempts != 0 || s.Checkpoint != nil || s.Usage != (workflow.Usage{}) || strings.Join(s.DependsOn, ",") != strings.Join(def.Steps[i].DependsOn, ",") {
			t.Fatalf("TC-12: step %d = %#v", i, s)
		}
	}
	events := e.events(ex.ID)
	if len(events) != 2 || events[0].Sequence != 1 || events[0].Kind != "started" || events[0].To != "RECEIVED" ||
		events[1].Sequence != 2 || events[1].Kind != "transition" || events[1].From != "RECEIVED" || events[1].To != "QUEUED" {
		t.Fatalf("TC-12: events = %#v", events)
	}
	jobs := e.jobs(ex.ID)
	if len(jobs) != 1 || jobs[0].StepKey != "a" || jobs[0].Status != "pending" || jobs[0].Max != 5 || jobs[0].Attempts != 0 {
		t.Fatalf("TC-12: jobs = %s", describeJobs(jobs))
	}

	t.Run("TC-12/rolled-back caller transaction leaves nothing", func(t *testing.T) {
		tx, err := e.pool.Begin(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		inTx, err := e.store.StartExecutionTx(e.ctx, tx, e.startReq(workspaceA, v.ID, "rolled-back", def))
		if err != nil || inTx.State != workflow.StateQueued {
			t.Fatalf("start in tx: %v %v", inTx.State, err)
		}
		if err := tx.Rollback(e.ctx); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"execution_steps", "execution_events", "jobs"} {
			if n := e.count(`SELECT count(*) FROM `+table+` WHERE execution_id = $1`, inTx.ID); n != 0 {
				t.Fatalf("%s kept %d rows after rollback", table, n)
			}
		}
		if n := e.count(`SELECT count(*) FROM workflow_executions WHERE trigger_key='rolled-back'`); n != 0 {
			t.Fatal("execution survived rollback")
		}
	})

	t.Run("TC-12/invalid starts persist nothing", func(t *testing.T) {
		before := e.count(`SELECT count(*) FROM workflow_executions`)
		cyclic := chainDef("a", "b")
		cyclic.Steps[0].DependsOn = []string{"b"}
		bad := []workflow.StartRequest{
			e.startReq(workspaceA, v.ID, "cyclic", cyclic),
			e.startReq(workspaceA, v.ID, "", def),
			e.startReq(workspaceA, v.ID, strings.Repeat("k", 513), def),
			e.startReq("not-a-uuid", v.ID, "bad-ws", def),
		}
		noOp := e.startReq(workspaceA, v.ID, "no-op-key", def)
		noOp.References.OperationKey = " "
		bad = append(bad, noOp)
		badCorr := e.startReq(workspaceA, v.ID, "bad-corr", def)
		badCorr.CorrelationID = "x"
		bad = append(bad, badCorr)
		for i, r := range bad {
			if _, err := e.store.StartExecution(e.ctx, r); !errors.Is(err, workflow.ErrInvalidInput) {
				t.Fatalf("bad start %d: %v", i, err)
			}
		}
		if after := e.count(`SELECT count(*) FROM workflow_executions`); after != before {
			t.Fatal("invalid start persisted rows")
		}
	})
}

// TC-13 (REQ-4): the same trigger key, sequential or concurrent, creates
// exactly one execution and every caller gets it.
func TestExecutionStartDeduplicatesTrigger(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "dedupe")
	def := chainDef("a", "b")
	first := e.start(workspaceA, v.ID, "same-key", def)
	other := chainDef("x")
	second := e.start(workspaceA, v.ID, "same-key", other)
	if second.ID != first.ID || defJSON(t, second.Definition) != defJSON(t, startable(def)) {
		t.Fatalf("TC-13: sequential duplicate = %d (first %d)", second.ID, first.ID)
	}
	const n = 8
	ids := make([]int64, n)
	errs := make([]error, n)
	parallel(n, func(i int) {
		ex, err := e.store.StartExecution(e.ctx, e.startReq(workspaceA, v.ID, "burst-key", def))
		ids[i], errs[i] = ex.ID, err
	})
	for i := range ids {
		if errs[i] != nil || ids[i] == 0 || ids[i] != ids[0] {
			t.Fatalf("TC-13: concurrent callers got ids=%v errs=%v", ids, errs)
		}
	}
	if c := e.count(`SELECT count(*) FROM workflow_executions WHERE trigger_key='burst-key'`); c != 1 {
		t.Fatalf("TC-13: %d executions for one trigger", c)
	}
	if c := len(e.events(ids[0])); c != 2 {
		t.Fatalf("TC-13: duplicate starts wrote history: %d events", c)
	}
	if c := len(e.jobs(ids[0])); c != 1 {
		t.Fatalf("TC-13: duplicate jobs: %d", c)
	}
	vb := e.enabledVersion(workspaceB, "dedupe")
	exB := e.start(workspaceB, vb.ID, "same-key", def)
	if exB.ID == first.ID {
		t.Fatal("TC-13: trigger dedupe crossed workspaces")
	}
}

// TC-14 (REQ-4, REQ-8): cancel is durable and idempotent; terminal
// executions are never reactivated by cancel.
func TestExecutionCancelIsDurable(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "cancel")
	ex := e.start(workspaceA, v.ID, "cancel-1", chainDef("a", "b"))
	got, err := e.store.CancelExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: ex.ID, IdempotencyKey: "cancel:1"})
	if err != nil || got.State != workflow.StateCancelled || got.CancelledAt == nil || got.FinishedAt == nil || got.Reason != workflow.ReasonCancelled {
		t.Fatalf("TC-14: cancel = %#v %v", got, err)
	}
	for _, s := range e.steps(workspaceA, ex.ID) {
		if s.State != workflow.StepCancelled {
			t.Fatalf("TC-14: step %s still %s", s.Key, s.State)
		}
	}
	fp := e.fingerprint(ex.ID)
	for _, key := range []string{"cancel:1", "cancel:2"} {
		again, err := e.store.CancelExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: ex.ID, IdempotencyKey: key})
		if err != nil || again.State != workflow.StateCancelled {
			t.Fatalf("TC-14: repeat cancel %s: %v %v", key, again.State, err)
		}
	}
	if e.fingerprint(ex.ID) != fp {
		t.Fatal("TC-14: repeated cancel changed rows")
	}
	if job, err := e.store.ClaimJob(e.ctx, e.base); err != nil || job != nil {
		t.Fatalf("TC-14: cancelled execution's job claimable: %#v %v", job, err)
	}
	if _, err := e.store.TransitionExecution(e.ctx, workflow.TransitionRequest{WorkspaceID: workspaceA, ExecutionID: ex.ID,
		From: workflow.StateCancelled, To: workflow.StateQueued, IdempotencyKey: "revive"}); err == nil {
		t.Fatal("TC-14: cancelled execution reactivated")
	}
	if _, err := e.store.RetryExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: ex.ID, IdempotencyKey: "retry"}); !errors.Is(err, workflow.ErrNotEligible) {
		t.Fatalf("TC-14: retry of cancelled: %v", err)
	}

	t.Run("TC-14/concurrent cancels write one transition", func(t *testing.T) {
		ex := e.start(workspaceA, v.ID, "cancel-burst", chainDef("a"))
		errs := make([]error, 6)
		parallel(6, func(i int) {
			_, errs[i] = e.store.CancelExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: ex.ID, IdempotencyKey: fmt.Sprintf("c-%d", i)})
		})
		for _, err := range errs {
			if err != nil {
				t.Fatalf("concurrent cancel: %v", errs)
			}
		}
		n := 0
		for _, ev := range e.events(ex.ID) {
			if ev.To == "CANCELLED" {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("cancel transitions = %d", n)
		}
	})

	t.Run("TC-14/terminal executions are not cancelled", func(t *testing.T) {
		done := e.start(workspaceA, v.ID, "done-1", chainDef("a"))
		e.complete(e.claim(e.base), e.base, nil, workflow.Usage{})
		if st := e.execution(workspaceA, done.ID).State; st != workflow.StateCompleted {
			t.Fatalf("setup: %s", st)
		}
		failed := e.start(workspaceA, v.ID, "failed-1", chainDef("a"))
		e.failJob(e.claim(e.base), e.base, workflow.ReasonNonRetryable)
		for _, id := range []int64{done.ID, failed.ID} {
			fp := e.fingerprint(id)
			if _, err := e.store.CancelExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: id, IdempotencyKey: "late-cancel"}); !errors.Is(err, workflow.ErrNotEligible) {
				t.Fatalf("cancel terminal %d: %v", id, err)
			}
			if e.fingerprint(id) != fp {
				t.Fatal("cancel changed a terminal execution")
			}
		}
	})

	t.Run("TC-14/invalid commands", func(t *testing.T) {
		if _, err := e.store.CancelExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: ex.ID, IdempotencyKey: "  "}); !errors.Is(err, workflow.ErrInvalidInput) {
			t.Fatalf("blank key: %v", err)
		}
		if _, err := e.store.CancelExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: 987654, IdempotencyKey: "k"}); !errors.Is(err, workflow.ErrNotFound) {
			t.Fatalf("missing execution: %v", err)
		}
	})
}

// TC-15 (REQ-3, REQ-4, REQ-6): workspace B can neither see nor change A's
// executions, steps, jobs or history; cross-workspace rows are refused.
func TestExecutionWorkspaceIsolation(t *testing.T) {
	e := newWFEnv(t)
	vA := e.enabledVersion(workspaceA, "iso")
	exA := e.start(workspaceA, vA.ID, "iso-a", chainDef("a", "b"))
	job := e.claim(e.base)
	fp := e.fingerprint(exA.ID)
	cmd := workflow.Command{WorkspaceID: workspaceB, ExecutionID: exA.ID, IdempotencyKey: "b-cmd"}

	if _, err := e.store.CancelExecution(e.ctx, cmd); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("TC-15: B cancel: %v", err)
	}
	if _, err := e.store.RetryExecution(e.ctx, cmd); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("TC-15: B retry: %v", err)
	}
	if _, err := e.store.TransitionExecution(e.ctx, workflow.TransitionRequest{WorkspaceID: workspaceB, ExecutionID: exA.ID,
		From: workflow.StateRunning, To: workflow.StateQueued, IdempotencyKey: "b-move"}); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("TC-15: B transition: %v", err)
	}
	cross := *job
	cross.WorkspaceID = workspaceB
	if err := e.store.AuthorizeAction(e.ctx, job.ID, workspaceB, job.ClaimToken, e.base); !errors.Is(err, workflow.ErrStaleClaim) {
		t.Fatalf("TC-15: B authorize: %v", err)
	}
	if _, err := e.store.CheckpointJob(e.ctx, checkpointReq(&cross, e.base, reusable("1"), workflow.Usage{InputTokens: 9}, "b-cp")); !errors.Is(err, workflow.ErrStaleClaim) {
		t.Fatalf("TC-15: B checkpoint: %v", err)
	}
	if _, err := e.store.RecordCheckpoint(e.ctx, workflow.CheckpointRequest{JobID: job.ID, WorkspaceID: workspaceB, ClaimToken: job.ClaimToken, At: e.base,
		ExecutionID: exA.ID, StepKey: "a", ExpectedAttempt: 1, Checkpoint: *reusable("1")}); !errors.Is(err, workflow.ErrStaleClaim) {
		t.Fatalf("TC-15: B record checkpoint: %v", err)
	}
	if err := e.store.CompleteJob(e.ctx, resultReq(&cross, e.base, nil, workflow.Usage{})); !errors.Is(err, workflow.ErrStaleClaim) {
		t.Fatalf("TC-15: B complete: %v", err)
	}
	if err := e.store.FailJob(e.ctx, resultReq(&cross, e.base, nil, workflow.Usage{})); !errors.Is(err, workflow.ErrStaleClaim) {
		t.Fatalf("TC-15: B fail: %v", err)
	}
	if _, err := e.store.StartExecution(e.ctx, e.startReq(workspaceB, vA.ID, "b-uses-a-version", chainDef("a"))); err == nil {
		t.Fatal("TC-15: B started an execution on A's version")
	}
	if e.fingerprint(exA.ID) != fp {
		t.Fatal("TC-15: workspace B changed A's execution")
	}
	if _, err := sqlcGetExecution(e, workspaceB, exA.ID); err == nil {
		t.Fatal("TC-15: B read A's execution")
	}
	if steps := e.steps(workspaceB, exA.ID); len(steps) != 0 {
		t.Fatalf("TC-15: B listed A's steps: %#v", steps)
	}

	t.Run("TC-15/cross-workspace rows are refused by the schema", func(t *testing.T) {
		_, err := e.pool.Exec(e.ctx, `INSERT INTO execution_steps(workspace_id,execution_id,step_key,position,kind) VALUES($1,$2,'evil',9,'notify')`, workspaceB, exA.ID)
		requirePGCode(t, err, "23503")
		_, err = e.pool.Exec(e.ctx, `INSERT INTO execution_events(workspace_id,execution_id,sequence,kind) VALUES($1,$2,99,'transition')`, workspaceB, exA.ID)
		requirePGCode(t, err, "23503")
		// An A execution without an active job, so only the FK can refuse.
		idle := e.start(workspaceA, vA.ID, "iso-idle", chainDef("a"))
		if _, err := e.store.CancelExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: idle.ID, IdempotencyKey: "idle"}); err != nil {
			t.Fatal(err)
		}
		mustExec(t, e.ctx, e.pool, `DELETE FROM jobs WHERE execution_id=$1`, idle.ID)
		_, err = e.pool.Exec(e.ctx, `INSERT INTO jobs(workspace_id,execution_id,step_key,due_at,max_attempts) VALUES($1,$2,'a',now(),1)`, workspaceB, idle.ID)
		requirePGCode(t, err, "23503")
		vB := e.enabledVersion(workspaceB, "iso")
		_, err = e.pool.Exec(e.ctx, `INSERT INTO workflow_executions(workspace_id,behavior_version_id,trigger_key,correlation_id,state,definition,source_kind,source_id,operation_key,parent_execution_id)
			VALUES($1,$2,'evil-child',gen_random_uuid(),'RECEIVED','{}','x','y','z',$3)`, workspaceB, vB.ID, exA.ID)
		requirePGCode(t, err, "23503")
		_, err = e.pool.Exec(e.ctx, `INSERT INTO workflow_executions(workspace_id,behavior_version_id,trigger_key,correlation_id,state,definition,source_kind,source_id,operation_key)
			VALUES($1,$2,'evil-version',gen_random_uuid(),'RECEIVED','{}','x','y','z')`, workspaceB, vA.ID)
		requirePGCode(t, err, "23503")
		_, err = e.pool.Exec(e.ctx, `INSERT INTO execution_events(workspace_id,execution_id,sequence,kind,step_key) VALUES($1,$2,99,'transition','ghost')`, workspaceA, exA.ID)
		requirePGCode(t, err, "23503")
	})
}

func sqlcGetExecution(e *wfEnv, ws string, id int64) (sqlc.WorkflowExecution, error) {
	return e.store.q.GetWorkflowExecution(e.ctx, sqlc.GetWorkflowExecutionParams{WorkspaceID: mustDBID(e.t, ws), ID: id})
}

func jsonLen(cp workflow.Checkpoint) (int, error) {
	b, err := json.Marshal(cp)
	return len(b), err
}

// checkpointOfSize builds a checkpoint whose JSON encoding is exactly n bytes.
func checkpointOfSize(t *testing.T, n int) workflow.Checkpoint {
	t.Helper()
	cp := workflow.Checkpoint{References: []workflow.CheckpointReference{{Kind: "tool_run", ReferenceID: ""}}, Reusable: true}
	// Measure with plain JSON: the empty placeholder id is itself invalid.
	baseLen, err := jsonLen(cp)
	if err != nil {
		t.Fatal(err)
	}
	pad := n - baseLen
	var b strings.Builder
	for i := 0; b.Len() < pad; i++ {
		fmt.Fprintf(&b, "%08x", uint32(i)*2654435761)
	}
	cp.References[0].ReferenceID = b.String()[:pad]
	got, err := jsonLen(cp)
	if err != nil || got != n {
		t.Fatalf("checkpoint size %d, want %d (%v)", got, n, err)
	}
	return cp
}

// TC-16 (REQ-5): only bounded, well-formed checkpoints and non-negative
// usage are written; rejected writes leave no partial event or update.
func TestExecutionCheckpointBounds(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "bounds")
	ex := e.start(workspaceA, v.ID, "bounds-1", chainDef("a", "b"))
	job := e.claim(e.base)
	at := e.base.Add(time.Second)

	near := checkpointOfSize(t, 64*1024-64)
	step, err := e.store.CheckpointJob(e.ctx, checkpointReq(job, at, &near, workflow.Usage{InputTokens: 1}, "cp-near"))
	if err != nil {
		t.Fatalf("TC-16: checkpoint near 64 KiB rejected: %v", err)
	}
	if step.Checkpoint == nil || step.Checkpoint.References[0].ReferenceID != near.References[0].ReferenceID {
		t.Fatal("TC-16: near-limit checkpoint not stored")
	}

	exact := checkpointOfSize(t, 64*1024)
	// At exactly 64 KiB of JSON the jsonb column size may exceed the DB
	// CHECK: either it is stored, or it is ErrInvalidInput with nothing
	// written. A raw DB error or a partial write is a defect.
	exactFP := e.fingerprint(ex.ID)
	if _, err := e.store.CheckpointJob(e.ctx, checkpointReq(job, at, &exact, workflow.Usage{}, "cp-exact")); err != nil {
		if !errors.Is(err, workflow.ErrInvalidInput) {
			t.Errorf("TC-16: exact 64 KiB checkpoint: want stored or ErrInvalidInput, got %v", err)
		}
		if e.fingerprint(ex.ID) != exactFP {
			t.Error("TC-16: rejected exact 64 KiB checkpoint left a partial write")
		}
	}

	rejected := func(name string, req workflow.JobCheckpointRequest, want error) {
		t.Helper()
		fp := e.fingerprint(ex.ID)
		_, err := e.store.CheckpointJob(e.ctx, req)
		if err == nil {
			t.Errorf("TC-16: %s accepted", name)
		} else if want != nil && !errors.Is(err, want) {
			t.Errorf("TC-16: %s: want %v, got %v", name, want, err)
		}
		if e.fingerprint(ex.ID) != fp {
			t.Errorf("TC-16: %s left a partial write", name)
		}
	}
	over := checkpointOfSize(t, 64*1024+1)
	rejected("checkpoint over 64 KiB", checkpointReq(job, at, &over, workflow.Usage{InputTokens: 5}, "cp-over"), workflow.ErrInvalidInput)
	rejected("negative usage", checkpointReq(job, at, reusable("1"), workflow.Usage{InputTokens: 3, OutputTokens: -1}, "cp-neg"), workflow.ErrInvalidInput)
	rejected("negative cache usage", checkpointReq(job, at, nil, workflow.Usage{CacheWriteTokens: -9}, "cp-neg2"), workflow.ErrInvalidInput)
	emptyKind := &workflow.Checkpoint{References: []workflow.CheckpointReference{{Kind: "", ReferenceID: "41"}}}
	rejected("reference without kind", checkpointReq(job, at, emptyKind, workflow.Usage{}, "cp-nokind"), workflow.ErrInvalidInput)
	emptyID := &workflow.Checkpoint{References: []workflow.CheckpointReference{{Kind: "tool_run", ReferenceID: "  "}}}
	rejected("reference without id", checkpointReq(job, at, emptyID, workflow.Usage{}, "cp-noid"), workflow.ErrInvalidInput)
	nul := &workflow.Checkpoint{References: []workflow.CheckpointReference{{Kind: "tool_run", ReferenceID: "a\x00b"}}}
	rejected("reference with NUL", checkpointReq(job, at, nul, workflow.Usage{}, "cp-nul"), nil)

	t.Run("TC-16/usage overflow is rejected atomically", func(t *testing.T) {
		if _, err := e.store.CheckpointJob(e.ctx, checkpointReq(job, at, nil, workflow.Usage{OutputTokens: math.MaxInt64 - 5}, "cp-big")); err != nil {
			t.Fatalf("large usage: %v", err)
		}
		rejected("usage overflow", checkpointReq(job, at, reusable("2"), workflow.Usage{OutputTokens: 10}, "cp-overflow"), nil)
		if s := e.step(workspaceA, ex.ID, "a"); s.Usage.OutputTokens != math.MaxInt64-5 {
			t.Fatalf("usage after overflow = %d", s.Usage.OutputTokens)
		}
	})

	t.Run("TC-16/duplicate checkpoint key does not double-count usage", func(t *testing.T) {
		before := e.step(workspaceA, ex.ID, "a").Usage.InputTokens
		if _, err := e.store.CheckpointJob(e.ctx, checkpointReq(job, at, nil, workflow.Usage{InputTokens: 4}, "cp-dup")); err != nil {
			t.Fatal(err)
		}
		_, err := e.store.CheckpointJob(e.ctx, checkpointReq(job, at, nil, workflow.Usage{InputTokens: 4}, "cp-dup"))
		if got := e.step(workspaceA, ex.ID, "a").Usage.InputTokens; got != before+4 {
			t.Fatalf("duplicate key counted usage twice: %d -> %d (err=%v)", before, got, err)
		}
	})

	t.Run("TC-16/legacy RecordCheckpoint requires the lease fence", func(t *testing.T) {
		base := workflow.CheckpointRequest{JobID: job.ID, WorkspaceID: workspaceA, ClaimToken: job.ClaimToken, At: at,
			ExecutionID: ex.ID, StepKey: "a", ExpectedAttempt: 1, Checkpoint: *reusable("3"), Usage: workflow.Usage{InputTokens: 2}, IdempotencyKey: "rc-1"}
		for name, mutate := range map[string]func(*workflow.CheckpointRequest){
			"no job":   func(r *workflow.CheckpointRequest) { r.JobID = 0 },
			"no token": func(r *workflow.CheckpointRequest) { r.ClaimToken = "" },
			"no time":  func(r *workflow.CheckpointRequest) { r.At = time.Time{} },
		} {
			r := base
			mutate(&r)
			fp := e.fingerprint(ex.ID)
			if _, err := e.store.RecordCheckpoint(e.ctx, r); !errors.Is(err, workflow.ErrInvalidInput) {
				t.Errorf("%s: %v", name, err)
			}
			if e.fingerprint(ex.ID) != fp {
				t.Errorf("%s wrote", name)
			}
		}
		if _, err := e.store.RecordCheckpoint(e.ctx, base); err != nil {
			t.Fatalf("fenced RecordCheckpoint: %v", err)
		}
	})
}

// failedParent runs a two-step execution: step a succeeds with cp, step b
// fails with reason. It returns the FAILED execution.
func (e *wfEnv) failedParent(versionID int64, trigger string, cp *workflow.Checkpoint, reason workflow.ReasonCode) workflow.Execution {
	e.t.Helper()
	ex := e.start(workspaceA, versionID, trigger, chainDef("a", "b"))
	e.complete(e.claim(e.base), e.base, cp, workflow.Usage{InputTokens: 11, OutputTokens: 4})
	e.failJob(e.claim(e.base.Add(time.Second)), e.base.Add(time.Second), reason)
	got := e.execution(workspaceA, ex.ID)
	if got.State != workflow.StateFailed || got.Reason != reason {
		e.t.Fatalf("setup failed parent: %s %s", got.State, got.Reason)
	}
	return got
}

// TC-18 (REQ-11): retrying a FAILED execution, repeatedly and concurrently,
// creates exactly one QUEUED child pinned to the same version.
func TestExecutionRetryCreatesPinnedChild(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "retry")
	parent := e.failedParent(v.ID, "retry-parent", reusable("41"), workflow.ReasonNonRetryable)
	parentFP := e.fingerprint(parent.ID)

	const n = 6
	ids := make([]int64, n)
	errs := make([]error, n)
	parallel(n, func(i int) {
		child, err := e.store.RetryExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: parent.ID, IdempotencyKey: fmt.Sprintf("retry-%d", i)})
		ids[i], errs[i] = child.ID, err
	})
	for i := range ids {
		if errs[i] != nil || ids[i] == 0 || ids[i] != ids[0] {
			t.Fatalf("TC-18: concurrent retries ids=%v errs=%v", ids, errs)
		}
	}
	if c := e.count(`SELECT count(*) FROM workflow_executions WHERE parent_execution_id=$1`, parent.ID); c != 1 {
		t.Fatalf("TC-18: %d children", c)
	}
	if e.fingerprint(parent.ID) != parentFP {
		t.Fatal("TC-18: retry changed the parent")
	}
	child := e.execution(workspaceA, ids[0])
	if child.State != workflow.StateQueued || child.ParentExecutionID != parent.ID || child.VersionID != parent.VersionID ||
		child.TriggerKey == parent.TriggerKey || child.References.OperationKey != parent.References.OperationKey ||
		child.References.SourceID != parent.References.SourceID || child.CorrelationID != parent.CorrelationID ||
		defJSON(t, child.Definition) != defJSON(t, parent.Definition) {
		t.Fatalf("TC-18: child = %#v\nparent = %#v", child, parent)
	}
	a, b := e.step(workspaceA, child.ID, "a"), e.step(workspaceA, child.ID, "b")
	if a.State != workflow.StepSucceeded || a.Checkpoint == nil || len(a.Checkpoint.References) != 1 || a.Checkpoint.References[0].ReferenceID != "41" ||
		a.Checkpoint.References[0].OperationKey != "op-41" {
		t.Fatalf("TC-18: reusable step not copied: %#v", a)
	}
	if a.Usage.InputTokens != 11 {
		t.Fatalf("TC-18: usage of the completed step lost: %#v", a.Usage)
	}
	if b.State != workflow.StepPending || b.Checkpoint != nil || b.Attempts != 0 {
		t.Fatalf("TC-18: unfinished step = %#v", b)
	}
	jobs := e.activeJobs(child.ID)
	if len(jobs) != 1 || jobs[0].StepKey != "b" {
		t.Fatalf("TC-18: child jobs = %s", describeJobs(jobs))
	}
	events := e.events(child.ID)
	if len(events) != 2 || events[0].Kind != "retry_child" || events[1].To != "QUEUED" {
		t.Fatalf("TC-18: child events = %#v", events)
	}
	again, err := e.store.RetryExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: parent.ID, IdempotencyKey: "retry-0"})
	if err != nil || again.ID != child.ID {
		t.Fatalf("TC-18: repeated key: %d %v", again.ID, err)
	}
	if next := e.claim(e.base.Add(2 * time.Second)); next.ExecutionID != child.ID || next.StepKey != "b" {
		t.Fatalf("TC-18: child resumed at %s (execution %d)", next.StepKey, next.ExecutionID)
	}
}

// TC-19 (REQ-11): unsafe retries are refused; an existing child is returned;
// resume and signal are not eligible before P5.
func TestExecutionRetryRejectsUnsafe(t *testing.T) {
	e := newWFEnv(t)
	v := e.enabledVersion(workspaceA, "unsafe")
	children := func(id int64) int {
		return e.count(`SELECT count(*) FROM workflow_executions WHERE parent_execution_id=$1`, id)
	}
	retry := func(id int64) (workflow.Execution, error) {
		return e.store.RetryExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: id, IdempotencyKey: fmt.Sprintf("r-%d", id)})
	}

	t.Run("TC-19/non-failed states", func(t *testing.T) {
		queued := e.start(workspaceA, v.ID, "u-queued", chainDef("a"))
		if _, err := retry(queued.ID); !errors.Is(err, workflow.ErrNotEligible) {
			t.Fatalf("queued: %v", err)
		}
		e.complete(e.claim(e.base), e.base, reusable("1"), workflow.Usage{})
		if _, err := retry(queued.ID); !errors.Is(err, workflow.ErrNotEligible) {
			t.Fatalf("completed: %v", err)
		}
		cancelled := e.start(workspaceA, v.ID, "u-cancelled", chainDef("a"))
		if _, err := e.store.CancelExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: cancelled.ID, IdempotencyKey: "c"}); err != nil {
			t.Fatal(err)
		}
		if _, err := retry(cancelled.ID); !errors.Is(err, workflow.ErrNotEligible) {
			t.Fatalf("cancelled: %v", err)
		}
		if children(queued.ID)+children(cancelled.ID) != 0 {
			t.Fatal("children created for ineligible executions")
		}
	})

	t.Run("TC-19/uncertain side effect", func(t *testing.T) {
		p := e.failedParent(v.ID, "u-uncertain", reusable("1"), workflow.ReasonSideEffectUncertain)
		fp := e.fingerprint(p.ID)
		if _, err := retry(p.ID); !errors.Is(err, workflow.ErrProviderUncertainty) {
			t.Fatalf("uncertain: %v", err)
		}
		if children(p.ID) != 0 || e.fingerprint(p.ID) != fp {
			t.Fatal("uncertain retry wrote")
		}
	})

	t.Run("TC-19/unsafe checkpoints", func(t *testing.T) {
		notReusable := &workflow.Checkpoint{References: []workflow.CheckpointReference{{Kind: "telegram_send", ReferenceID: "m1"}}}
		for name, cp := range map[string]*workflow.Checkpoint{"not reusable": notReusable, "no checkpoint": nil} {
			p := e.failedParent(v.ID, "u-"+strings.ReplaceAll(name, " ", "-"), cp, workflow.ReasonNonRetryable)
			if _, err := retry(p.ID); !errors.Is(err, workflow.ErrUnsafeRetry) {
				t.Fatalf("%s: %v", name, err)
			}
			if children(p.ID) != 0 {
				t.Fatalf("%s: child created", name)
			}
		}
	})

	t.Run("TC-19/original parent after a child exists returns that child", func(t *testing.T) {
		p := e.failedParent(v.ID, "u-lineage", reusable("1"), workflow.ReasonAttemptsExhausted)
		child, err := retry(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		job := e.claim(e.base.Add(2 * time.Second))
		if job.ExecutionID != child.ID {
			t.Fatalf("claimed %d", job.ExecutionID)
		}
		e.failJob(job, e.base.Add(2*time.Second), workflow.ReasonNonRetryable)
		again, err := e.store.RetryExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: p.ID, IdempotencyKey: "second-try"})
		if err != nil || again.ID != child.ID {
			t.Fatalf("original parent retried again: %d (child %d) %v", again.ID, child.ID, err)
		}
		if children(p.ID) != 1 {
			t.Fatal("second direct child")
		}
		grandchild, err := retry(child.ID)
		if err != nil || grandchild.ParentExecutionID != child.ID || grandchild.VersionID != p.VersionID ||
			grandchild.References.OperationKey != p.References.OperationKey {
			t.Fatalf("retry of failed child: %#v %v", grandchild, err)
		}
		// Leave no due job behind for the next subtest's global claim.
		if _, err := e.store.CancelExecution(e.ctx, workflow.Command{WorkspaceID: workspaceA, ExecutionID: grandchild.ID, IdempotencyKey: "cleanup"}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("TC-19/resume and signal are not eligible yet", func(t *testing.T) {
		p := e.failedParent(v.ID, "u-resume", reusable("1"), workflow.ReasonNonRetryable)
		engine := workflow.NewEngine(e.store)
		cmd := workflow.Command{WorkspaceID: workspaceA, ExecutionID: p.ID, IdempotencyKey: "resume-1"}
		if _, err := e.store.ResumeExecution(e.ctx, cmd); !errors.Is(err, workflow.ErrNotEligible) {
			t.Fatalf("resume: %v", err)
		}
		if _, err := engine.Resume(e.ctx, cmd); !errors.Is(err, workflow.ErrNotEligible) {
			t.Fatalf("engine resume: %v", err)
		}
		sig := workflow.Signal{WorkspaceID: workspaceA, ExecutionID: p.ID, SignalID: "s1", BlockedStepKey: "b", ReferenceKind: "approval", ReferenceID: "1", IdempotencyKey: "sig-1"}
		if err := e.store.HandleSignal(e.ctx, sig); !errors.Is(err, workflow.ErrNotEligible) {
			t.Fatalf("signal: %v", err)
		}
		if err := engine.HandleSignal(e.ctx, sig); !errors.Is(err, workflow.ErrNotEligible) {
			t.Fatalf("engine signal: %v", err)
		}
		if _, err := workflow.NewEngine(nil).Retry(e.ctx, cmd); !errors.Is(err, workflow.ErrInvalidInput) {
			t.Fatalf("nil store engine: %v", err)
		}
	})
}

// TC-12 (REQ-4, REQ-5; defect D1): a valid definition whose root step has no
// dependencies, as every normalized schema 1 and schema 2 graph has, starts.
func TestExecutionStartAcceptsRootStepsWithoutDependencies(t *testing.T) {
	e := newWFEnv(t)
	v1 := e.publishEnabled(workspaceA, wfDoc("legacy"))
	v2 := e.publishEnabled(workspaceA, wfDoc2("modern", schema2Workflow()))
	for name, tc := range map[string]struct {
		version int64
		def     workflow.Definition
	}{
		"schema 1 default graph": {v1.ID, e.definitionOf(workspaceA, "legacy", 1)},
		"schema 2 graph":         {v2.ID, e.definitionOf(workspaceA, "modern", 1)},
		"hand-written chain":     {v1.ID, workflow.Definition{Steps: []workflow.StepDefinition{{Key: "a", Kind: workflow.StepKindModel}}, Retry: workflow.RetryPolicy{MaxAttempts: 1}, StepTimeout: time.Minute, LeaseDuration: 2 * time.Minute}},
	} {
		t.Run("TC-12/"+name, func(t *testing.T) {
			if tc.def.Steps[0].DependsOn != nil {
				t.Fatalf("fixture root step has dependencies: %#v", tc.def.Steps[0])
			}
			req := workflow.StartRequest{WorkspaceID: workspaceA, VersionID: tc.version, TriggerKey: "root-" + name,
				CorrelationID: "019a6690-abcd-7000-8000-0000000000c1", Definition: tc.def,
				References: workflow.References{SourceKind: "test", SourceID: "s", OperationKey: "op-" + name}}
			ex, err := e.store.StartExecution(e.ctx, req)
			if err != nil {
				t.Fatalf("D1: valid definition cannot start: %v", err)
			}
			if ex.State != workflow.StateQueued || len(e.steps(workspaceA, ex.ID)) != len(tc.def.Steps) {
				t.Fatalf("start = %#v", ex)
			}
		})
	}
}
