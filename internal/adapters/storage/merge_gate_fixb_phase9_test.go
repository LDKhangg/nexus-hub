package storage

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/policy"
)

// Phase 9 Fix B regressions (review B R-B-1..R-B-5, D-B-1, D-B-2; commits
// 84595c6, 9bb65d5) with the real stores and fakes for GitHub, Telegram and
// the step waker. CI only; DB tests use the loopback test DB.

const fbClosedPrefix = "⌛ Hết hạn: merge PR acme/api#12"

func (m *mgEnv) endExec(t *testing.T, exec int64, how string) {
	t.Helper()
	switch how {
	case "COMPLETED", "FAILED":
		mustExec(t, m.ctx, m.pool, `UPDATE workflow_executions SET state=$2, finished_at=now() WHERE id=$1`, exec, how)
	case "CANCELLED":
		mustExec(t, m.ctx, m.pool, `UPDATE workflow_executions SET state=$2, cancelled_at=now(), finished_at=now() WHERE id=$1`, exec, how)
	case "DEADLINE":
		mustExec(t, m.ctx, m.pool, `UPDATE workflow_executions SET deadline_at=now()-interval '1 minute' WHERE id=$1`, exec)
	default:
		t.Fatalf("unknown end %q", how)
	}
}

// askFor asks for the merge of PR number in an execution and returns the open gate.
func (m *mgEnv) askFor(t *testing.T, exec, number int64) behavior.PolicyGateRecord {
	t.Helper()
	out, err := m.actions.RunTask(m.ctx, m.run(exec, "acme/api", number, mgHead))
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := m.waitingGate(t, out)
	return rec
}

// editsOf returns the edits made to one message.
func (m *mgEnv) editsOf(id int64) []mgEdit {
	m.msgr.mu.Lock()
	defer m.msgr.mu.Unlock()
	var out []mgEdit
	for _, e := range m.msgr.edits {
		if e.ID == id {
			out = append(out, e)
		}
	}
	return out
}

