package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/google/uuid"
)

type workflowCLIFixture struct {
	*phase4CLI
	store     *storage.WorkflowStore
	versionID int64
}

func newWorkflowCLIFixture(t *testing.T) *workflowCLIFixture {
	t.Helper()
	c := newPhase4CLI(t)
	c.must("behavior", "import", writeBehaviorFile(t, restoreBehaviorV1), "--workspace-id", c.ws)
	c.must("behavior", "publish", "restore-cli", "--workspace-id", c.ws)
	c.must("behavior", "enable", "restore-cli", "--workspace-id", c.ws)
	store, err := storage.NewWorkflowStore(c.pool)
	if err != nil {
		t.Fatal(err)
	}
	f := &workflowCLIFixture{phase4CLI: c, store: store}
	if err := c.pool.QueryRow(c.ctx, `SELECT v.id FROM behavior_versions v JOIN behaviors b ON b.id=v.behavior_id WHERE b.workspace_id=$1 AND b.name='restore-cli' ORDER BY v.version DESC LIMIT 1`, c.ws).Scan(&f.versionID); err != nil {
		t.Fatal(err)
	}
	return f
}

func cliDefinition(keys ...string) workflow.Definition {
	d := workflow.Definition{
		Retry:         workflow.RetryPolicy{MaxAttempts: 1},
		StepTimeout:   time.Minute,
		LeaseDuration: 2 * time.Minute,
	}
	for i, key := range keys {
		step := workflow.StepDefinition{Key: key, Kind: workflow.StepKindModel, DependsOn: []string{}}
		if i > 0 {
			step.Kind = workflow.StepKindNotify
			step.DependsOn = []string{keys[i-1]}
		}
		d.Steps = append(d.Steps, step)
	}
	return d
}

func (f *workflowCLIFixture) start(t *testing.T, trigger string, refs workflow.References, keys ...string) workflow.Execution {
	t.Helper()
	if refs.SourceKind == "" {
		refs = workflow.References{SourceKind: "clickup.comment", SourceID: "comment-1", Subject: "task-1", OperationKey: "op:" + trigger}
	}
	ex, err := f.store.StartExecution(f.ctx, workflow.StartRequest{
		WorkspaceID: f.ws, VersionID: f.versionID, TriggerKey: trigger, CorrelationID: uuid.NewString(),
		Definition: cliDefinition(keys...), References: refs,
	})
	if err != nil {
		t.Fatalf("start %s: %v", trigger, err)
	}
	return ex
}

func decodeObject(t *testing.T, out string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	return v
}

func objectItems(t *testing.T, out, key string) ([]any, string) {
	t.Helper()
	v := decodeObject(t, out)
	items, ok := v[key].([]any)
	if !ok {
		t.Fatalf("%s output has no %s list: %v", key, key, v)
	}
	next, _ := v["next_cursor"].(string)
	return items, next
}

func (f *workflowCLIFixture) claim(t *testing.T) *workflow.Job {
	t.Helper()
	job, err := f.store.ClaimJob(f.ctx, time.Now().Add(time.Minute))
	if err != nil || job == nil {
		t.Fatalf("claim job=%v err=%v", job, err)
	}
	return job
}

func jobResult(job *workflow.Job, reason workflow.ReasonCode, checkpoint *workflow.Checkpoint) workflow.JobResultRequest {
	return workflow.JobResultRequest{JobCheckpointRequest: workflow.JobCheckpointRequest{
		JobID: job.ID, WorkspaceID: job.WorkspaceID, ExecutionID: job.ExecutionID, StepKey: job.StepKey,
		ExpectedAttempt: job.Attempts, ClaimToken: job.ClaimToken, At: time.Now(), Checkpoint: checkpoint,
	}, Reason: reason}
}

