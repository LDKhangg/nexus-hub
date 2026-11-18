package storage

import (
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// Phase 10 Fix A regressions (review A R-A-1..3, commit 0b49311) against
// PostgreSQL: migration 00034, failed_final, the claim and sweep rules for
// drafts of ended executions, the unknown-outcome lookup and the step wake.
// CI only; DB tests use the loopback test DB.

type p10FixEnv struct {
	*b2Env
	s *ClickUpDraftStore
}

func newP10FixEnv(t *testing.T) *p10FixEnv {
	t.Helper()
	e := newB2Env(t)
	mustExec(t, e.ctx, e.pool, `INSERT INTO integrations(id,workspace_id,platform) VALUES($1,$2,'telegram')`, integrationA, workspaceA)
	s, err := NewClickUpDraftStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	return &p10FixEnv{b2Env: e, s: s}
}

// mk makes a draft with its preview bound; exec 0 means no execution.
func (e *p10FixEnv) mk(t *testing.T, exec int64, source, key string, expires time.Time) behavior.ClickUpDraft {
	t.Helper()
	d, err := e.s.CreateDraft(e.ctx, behavior.NewClickUpDraft{
		Chat:   behavior.ChatRef{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: -1001, ThreadID: 7},
		TaskID: "86abc", Body: "🤖 x", BodySHA256: p10Sha("a"), Source: source, ExecutionID: exec, PlanTaskKey: key, ExpiresAt: expires,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.SetDraftPreview(e.ctx, workspaceA, d.ID, 500+d.ID); err != nil {
		t.Fatal(err)
	}
	d.PreviewMessageID = 500 + d.ID
	return d
}

func (e *p10FixEnv) claim(d behavior.ClickUpDraft, at time.Time) (behavior.ClickUpDraft, error) {
	return e.s.ClaimDraftPost(e.ctx, workspaceA, d.ID, d.BodySHA256, d.PreviewMessageID, at)
}

// endExec ends an execution the way the engine does (state and finish time).
func (e *p10FixEnv) endExec(t *testing.T, id int64, state string) {
	t.Helper()
	if state == "CANCELLED" {
		mustExec(t, e.ctx, e.pool, `UPDATE workflow_executions SET state=$2, cancelled_at=now(), finished_at=now() WHERE id=$1`, id, state)
		return
	}
	mustExec(t, e.ctx, e.pool, `UPDATE workflow_executions SET state=$2, finished_at=now() WHERE id=$1`, id, state)
}

func (e *p10FixEnv) get(t *testing.T, id int64) behavior.ClickUpDraft {
	t.Helper()
	d, err := e.s.GetDraft(e.ctx, workspaceA, id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TC-FA16 (R-A-1): migration 00034 Up accepts failed_final only with the body cleared and a finish time;
// Down turns failed_final rows into cancelled (reason kept) and the old checks hold again; Up accepts it again.
func TestP10Migration00034DownUp(t *testing.T) {
	e := newP10FixEnv(t)
	now := time.Now()
	d := e.mk(t, 0, behavior.DraftSourceAssistant, "", now.Add(time.Hour))
	if _, err := e.claim(d, now); err != nil {
		t.Fatal(err)
	}
	// failed_final needs the body cleared and a finish time.
	_, err := e.pool.Exec(e.ctx, `UPDATE clickup_drafts SET state='failed_final' WHERE id=$1`, d.ID)
	requirePGCode(t, err, "23514")
	_, err = e.pool.Exec(e.ctx, `UPDATE clickup_drafts SET state='failed_final', body=NULL WHERE id=$1`, d.ID)
	requirePGCode(t, err, "23514")
	_, err = e.pool.Exec(e.ctx, `UPDATE clickup_drafts SET state='failed_final', finished_at=now() WHERE id=$1`, d.ID)
	requirePGCode(t, err, "23514")
	ended, err := e.s.EndFailedDraft(e.ctx, workspaceA, d.ID, "unknown_outcome")
	if err != nil || ended.State != behavior.DraftFailedFinal || ended.Body != "" || ended.FinishedAt == nil || ended.ReasonCode != "unknown_outcome" {
		t.Fatalf("ended %+v %v", ended, err)
	}
	// A failed (retryable) draft still keeps its body and has no finish time.
	retry := e.mk(t, 0, behavior.DraftSourceAssistant, "", now.Add(time.Hour))
	if _, err := e.claim(retry, now); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.FailDraft(e.ctx, workspaceA, retry.ID, "rate_limited"); err != nil {
		t.Fatal(err)
	}
	if got := e.get(t, retry.ID); got.State != behavior.DraftFailed || got.Body == "" || got.FinishedAt != nil {
		t.Fatalf("failed draft %+v", got)
	}

	mustExec(t, e.ctx, e.pool, downSection(t, "00034_clickup_draft_failed_final.sql"))
	if got := e.get(t, d.ID); got.State != behavior.DraftCancelled || got.Body != "" || got.FinishedAt == nil || got.ReasonCode != "unknown_outcome" {
		t.Fatalf("Down left %+v", got)
	}
	if got := e.get(t, retry.ID); got.State != behavior.DraftFailed || got.Body == "" {
		t.Fatalf("Down changed a failed draft: %+v", got)
	}
	if n := e.count(`SELECT count(*) FROM clickup_drafts WHERE state='failed_final'`); n != 0 {
		t.Fatalf("Down left %d failed_final rows", n)
	}
	_, err = e.pool.Exec(e.ctx, `UPDATE clickup_drafts SET state='failed_final', body=NULL, finished_at=now() WHERE id=$1`, retry.ID)
	requirePGCode(t, err, "23514")

	mustExec(t, e.ctx, e.pool, upSection(t, "00034_clickup_draft_failed_final.sql"))
	if _, err := e.s.EndFailedDraft(e.ctx, workspaceA, retry.ID, "rate_limited"); err != nil {
		t.Fatalf("after Up: %v", err)
	}
	if got := e.get(t, retry.ID); got.State != behavior.DraftFailedFinal || got.Body != "" {
		t.Fatalf("after Up %+v", got)
	}
}

// TC-FA17 (R-A-1): EndFailedDraft ends a posting or failed draft only; the ended draft can no longer be claimed,
// cancelled, expired or reclaimed; an unsafe reason code is refused.
func TestP10FixA_EndFailedDraftRules(t *testing.T) {
	e := newP10FixEnv(t)
	now := time.Now()
	fromPosting := e.mk(t, 0, behavior.DraftSourceAssistant, "", now.Add(time.Minute))
	fromFailed := e.mk(t, 0, behavior.DraftSourceAssistant, "", now.Add(time.Minute))
	pending := e.mk(t, 0, behavior.DraftSourceAssistant, "", now.Add(time.Minute))
	posted := e.mk(t, 0, behavior.DraftSourceAssistant, "", now.Add(time.Minute))
	if _, err := e.claim(fromPosting, now); err != nil {
		t.Fatal(err)
	}
	if _, err := e.claim(fromFailed, now); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.FailDraft(e.ctx, workspaceA, fromFailed.ID, "server_error"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.claim(posted, now); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.FinishDraftPosted(e.ctx, workspaceA, posted.ID, "9", ""); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]int64{"pending": pending.ID, "posted": posted.ID} {
		if _, err := e.s.EndFailedDraft(e.ctx, workspaceA, id, "x"); !errors.Is(err, behavior.ErrConflict) {
			t.Errorf("end of a %s draft: %v", name, err)
		}
	}
	if _, err := e.s.EndFailedDraft(e.ctx, workspaceB, fromPosting.ID, "x"); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("foreign workspace: %v", err)
	}
	if _, err := e.s.EndFailedDraft(e.ctx, workspaceA, fromPosting.ID, "Bad Code!"); err == nil {
		t.Fatal("an unsafe reason code was stored")
	}
	for _, d := range []behavior.ClickUpDraft{fromPosting, fromFailed} {
		got, err := e.s.EndFailedDraft(e.ctx, workspaceA, d.ID, "policy_dry_run")
		if err != nil || got.State != behavior.DraftFailedFinal || got.Body != "" || got.FinishedAt == nil || got.ReasonCode != "policy_dry_run" {
			t.Fatalf("end %+v %v", got, err)
		}
		if _, err := e.s.EndFailedDraft(e.ctx, workspaceA, d.ID, "x"); !errors.Is(err, behavior.ErrConflict) {
			t.Errorf("second end: %v", err)
		}
		if _, err := e.claim(d, now); !errors.Is(err, behavior.ErrConflict) {
			t.Errorf("claim of an ended draft: %v", err)
		}
		if _, err := e.s.CancelDraft(e.ctx, workspaceA, d.ID); !errors.Is(err, behavior.ErrConflict) {
			t.Errorf("cancel of an ended draft: %v", err)
		}
		if _, err := e.s.FailDraft(e.ctx, workspaceA, d.ID, "x"); !errors.Is(err, behavior.ErrConflict) {
			t.Errorf("fail of an ended draft: %v", err)
		}
	}
	// Expiry and the stale-post sweep leave ended drafts alone.
	if got, err := e.s.ExpireDrafts(e.ctx, now.Add(time.Hour), 10); err != nil || len(got) != 1 || got[0].ID != pending.ID {
		t.Fatalf("expired %+v %v, want only the pending draft", got, err)
	}
	if got, err := e.s.ReclaimStalePosts(e.ctx, now.Add(time.Hour), 10); err != nil || len(got) != 0 {
		t.Fatalf("reclaimed %+v %v", got, err)
	}
	for _, d := range []behavior.ClickUpDraft{fromPosting, fromFailed} {
		if got := e.get(t, d.ID); got.State != behavior.DraftFailedFinal {
			t.Fatalf("draft %d is %s", d.ID, got.State)
		}
	}
}

// TC-FA18 (R-A-1): HasUnknownOutcomeDraft is true only for an orchestrator draft of that execution that ended
// failed_final with unknown_outcome.
func TestP10FixA_HasUnknownOutcomeDraft(t *testing.T) {
	e := newP10FixEnv(t)
	now := time.Now()
	ex, other := e.startTask(t), e.startTask(t)
	has := func(exec int64) bool {
		t.Helper()
		ok, err := e.s.HasUnknownOutcomeDraft(e.ctx, workspaceA, exec)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	end := func(d behavior.ClickUpDraft, code string) {
		t.Helper()
		if _, err := e.claim(d, now); err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.EndFailedDraft(e.ctx, workspaceA, d.ID, code); err != nil {
			t.Fatal(err)
		}
	}
	end(e.mk(t, ex, behavior.DraftSourceOrchestrator, "run_01:w", now.Add(time.Hour)), "policy_dry_run")
	end(e.mk(t, ex, behavior.DraftSourceAssistant, "", now.Add(time.Hour)), "unknown_outcome")
	pending := e.mk(t, ex, behavior.DraftSourceOrchestrator, "run_02:w", now.Add(time.Hour))
	failedRetry := e.mk(t, ex, behavior.DraftSourceOrchestrator, "run_03:w", now.Add(time.Hour))
	if _, err := e.claim(failedRetry, now); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.FailDraft(e.ctx, workspaceA, failedRetry.ID, "unknown_outcome"); err != nil {
		t.Fatal(err)
	}
	_ = pending
	if has(ex) || has(other) {
		t.Fatal("a dry run, an assistant draft, a pending draft or a retryable failure counted as unknown")
	}
	end(e.mk(t, ex, behavior.DraftSourceOrchestrator, "run_04:w", now.Add(time.Hour)), "unknown_outcome")
	if !has(ex) {
		t.Fatal("an unknown outcome was not found")
	}
	if has(other) {
		t.Fatal("another execution is blocked")
	}
	if ok, err := e.s.HasUnknownOutcomeDraft(e.ctx, workspaceA, 0); err != nil || ok {
		t.Fatalf("execution 0: %v %v", ok, err)
	}
	if ok, err := e.s.HasUnknownOutcomeDraft(e.ctx, workspaceB, ex); err != nil || ok {
		t.Fatalf("another workspace: %v %v", ok, err)
	}
	if _, err := e.s.HasUnknownOutcomeDraft(e.ctx, "not-a-uuid", ex); err == nil {
		t.Fatal("a bad workspace id was accepted")
	}
}

// TC-FA19 (R-A-2): ClaimDraftPost refuses an orchestrator draft whose execution completed, failed, was cancelled
// or is past its deadline (pending and failed drafts); a live execution, a future deadline and other draft
// sources are still claimed.
func TestP10FixA_ClaimRefusesEndedExecutions(t *testing.T) {
	e := newP10FixEnv(t)
	now := time.Now()
	for _, state := range []string{"COMPLETED", "FAILED", "CANCELLED"} {
		t.Run(state, func(t *testing.T) {
			ex := e.startTask(t)
			pending := e.mk(t, ex, behavior.DraftSourceOrchestrator, "run_01:w", now.Add(time.Hour))
			failed := e.mk(t, ex, behavior.DraftSourceOrchestrator, "run_02:w", now.Add(time.Hour))
			assistant := e.mk(t, ex, behavior.DraftSourceAssistant, "", now.Add(time.Hour))
			report := e.mk(t, ex, behavior.DraftSourceExecutionReport, "", now.Add(time.Hour))
			if _, err := e.claim(failed, now); err != nil {
				t.Fatal(err)
			}
			if _, err := e.s.FailDraft(e.ctx, workspaceA, failed.ID, "rate_limited"); err != nil {
				t.Fatal(err)
			}
			e.endExec(t, ex, state)
			for name, d := range map[string]behavior.ClickUpDraft{"pending": pending, "failed": failed} {
				if _, err := e.claim(d, now); !errors.Is(err, behavior.ErrConflict) {
					t.Errorf("%s draft claimed after the execution %s: %v", name, state, err)
				}
				if got := e.get(t, d.ID); got.State == behavior.DraftPosting {
					t.Errorf("%s draft is posting", name)
				}
			}
			for name, d := range map[string]behavior.ClickUpDraft{"assistant": assistant, "execution_report": report} {
				if _, err := e.claim(d, now); err != nil {
					t.Errorf("%s draft refused: %v", name, err)
				}
			}
		})
	}
	t.Run("deadline", func(t *testing.T) {
		ex := e.startTask(t)
		d := e.mk(t, ex, behavior.DraftSourceOrchestrator, "run_01:w", now.Add(time.Hour))
		mustExec(t, e.ctx, e.pool, `UPDATE workflow_executions SET deadline_at=$2 WHERE id=$1`, ex, now.Add(30*time.Minute))
		if _, err := e.claim(d, now.Add(31*time.Minute)); !errors.Is(err, behavior.ErrConflict) {
			t.Fatalf("claim after the deadline: %v", err)
		}
		if got, err := e.claim(d, now); err != nil || got.State != behavior.DraftPosting {
			t.Fatalf("claim before the deadline: %+v %v", got, err)
		}
	})
	t.Run("live execution", func(t *testing.T) {
		ex := e.startTask(t)
		d := e.mk(t, ex, behavior.DraftSourceOrchestrator, "run_01:w", now.Add(time.Hour))
		if got, err := e.claim(d, now); err != nil || got.State != behavior.DraftPosting {
			t.Fatalf("claim: %+v %v", got, err)
		}
	})
}

// TC-FA20 (R-A-2): CancelEndedExecutionDrafts cancels pending and failed orchestrator drafts of ended and
// past-deadline executions (reason execution_ended, body cleared); it never touches posting drafts, other sources,
// finished drafts or live executions; it honours the limit and a second call finds nothing.
func TestP10FixA_CancelEndedExecutionDrafts(t *testing.T) {
	e := newP10FixEnv(t)
	now := time.Now()
	exC, exF, exX, exD, exLive, exFuture := e.startTask(t), e.startTask(t), e.startTask(t), e.startTask(t), e.startTask(t), e.startTask(t)
	mustExec(t, e.ctx, e.pool, `UPDATE workflow_executions SET deadline_at=$2 WHERE id=$1`, exD, now.Add(-time.Minute))
	mustExec(t, e.ctx, e.pool, `UPDATE workflow_executions SET deadline_at=$2 WHERE id=$1`, exFuture, now.Add(time.Hour))
	orch := func(ex int64, key string) behavior.ClickUpDraft {
		return e.mk(t, ex, behavior.DraftSourceOrchestrator, key, now.Add(24*time.Hour))
	}
	wantCancelled := []behavior.ClickUpDraft{orch(exC, "run_01:w"), orch(exF, "run_01:w"), orch(exX, "run_01:w"), orch(exD, "run_01:w")}
	failed := orch(exC, "run_02:w")
	if _, err := e.claim(failed, now); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.FailDraft(e.ctx, workspaceA, failed.ID, "rate_limited"); err != nil {
		t.Fatal(err)
	}
	wantCancelled = append(wantCancelled, failed)
	posting := orch(exC, "run_03:w")
	if _, err := e.claim(posting, now); err != nil {
		t.Fatal(err)
	}
	posted := orch(exC, "run_04:w")
	if _, err := e.claim(posted, now); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.FinishDraftPosted(e.ctx, workspaceA, posted.ID, "9", ""); err != nil {
		t.Fatal(err)
	}
	ended := orch(exC, "run_05:w")
	if _, err := e.claim(ended, now); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.EndFailedDraft(e.ctx, workspaceA, ended.ID, "unknown_outcome"); err != nil {
		t.Fatal(err)
	}
	assistant := e.mk(t, exC, behavior.DraftSourceAssistant, "", now.Add(24*time.Hour))
	report := e.mk(t, exC, behavior.DraftSourceExecutionReport, "", now.Add(24*time.Hour))
	live := orch(exLive, "run_01:w")
	future := orch(exFuture, "run_01:w")
	e.endExec(t, exC, "COMPLETED")
	e.endExec(t, exF, "FAILED")
	e.endExec(t, exX, "CANCELLED")

	if got, err := e.s.CancelEndedExecutionDrafts(e.ctx, now, 0); err != nil || got != nil {
		t.Fatalf("limit 0: %v %v", got, err)
	}
	first, err := e.s.CancelEndedExecutionDrafts(e.ctx, now, 2)
	if err != nil || len(first) != 2 {
		t.Fatalf("limit 2: %d drafts %v", len(first), err)
	}
	rest, err := e.s.CancelEndedExecutionDrafts(e.ctx, now, 50)
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for _, d := range append(first, rest...) {
		got = append(got, d.ID)
		if d.State != behavior.DraftCancelled || d.Body != "" || d.FinishedAt == nil || d.ReasonCode != behavior.ReasonDraftExecutionEnded || d.Source != behavior.DraftSourceOrchestrator {
			t.Fatalf("cancelled draft %+v", d)
		}
	}
	var want []int64
	for _, d := range wantCancelled {
		want = append(want, d.ID)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("cancelled %v, want %v", got, want)
	}
	if again, err := e.s.CancelEndedExecutionDrafts(e.ctx, now, 50); err != nil || len(again) != 0 {
		t.Fatalf("second call: %d %v", len(again), err)
	}
	for name, c := range map[string]struct {
		d     behavior.ClickUpDraft
		state string
	}{
		"posting":          {posting, behavior.DraftPosting},
		"posted":           {posted, behavior.DraftPosted},
		"failed_final":     {ended, behavior.DraftFailedFinal},
		"assistant":        {assistant, behavior.DraftPending},
		"execution_report": {report, behavior.DraftPending},
		"live":             {live, behavior.DraftPending},
		"future deadline":  {future, behavior.DraftPending},
	} {
		if g := e.get(t, c.d.ID); g.State != c.state {
			t.Errorf("%s draft is %s, want %s", name, g.State, c.state)
		}
	}
	// The posting draft can still finish.
	if done, err := e.s.FinishDraftPosted(e.ctx, workspaceA, posting.ID, "10", ""); err != nil || done.State != behavior.DraftPosted {
		t.Fatalf("posting draft finish: %+v %v", done, err)
	}
}

// TC-FA21 (R-A-1): a waiting orchestrator step is woken once its draft is posted, cancelled, expired or
// failed_final, and not while the draft is pending, posting or failed with a retry.
func TestP10FixA_DraftEndWakesWaitingStep(t *testing.T) {
	e := newP10FixEnv(t)
	vid := e.enabledVersion(workspaceA, "p10fix").ID
	now := time.Now()
	type tc struct {
		name string
		wake bool
		move func(d behavior.ClickUpDraft)
	}
	must := func(_ behavior.ClickUpDraft, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	cases := []tc{
		{"pending", false, func(behavior.ClickUpDraft) {}},
		{"posting", false, func(d behavior.ClickUpDraft) { must(e.claim(d, now)) }},
		{"failed with retry", false, func(d behavior.ClickUpDraft) {
			must(e.claim(d, now))
			must(e.s.FailDraft(e.ctx, workspaceA, d.ID, "rate_limited"))
		}},
		{"posted", true, func(d behavior.ClickUpDraft) {
			must(e.claim(d, now))
			must(e.s.FinishDraftPosted(e.ctx, workspaceA, d.ID, "9", ""))
		}},
		{"cancelled", true, func(d behavior.ClickUpDraft) { must(e.s.CancelDraft(e.ctx, workspaceA, d.ID)) }},
		{"failed_final", true, func(d behavior.ClickUpDraft) {
			must(e.claim(d, now))
			must(e.s.EndFailedDraft(e.ctx, workspaceA, d.ID, "unknown_outcome"))
		}},
		{"expired", true, func(d behavior.ClickUpDraft) {
			mustExec(t, e.ctx, e.pool, `UPDATE clickup_drafts SET expires_at=now()-interval '1 minute' WHERE id=$1`, d.ID)
			if got, err := e.s.ExpireDrafts(e.ctx, now, 50); err != nil || len(got) != 1 || got[0].ID != d.ID {
				t.Fatalf("expire: %+v %v", got, err)
			}
		}},
	}
	want := map[int64]bool{}
	names := map[int64]string{}
	for i, c := range cases {
		ex := e.start(workspaceA, vid, "fixa-wake-"+string(rune('a'+i)), chainDef("a"))
		mustExec(t, e.ctx, e.pool, `UPDATE jobs SET status='completed' WHERE execution_id=$1 AND status='pending'`, ex.ID)
		d := e.mk(t, ex.ID, behavior.DraftSourceOrchestrator, "run_01:w", now.Add(24*time.Hour))
		cp := `{"references":[{"kind":"orchestrator_wait","reference_id":"task:draft:` + strconv.FormatInt(d.ID, 10) + `"}]}`
		mustExec(t, e.ctx, e.pool, `UPDATE execution_steps SET kind=$2, state='WAITING', checkpoint=$3::jsonb WHERE execution_id=$1 AND step_key='a'`, ex.ID, workflow2RunTask, cp)
		mustExec(t, e.ctx, e.pool, `UPDATE workflow_executions SET state='WAITING_FOR_USER' WHERE id=$1`, ex.ID)
		c.move(d)
		want[ex.ID], names[ex.ID] = c.wake, c.name
	}
	rows, err := sqlc.New(e.pool).ListOrchestratorStepsToWake(e.ctx, sqlc.ListOrchestratorStepsToWakeParams{AtTime: ts(now), NowUnix: now.Unix(), RowLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, r := range rows {
		got[r.ExecutionID] = true
	}
	var missing, extra []string
	for id, w := range want {
		switch {
		case w && !got[id]:
			missing = append(missing, names[id])
		case !w && got[id]:
			extra = append(extra, names[id])
		}
	}
	slices.Sort(missing)
	slices.Sort(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("wake query: not woken %v, woken but must not be %v", missing, extra)
	}
}