func (m *mgEnv) dbTime(t *testing.T, sql string, args ...any) time.Time {
	t.Helper()
	var at time.Time
	if err := m.pool.QueryRow(m.ctx, sql, args...).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// TC-FB-1 (R-B-1): a gate's expiry is capped at its execution's deadline; a deadline already passed blocks with approval_expired and asks nobody.
func TestP9FixB_GateExpiryCappedAtDeadline(t *testing.T) {
	t.Run("deadline before the TTL", func(t *testing.T) {
		m := newMGEnv(t, mgOpts{})
		mustExec(t, m.ctx, m.pool, `UPDATE workflow_executions SET deadline_at=now()+interval '2 hours' WHERE id=$1`, m.exec)
		rec, _ := m.ask(t)
		dl := m.dbTime(t, `SELECT deadline_at FROM workflow_executions WHERE id=$1`, m.exec)
		stored := m.dbTime(t, `SELECT expires_at FROM approvals WHERE id=$1`, rec.ID)
		for name, at := range map[string]time.Time{"returned": rec.ExpiresAt, "stored": stored} {
			if d := dl.Sub(at); d > time.Millisecond || d < -time.Millisecond {
				t.Errorf("%s expiry %v is not the deadline %v", name, at, dl)
			}
		}
	})
	t.Run("deadline after the TTL keeps the TTL", func(t *testing.T) {
		m := newMGEnv(t, mgOpts{})
		mustExec(t, m.ctx, m.pool, `UPDATE workflow_executions SET deadline_at=now()+interval '72 hours' WHERE id=$1`, m.exec)
		rec, _ := m.ask(t)
		if d := time.Until(rec.ExpiresAt); d < 23*time.Hour || d > 25*time.Hour {
			t.Fatalf("expiry %v is not about 24 h away", rec.ExpiresAt)
		}
	})
	t.Run("deadline already passed", func(t *testing.T) {
		m := newMGEnv(t, mgOpts{})
		m.endExec(t, m.exec, "DEADLINE")
		out, err := m.actions.RunTask(m.ctx, m.task())
		if err != nil {
			t.Fatal(err)
		}
		requireBlocked(t, out, behavior.ReasonApprovalExpired)
		if sent, _ := m.msgr.counts(); sent != 0 {
			t.Fatalf("the owner was asked for a gate that is already expired (%d messages)", sent)
		}
		m.requireNoMerge(t)
	})
}

// TC-FB-2 (R-B-1): a press after the execution completed, failed, was cancelled or passed its deadline is stale: the message is closed with ⌛, nothing is approved and the step is not woken.
func TestP9FixB_PressAfterExecutionEnded(t *testing.T) {
	for _, how := range []string{"COMPLETED", "FAILED", "CANCELLED", "DEADLINE"} {
		t.Run(how, func(t *testing.T) {
			m := newMGEnv(t, mgOpts{})
			rec, _ := m.ask(t)
			m.endExec(t, m.exec, how)
			toast, code, keep := m.press(rec, behavior.CallbackApprove)
			if toast != "Đã xử lý hoặc đã hết hạn." || code != "stale" || keep {
				t.Fatalf("press: %q %q keep=%v", toast, code, keep)
			}
			if e := m.msgr.lastEdit(); e.ID != rec.MessageID || !strings.HasPrefix(e.Text, fbClosedPrefix) {
				t.Fatalf("edit %+v", e)
			}
			if got := m.reload(t, rec.ID); got.Decision == behavior.DecisionApproved || got.Decision == behavior.DecisionRejected || got.Consumed {
				t.Fatalf("an ended execution's gate was decided: %+v", got)
			}
			if n := m.count(t, `SELECT count(*) FROM approvals WHERE id=$1 AND decided_user_id IS NOT NULL`, rec.ID); n != 0 {
				t.Fatal("a decider was recorded")
			}
			if len(m.waker.calls) != 0 {
				t.Fatalf("the step was woken: %v", m.waker.calls)
			}
			// Rejecting is refused the same way.
			if toast, code, _ := m.press(rec, behavior.CallbackReject); code != "stale" || toast == "Đã từ chối." {
				t.Fatalf("reject press: %q %q", toast, code)
			}
			m.requireNoMerge(t)
		})
	}
}

// TC-FB-3 (R-B-1): after the old execution ended, a new /plan for the same PR gets its own gate and the old message shows ⌛.
func TestP9FixB_NewExecutionSamePRGetsOwnGate(t *testing.T) {
	m := newMGEnv(t, mgOpts{})
	old, _ := m.ask(t)
	m.endExec(t, m.exec, "COMPLETED")
	exec2 := m.startTask(t)
	out, err := m.actions.RunTask(m.ctx, m.run(exec2, "acme/api", 12, mgHead))
	if err != nil {
		t.Fatal(err)
	}
	fresh, _ := m.waitingGate(t, out)
	if fresh.ID == old.ID || fresh.ExecutionID != exec2 {
		t.Fatalf("new gate %+v old %d", fresh, old.ID)
	}
	if sent, _ := m.msgr.counts(); sent != 2 {
		t.Fatalf("%d messages sent, want one per gate", sent)
	}
	if got := m.reload(t, old.ID); got.Decision != behavior.DecisionExpired {
		t.Fatalf("old gate %+v", got)
	}
	es := m.editsOf(old.MessageID)
	if len(es) != 1 || !strings.HasPrefix(es[0].Text, fbClosedPrefix) {
		t.Fatalf("old message edits %+v", es)
	}
	if len(m.editsOf(fresh.MessageID)) != 0 {
		t.Fatal("the new message was edited")
	}
	// The old press is stale; the new gate can be approved.
	if toast, code, _ := m.press(old, behavior.CallbackApprove); code != "stale" || toast == "Đã duyệt." {
		t.Fatalf("press on the old gate: %q %q", toast, code)
	}
	if toast, code, _ := m.press(fresh, behavior.CallbackApprove); toast != "Đã duyệt." || code != "" {
		t.Fatalf("press on the new gate: %q %q", toast, code)
	}
}

// TC-FB-4 (R-B-1): PolicyGates.Sweep expires and closes the gates of ended executions only; live ones are untouched; a second sweep finds nothing.
func TestP9FixB_SweepEndedExecutionsOnly(t *testing.T) {
	m := newMGEnv(t, mgOpts{})
	execDone, execLate := m.exec, m.startTask(t)
	execLive := m.startTask(t)
	gDone := m.askFor(t, execDone, 12)
	gLate := m.askFor(t, execLate, 13)
	gLive := m.askFor(t, execLive, 14)
	if n, err := m.gates.Sweep(m.ctx); err != nil || n != 0 {
		t.Fatalf("sweep with nothing ended: %d %v", n, err)
	}
	if _, edits := m.msgr.counts(); edits != 0 {
		t.Fatal("the sweep edited a live gate's message")
	}
	m.endExec(t, execDone, "COMPLETED")
	m.endExec(t, execLate, "DEADLINE")
	n, err := m.gates.Sweep(m.ctx)
	if err != nil || n != 2 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	for name, g := range map[string]behavior.PolicyGateRecord{"completed": gDone, "past deadline": gLate} {
		if got := m.reload(t, g.ID); got.Decision != behavior.DecisionExpired {
			t.Errorf("%s gate %+v", name, got)
		}
		es := m.editsOf(g.MessageID)
		if len(es) != 1 || !strings.HasPrefix(es[0].Text, "⌛ Hết hạn") {
			t.Errorf("%s message edits %+v", name, es)
		}
	}
	if got := m.reload(t, gLive.ID); got.Decision != "" {
		t.Fatalf("the live gate was touched: %+v", got)
	}
	if len(m.editsOf(gLive.MessageID)) != 0 {
		t.Fatal("the live gate's message was edited")
	}
	if n, err := m.gates.Sweep(m.ctx); err != nil || n != 0 {
		t.Fatalf("second sweep: %d %v", n, err)
	}
	if _, edits := m.msgr.counts(); edits != 2 {
		t.Fatalf("%d edits after two sweeps", edits)
	}
	// The live gate still works.
	if toast, _, _ := m.press(gLive, behavior.CallbackApprove); toast != "Đã duyệt." {
		t.Fatalf("live press %q", toast)
	}
	// A decided gate of an ended execution is not swept (its decision stays).
	m.endExec(t, execLive, "COMPLETED")
	if n, err := m.gates.Sweep(m.ctx); err != nil || n != 0 {
		t.Fatalf("sweep of a decided gate: %d %v", n, err)
	}
	if m.reload(t, gLive.ID).Decision != behavior.DecisionApproved {
		t.Fatal("a decided gate was overwritten")
	}
}

// TC-FB-5 (R-B-1): the TTL sweep (ExpireDue) leaves due gates of ended executions to the gate sweep and still expires those of live ones.
func TestP9FixB_ExpireDueLeavesEndedGatesToSweep(t *testing.T) {
	m := newMGEnv(t, mgOpts{})
	execEnded, execLive := m.exec, m.startTask(t)
	mustExec(t, m.ctx, m.pool, `UPDATE workflow_executions SET deadline_at=now()+interval '72 hours' WHERE id=$1`, execLive)
	gEnded := m.askFor(t, execEnded, 12)
	gLive := m.askFor(t, execLive, 13)
	m.endExec(t, execEnded, "COMPLETED")
	due, err := m.behaviors.ExpireDue(m.ctx, time.Now().Add(48*time.Hour), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != gLive.ID {
		t.Fatalf("ExpireDue returned %+v, want only the live execution's gate %d", due, gLive.ID)
	}
	if got := m.reload(t, gEnded.ID); got.Decision != "" {
		t.Fatalf("the TTL sweep took the ended gate: %+v", got)
	}
	if n, err := m.gates.Sweep(m.ctx); err != nil || n != 1 {
		t.Fatalf("gate sweep: %d %v", n, err)
	}
	if es := m.editsOf(gEnded.MessageID); len(es) != 1 || !strings.HasPrefix(es[0].Text, fbClosedPrefix) {
		t.Fatalf("ended gate message edits %+v", es)
	}
}

// TC-FB-6 (R-B-5): an open gate past its expiry is replaced by a new one for the same merge (the old message closed with ⌛), not reused.
func TestP9FixB_ExpiredOpenGateIsReplaced(t *testing.T) {
	m := newMGEnv(t, mgOpts{})
	mustExec(t, m.ctx, m.pool, `UPDATE workflow_executions SET deadline_at=now()+interval '72 hours' WHERE id=$1`, m.exec)
	old, _ := m.ask(t)
	later := time.Now().Add(48 * time.Hour)
	m.merge.Now = func() time.Time { return later }
	out, err := m.actions.RunTask(m.ctx, m.task())
	if err != nil {
		t.Fatal(err)
	}
	fresh, _ := m.waitingGate(t, out)
	if fresh.ID == old.ID {
		t.Fatal("the expired gate was reused")
	}
	if got := m.reload(t, old.ID); got.Decision != behavior.DecisionExpired {
		t.Fatalf("old gate %+v", got)
	}
	if es := m.editsOf(old.MessageID); len(es) != 1 || !strings.HasPrefix(es[0].Text, fbClosedPrefix) {
		t.Fatalf("old message edits %+v", es)
	}
	if sent, _ := m.msgr.counts(); sent != 2 {
		t.Fatalf("%d messages sent", sent)
	}
	if m.gateRows(t) != 2 {
		t.Fatalf("%d gate rows", m.gateRows(t))
	}
	// The new gate can still be approved (the press compares with the real clock).
	m.merge.Now = nil
	if toast, _, _ := m.press(fresh, behavior.CallbackApprove); toast != "Đã duyệt." {
		t.Fatalf("press %q", toast)
	}
}

// TC-FB-7 (R-B-3): under allow, a merge target that no earlier task evidence names (repo#number and the full head SHA in one entry) is blocked with merge_target_unproven: no merge, no gate, no message.
func TestP9FixB_AllowWithoutProofIsUnproven(t *testing.T) {
	const allow = "schema: policy/v1\ndefaults:\n  read: allow\n  write: allow\n  dangerous: approval_required\nactions:\n  github_pr_merge: allow\napproval_ttl_hours: 24\n"
	other := "fedcba9876543210fedcba9876543210fedcba98"
	cases := map[string][]string{
		"no proofs":                nil,
		"other PR":                 {"acme/api#13 " + mgHead},
		"number prefix only":       {"acme/api#120 " + mgHead},
		"other repo":               {"acme/web#12 " + mgHead},
		"short sha":                {"acme/api#12 " + mgHead[:7]},
		"other sha":                {"acme/api#12 " + other},
		"sha with extra hex":       {"acme/api#12 " + mgHead + "ab"},
		"pr and sha in two proofs": {"acme/api#12", "head " + mgHead},
		"sha only":                 {mgHead},
	}
	for name, proofs := range cases {
		t.Run(name, func(t *testing.T) {
			m := newMGEnv(t, mgOpts{})
			if _, _, err := m.store.ApplyRuleSet(m.ctx, workspaceA, p9Parse(t, allow), policy.CreatedByCLI, 0); err != nil {
				t.Fatal(err)
			}
			task := m.task()
			task.Proofs = proofs
			out, err := m.actions.RunTask(m.ctx, task)
			if err != nil {
				t.Fatal(err)
			}
			requireBlocked(t, out, behavior.ReasonMergeTargetUnproven)
			m.requireNoMerge(t)
			if m.gateRows(t) != 0 {
				t.Fatal("an allow without proof created a gate")
			}
			if sent, _ := m.msgr.counts(); sent != 0 {
				t.Fatal("an allow without proof sent a message")
			}
		})
	}
	good := map[string][]string{
		"repo#number":     {"github:pr:acme/api#12\nhead " + mgHead},
		"pull url":        {"https://github.com/acme/api/pull/12 head " + mgHead},
		"among others":    {"unrelated", "branch nexus/fix\npr acme/api#12 at " + mgHead + "."},
		"uppercase sha":   {"acme/api#12 " + strings.ToUpper(mgHead)},
		"after a failure": {"acme/api#13 " + other, "acme/api#12 " + mgHead},
	}
	for name, proofs := range good {
		t.Run("proven "+name, func(t *testing.T) {
			m := newMGEnv(t, mgOpts{})
			if _, _, err := m.store.ApplyRuleSet(m.ctx, workspaceA, p9Parse(t, allow), policy.CreatedByCLI, 0); err != nil {
				t.Fatal(err)
			}
			task := m.task()
			task.Proofs = proofs
			out, err := m.actions.RunTask(m.ctx, task)
			if err != nil || !out.Succeeded {
				t.Fatalf("%+v %v", out, err)
			}
			if _, merges := m.gh.state(); merges != 1 || m.gateRows(t) != 0 {
				t.Fatalf("merges=%d gates=%d", merges, m.gateRows(t))
			}
			if m.gh.bodies[0]["sha"] != mgHead {
				t.Fatalf("merged with %v", m.gh.bodies[0])
			}
		})
	}
}

// TC-FB-8 (R-B-3): under approval_required the owner's approval binds the shown target, so no proofs are needed to merge after Duyệt.
func TestP9FixB_ApprovalBindsTargetWithoutProofs(t *testing.T) {
	m := newMGEnv(t, mgOpts{})
	_, ref := m.approved(t)
	task := m.task()
	task.Proofs = nil
	out, err := m.actions.ResumeTask(m.ctx, task, ref)
	if err != nil || !out.Succeeded {
		t.Fatalf("%+v %v", out, err)
	}
	if _, merges := m.gh.state(); merges != 1 {
		t.Fatalf("merges %d", merges)
	}
}

var fbFullSHA = regexp.MustCompile(`^github:merge:acme/api#12:[0-9a-f]{40}$`)

// TC-FB-9 (D-B-2): the replayed evidence of a merge equals the live one and keeps the full 40-hex merge SHA, after a restart too.
func TestP9FixB_ReplayKeepsFullMergeSHA(t *testing.T) {
	m := newMGEnv(t, mgOpts{})
	_, ref := m.approved(t)
	live, err := m.actions.ResumeTask(m.ctx, m.task(), ref)
	if err != nil || !live.Succeeded || len(live.Evidence) != 1 {
		t.Fatalf("%+v %v", live, err)
	}
	if !fbFullSHA.MatchString(live.Evidence[0].ID) || !strings.HasSuffix(live.Evidence[0].ID, mgMergeSHA) {
		t.Fatalf("live evidence %q", live.Evidence[0].ID)
	}
	m.build(mgOpts{})
	for i, wait := range []string{ref, ""} {
		again, err := m.actions.ResumeTask(m.ctx, m.task(), wait)
		if err != nil || !again.Succeeded || len(again.Evidence) != 1 {
			t.Fatalf("replay %d: %+v %v", i, again, err)
		}
		if again.Evidence[0].ID != live.Evidence[0].ID || again.Evidence[0].Kind != live.Evidence[0].Kind {
			t.Fatalf("replay %d evidence %+v differs from live %+v", i, again.Evidence[0], live.Evidence[0])
		}
	}
	if _, merges := m.gh.state(); merges != 1 {
		t.Fatalf("merges %d", merges)
	}
}

// TC-FB-10 (Review B gap): the wake query wakes the step waiting on task:gate:<id> once the gate is decided, and not while it is open or after the execution ended.
// The step row and the execution state are seeded here as the orchestrator step leaves them (WAITING with the gate reference, WAITING_FOR_APPROVAL).
func TestP9FixB_WakeQueryFollowsTheGate(t *testing.T) {
	m := newMGEnv(t, mgOpts{})
	rec, ref := m.ask(t)
	if !strings.HasPrefix(ref, "gate:") {
		t.Fatalf("wait ref %q", ref)
	}
	var step string
	if err := m.pool.QueryRow(m.ctx, `SELECT step_key FROM execution_steps WHERE execution_id=$1 ORDER BY position LIMIT 1`, m.exec).Scan(&step); err != nil {
		t.Fatal(err)
	}
	mustExec(t, m.ctx, m.pool, `UPDATE jobs SET status='completed' WHERE execution_id=$1 AND status IN ('pending','processing')`, m.exec)
	cp := fmt.Sprintf(`{"references":[{"kind":"orchestrator_wait","reference_id":"task:gate:%d"}]}`, rec.ID)
	mustExec(t, m.ctx, m.pool, `UPDATE execution_steps SET kind=$3, state='WAITING', checkpoint=$4::jsonb WHERE execution_id=$1 AND step_key=$2`, m.exec, step, workflow2RunTask, cp)
	mustExec(t, m.ctx, m.pool, `UPDATE workflow_executions SET state='WAITING_FOR_APPROVAL' WHERE id=$1`, m.exec)
	woken := func() bool {
		rows, err := sqlc.New(m.pool).ListOrchestratorStepsToWake(m.ctx, sqlc.ListOrchestratorStepsToWakeParams{AtTime: ts(time.Now()), NowUnix: time.Now().Unix(), RowLimit: 100})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.ExecutionID == m.exec && r.StepKey == step {
				return true
			}
		}
		return false
	}
	if woken() {
		t.Fatal("the step is woken while the gate is open")
	}
	if toast, _, _ := m.press(rec, behavior.CallbackApprove); toast != "Đã duyệt." {
		t.Fatalf("press %q", toast)
	}
	if !woken() {
		t.Fatal("the step is not woken after the gate was decided")
	}
	// An ended execution is left to the reconciler.
	m.endExec(t, m.exec, "COMPLETED")
	if woken() {
		t.Fatal("a completed execution's step is woken")
	}
}