// TC-C14 (REQ-11): every command is workspace scoped, list limits are bounded,
// and an opaque cursor pages more than 200 executions without duplicates.
func TestWorkflowCLIWorkspaceAndPagination(t *testing.T) {
	f := newWorkflowCLIFixture(t)
	for _, args := range [][]string{{"workflow", "list"}, {"workflow", "list", "--workspace-id", "not-a-uuid"}, {"workflow", "show", "1", "--workspace-id", "not-a-uuid"}} {
		if out, err := f.run(args...); err == nil || out != "" || !strings.Contains(err.Error(), "usage: workflow") {
			t.Fatalf("unscoped command %v: out=%q err=%v", args, out, err)
		}
	}
	for i := 0; i < 205; i++ {
		f.start(t, fmt.Sprintf("page-%03d", i), workflow.References{}, "model")
	}
	first, cursor := objectItems(t, f.must("workflow", "list", "--workspace-id", f.ws), "executions")
	if len(first) != 50 || cursor == "" || strings.Contains(cursor, "205") {
		t.Fatalf("default page len=%d cursor=%q", len(first), cursor)
	}
	seen := map[float64]bool{}
	for _, raw := range first {
		seen[raw.(map[string]any)["id"].(float64)] = true
	}
	for cursor != "" {
		page, next := objectItems(t, f.must("workflow", "list", "--workspace-id", f.ws, "--limit", "200", "--cursor", cursor), "executions")
		for _, raw := range page {
			id := raw.(map[string]any)["id"].(float64)
			if seen[id] {
				t.Fatalf("execution %v repeated across pages", id)
			}
			seen[id] = true
		}
		cursor = next
	}
	if len(seen) != 205 {
		t.Fatalf("paged %d executions, want 205", len(seen))
	}
	for _, limit := range []string{"0", "201"} {
		if _, err := f.run("workflow", "list", "--workspace-id", f.ws, "--limit", limit); err == nil || !strings.Contains(err.Error(), "1-200") {
			t.Fatalf("limit %s accepted: %v", limit, err)
		}
	}
	other := uuid.NewString()
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO workspaces(id,display_name) VALUES($1,'Other')`, other); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run("workflow", "show", "1", "--workspace-id", other); err == nil || !strings.Contains(err.Error(), "not_found") || strings.Contains(err.Error(), f.ws) {
		t.Fatalf("cross-workspace show error=%v", err)
	}
}

// TC-C15 (REQ-11): the operational timeline and dead-letter views expose
// stable pages of safe state, counter, reason and identifier metadata.
func TestWorkflowCLITimelineAndDeadLetter(t *testing.T) {
	f := newWorkflowCLIFixture(t)
	ex := f.start(t, "dead-letter", workflow.References{}, "model")
	job := f.claim(t)
	if err := f.store.FailJob(f.ctx, jobResult(job, workflow.ReasonAttemptsExhausted, nil)); err != nil {
		t.Fatal(err)
	}
	show := f.must("workflow", "show", fmt.Sprint(ex.ID), "--workspace-id", f.ws)
	behaviorDigest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("restore-cli")))
	for _, want := range []string{`"state":"FAILED"`, `"reason":"attempts_exhausted"`, `"behavior_digest":"` + behaviorDigest + `"`} {
		if !strings.Contains(show, want) {
			t.Fatalf("show missing %s: %s", want, show)
		}
	}
	if strings.Contains(show, "restore-cli") {
		t.Fatalf("show leaked raw behavior name: %s", show)
	}
	events, cursor := objectItems(t, f.must("workflow", "history", fmt.Sprint(ex.ID), "--workspace-id", f.ws, "--limit", "1"), "events")
	if len(events) != 1 || cursor == "" {
		t.Fatalf("history first page len=%d cursor=%q", len(events), cursor)
	}
	next, _ := objectItems(t, f.must("workflow", "history", fmt.Sprint(ex.ID), "--workspace-id", f.ws, "--limit", "1", "--cursor", cursor), "events")
	if len(next) != 1 || next[0].(map[string]any)["sequence"].(float64) <= events[0].(map[string]any)["sequence"].(float64) {
		t.Fatalf("history is not stable and ordered: %v then %v", events, next)
	}
	dead, _ := objectItems(t, f.must("workflow", "dead-letter", "--workspace-id", f.ws, "--execution-id", fmt.Sprint(ex.ID)), "dead_letters")
	if len(dead) != 1 || dead[0].(map[string]any)["dead_letter_reason"] != "attempts_exhausted" {
		t.Fatalf("dead letters=%v", dead)
	}
}

// R1: every registered reason keeps its stable machine code across the
// operator views, while an unregistered stored value is reduced to unknown
// and cannot expose caller-controlled text.
func TestWorkflowCLIReasonCodesAndUnknownFallback(t *testing.T) {
	f := newWorkflowCLIFixture(t)

	assertFailedReason := func(name string, reason workflow.ReasonCode, want, forbidden string) {
		t.Helper()
		ex := f.start(t, name, workflow.References{}, "model")
		job := f.claim(t)
		if job.ExecutionID != ex.ID {
			t.Fatalf("%s claimed execution=%d want=%d", name, job.ExecutionID, ex.ID)
		}
		if err := f.store.FailJob(f.ctx, jobResult(job, reason, nil)); err != nil {
			t.Fatal(err)
		}
		outputs := map[string]string{
			"show":        f.must("workflow", "show", fmt.Sprint(ex.ID), "--workspace-id", f.ws),
			"dead-letter": f.must("workflow", "dead-letter", "--workspace-id", f.ws, "--execution-id", fmt.Sprint(ex.ID)),
			"history":     f.must("workflow", "history", fmt.Sprint(ex.ID), "--workspace-id", f.ws),
		}
		for view, out := range outputs {
			if !strings.Contains(out, `"`+want+`"`) {
				t.Fatalf("%s %s missing reason %q: %s", name, view, want, out)
			}
			if forbidden != "" && strings.Contains(out, forbidden) {
				t.Fatalf("%s %s leaked caller text %q: %s", name, view, forbidden, out)
			}
		}
	}

	assertFailedReason("behavior-reason", workflow.ReasonCode(behavior.CodeRunMissing), behavior.CodeRunMissing, "")
	assertFailedReason("agent-reason", workflow.ReasonCode(agent.CodeInvalidDecision), agent.CodeInvalidDecision, "")
	const callerText = "caller-private-reason-text"
	assertFailedReason("unknown-reason", workflow.ReasonCode(callerText), "unknown", callerText)

	signalled := f.start(t, "signal-reasons", workflow.References{}, "model")
	transitions := []workflow.TransitionRequest{
		{WorkspaceID: f.ws, ExecutionID: signalled.ID, From: workflow.StateQueued, To: workflow.StateRunning, Reason: storage.ReasonSignalReceived, IdempotencyKey: "signal-received"},
		{WorkspaceID: f.ws, ExecutionID: signalled.ID, From: workflow.StateRunning, To: workflow.StateQueued, Reason: storage.ReasonResumed, IdempotencyKey: "signal-resumed"},
	}
	for _, req := range transitions {
		if _, err := f.store.TransitionExecution(f.ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	signalShow := f.must("workflow", "show", fmt.Sprint(signalled.ID), "--workspace-id", f.ws)
	signalHistory := f.must("workflow", "history", fmt.Sprint(signalled.ID), "--workspace-id", f.ws)
	if !strings.Contains(signalShow, `"reason":"resumed"`) || !strings.Contains(signalHistory, `"reason":"signal_received"`) || !strings.Contains(signalHistory, `"reason":"resumed"`) {
		t.Fatalf("signal reasons missing: show=%s history=%s", signalShow, signalHistory)
	}

	for _, tc := range []struct {
		name   string
		state  workflow.State
		reason workflow.ReasonCode
	}{
		{"completed", workflow.StateCompleted, workflow.ReasonCompleted},
		{"failed", workflow.StateFailed, workflow.ReasonFailed},
		{"cancelled", workflow.StateCancelled, workflow.ReasonCancelled},
	} {
		ex := f.start(t, "terminal-"+tc.name, workflow.References{}, "model")
		from := workflow.StateQueued
		if tc.state == workflow.StateCompleted {
			if _, err := f.store.TransitionExecution(f.ctx, workflow.TransitionRequest{WorkspaceID: f.ws, ExecutionID: ex.ID, From: from, To: workflow.StateRunning, Reason: workflow.ReasonAccepted, IdempotencyKey: "start-" + tc.name}); err != nil {
				t.Fatal(err)
			}
			from = workflow.StateRunning
		}
		if _, err := f.store.TransitionExecution(f.ctx, workflow.TransitionRequest{WorkspaceID: f.ws, ExecutionID: ex.ID, From: from, To: tc.state, Reason: tc.reason, IdempotencyKey: "finish-" + tc.name}); err != nil {
			t.Fatal(err)
		}
		show := f.must("workflow", "show", fmt.Sprint(ex.ID), "--workspace-id", f.ws)
		history := f.must("workflow", "history", fmt.Sprint(ex.ID), "--workspace-id", f.ws)
		want := `"reason":"` + tc.name + `"`
		if !strings.Contains(show, want) || !strings.Contains(history, want) {
			t.Fatalf("terminal %s missing %s: show=%s history=%s", tc.name, want, show, history)
		}
	}
}

// TC-C16 (REQ-12): repeated operator commands have one durable effect and
// distinguish an idempotent repeat from a command that is not eligible.
func TestWorkflowCLIIdempotentCommands(t *testing.T) {
	f := newWorkflowCLIFixture(t)
	ex := f.start(t, "cancel-once", workflow.References{}, "model")
	if out := f.must("workflow", "cancel", fmt.Sprint(ex.ID), "--workspace-id", f.ws); !strings.Contains(out, `"outcome":"cancelled"`) {
		t.Fatal(out)
	}
	if _, err := f.run("workflow", "cancel", fmt.Sprint(ex.ID), "--workspace-id", f.ws); err == nil || !strings.Contains(err.Error(), "already_cancelled") {
		t.Fatalf("repeat cancel=%v", err)
	}
	var cancelledEvents int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM execution_events WHERE execution_id=$1 AND to_state='CANCELLED'`, ex.ID).Scan(&cancelledEvents); err != nil || cancelledEvents != 1 {
		t.Fatalf("cancel events=%d err=%v", cancelledEvents, err)
	}
	if _, err := f.run("workflow", "resume", fmt.Sprint(ex.ID), "--workspace-id", f.ws); err == nil || !strings.Contains(err.Error(), "not_resumable") {
		t.Fatalf("cancelled resume=%v", err)
	}
}

// TC-C17 (REQ-12): resume/retry reject unsafe states; a safe retry creates
// exactly one pinned child and repeating it returns that child.
func TestWorkflowCLIResumeRetrySafety(t *testing.T) {
	f := newWorkflowCLIFixture(t)
	running := f.start(t, "running", workflow.References{}, "model")
	if _, err := f.run("workflow", "retry", fmt.Sprint(running.ID), "--workspace-id", f.ws); err == nil || !strings.Contains(err.Error(), "not_retryable") {
		t.Fatalf("running retry=%v", err)
	}
	f.must("workflow", "cancel", fmt.Sprint(running.ID), "--workspace-id", f.ws)
	failed := f.start(t, "failed", workflow.References{}, "model")
	job := f.claim(t)
	if job.ExecutionID != failed.ID {
		t.Fatalf("claimed execution %d, want %d", job.ExecutionID, failed.ID)
	}
	if err := f.store.FailJob(f.ctx, jobResult(job, workflow.ReasonAttemptsExhausted, nil)); err != nil {
		t.Fatal(err)
	}
	first := decodeObject(t, f.must("workflow", "retry", fmt.Sprint(failed.ID), "--workspace-id", f.ws))
	second := decodeObject(t, f.must("workflow", "retry", fmt.Sprint(failed.ID), "--workspace-id", f.ws))
	if first["retry_execution_id"] != second["retry_execution_id"] || second["outcome"] != "existing" {
		t.Fatalf("retry outputs first=%v second=%v", first, second)
	}
	var children int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM workflow_executions WHERE parent_execution_id=$1`, failed.ID).Scan(&children); err != nil || children != 1 {
		t.Fatalf("retry children=%d err=%v", children, err)
	}
}

// TC-C18 (REQ-14): cutover is read-only by default and applying it twice is
// idempotent, reporting the already-cut-over state without duplicating data.
func TestWorkflowCLICutoverDefaultsReadOnly(t *testing.T) {
	f := newWorkflowCLIFixture(t)
	before := decodeObject(t, f.must("workflow", "cutover", "--workspace-id", f.ws))
	if before["preflight"] != true || before["applied"] != false {
		t.Fatalf("preflight=%v", before)
	}
	mode, err := f.store.RuntimeMode(f.ctx, f.ws)
	if err != nil || mode != storage.RuntimeModeLegacy {
		t.Fatalf("preflight changed mode=%q err=%v", mode, err)
	}
	first := decodeObject(t, f.must("workflow", "cutover", "--workspace-id", f.ws, "--apply"))
	second := decodeObject(t, f.must("workflow", "cutover", "--workspace-id", f.ws, "--apply"))
	if first["applied"] != true || second["already_cut_over"] != true {
		t.Fatalf("apply outputs first=%v second=%v", first, second)
	}
}

// TC-C19 (REQ-15): operator reads and errors must never print planted prompt,
// transcript, tool payload or secret text, including hostile reference values.
func TestWorkflowCLINoPayloadLeak(t *testing.T) {
	f := newWorkflowCLIFixture(t)
	secret := "ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(secret)))
	ex := f.start(t, "payload-leak", workflow.References{
		SourceKind: "clickup.comment", SourceID: secret, Subject: secret, Revision: secret, OperationKey: secret,
	}, "model")
	var correlations []string
	for _, args := range [][]string{
		{"workflow", "show", fmt.Sprint(ex.ID), "--workspace-id", f.ws},
		{"workflow", "show", fmt.Sprint(ex.ID), "--workspace-id", f.ws},
		{"workflow", "list", "--workspace-id", f.ws},
		{"workflow", "steps", fmt.Sprint(ex.ID), "--workspace-id", f.ws},
		{"workflow", "history", fmt.Sprint(ex.ID), "--workspace-id", f.ws},
	} {
		out, err := f.run(args...)
		combined := out
		if err != nil {
			combined += err.Error()
		}
		if strings.Contains(combined, secret) || strings.Contains(combined, "ghp_") {
			t.Fatalf("%v leaked planted payload: %q", args, combined)
		}
		if strings.Contains(combined, digest) {
			correlations = append(correlations, digest)
		}
	}
	if len(correlations) < 2 {
		t.Fatalf("stable reference correlation missing: digest %q appeared in %d command outputs", digest, len(correlations))
	}
}

// TC-C20 (REQ-16): migrated executions retain inspect/trace identifiers while
// reads neither mutate the execution nor expose legacy payload content.
func TestLegacyInspectTraceLinks(t *testing.T) {
	f := newWorkflowCLIFixture(t)
	behaviors, err := storage.NewBehaviorStore(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	version, err := behaviors.GetVersion(f.ctx, f.ws, "restore-cli", 0)
	if err != nil {
		t.Fatal(err)
	}
	trigger, err := json.Marshal(behavior.TriggerData{Type: behavior.TriggerCommentAdded, EventKey: "legacy-event-71", TaskID: "task-9", CommentID: "comment-71"})
	if err != nil {
		t.Fatal(err)
	}
	run, created, err := behaviors.CreateRun(f.ctx, f.ws, behavior.NewRun{
		BehaviorID: version.BehaviorID, VersionID: version.ID, Version: version.Version,
		TriggerKind: behavior.TriggerCommentAdded, Trigger: trigger, Subject: behavior.TaskSubject("task-9"), Revision: "legacy-event-71", RunAt: time.Now().Add(-time.Minute),
	})
	if err != nil || !created {
		t.Fatalf("legacy run created=%v err=%v", created, err)
	}
	cutover := decodeObject(t, f.must("workflow", "cutover", "--workspace-id", f.ws, "--apply"))
	behaviorRuns, ok := cutover["behavior_runs"].(map[string]any)
	if !ok || cutover["imported_executions"] != float64(1) || behaviorRuns["pending"] != float64(1) {
		t.Fatalf("cutover counters=%v", cutover)
	}
	var parentID int64
	if err := f.pool.QueryRow(f.ctx, `SELECT id FROM workflow_executions WHERE legacy_run_id=$1`, run.ID).Scan(&parentID); err != nil {
		t.Fatal(err)
	}
	job := f.claim(t)
	if job.ExecutionID != parentID {
		t.Fatalf("claimed execution=%d want migrated parent=%d", job.ExecutionID, parentID)
	}
	if err := f.store.FailJob(f.ctx, jobResult(job, workflow.ReasonAttemptsExhausted, nil)); err != nil {
		t.Fatal(err)
	}
	retry := decodeObject(t, f.must("workflow", "retry", fmt.Sprint(parentID), "--workspace-id", f.ws))
	childID := int64(retry["retry_execution_id"].(float64))

	trace := f.must("trace", "--workspace-id", f.ws, fmt.Sprint(run.ID), "--json")
	for _, want := range []string{fmt.Sprintf(`"id": %d`, run.ID), fmt.Sprintf(`"execution_id": %d`, parentID), fmt.Sprintf(`"execution_id": %d`, childID), fmt.Sprintf(`"parent_execution_id": %d`, parentID)} {
		if !strings.Contains(trace, want) {
			t.Fatalf("trace missing %s: %s", want, trace)
		}
	}
	show := f.must("workflow", "show", fmt.Sprint(parentID), "--workspace-id", f.ws)
	if !strings.Contains(show, fmt.Sprintf(`"legacy_run_id":%d`, run.ID)) {
		t.Fatalf("workflow show lacks legacy run link: %s", show)
	}
	inspect := f.must("inspect")
	for _, want := range []string{fmt.Sprintf(`"execution_id":%d`, parentID), fmt.Sprintf(`"execution_id":%d`, childID), fmt.Sprintf(`"legacy_run_id":%d`, run.ID)} {
		if !strings.Contains(inspect, want) {
			t.Fatalf("inspect missing %s", want)
		}
	}
}
