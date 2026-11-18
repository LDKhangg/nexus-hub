package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// Phase 10 Batch A, W1 (REQ-7): the ClickUp draft store and the draft's audit trail against PostgreSQL.

func p10DraftEnv(t *testing.T) (*wfEnv, *ClickUpDraftStore) {
	t.Helper()
	e, _ := topicEnv(t)
	s, err := NewClickUpDraftStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	return e, s
}

func p10NewDraft(t *testing.T, e *wfEnv, s *ClickUpDraftStore, body string, expires time.Time) behavior.ClickUpDraft {
	t.Helper()
	d, err := s.CreateDraft(e.ctx, behavior.NewClickUpDraft{
		Chat:   behavior.ChatRef{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: -1001, ThreadID: 7},
		TaskID: "86abc", Body: body, BodySHA256: strings.Repeat("a", 64), Source: behavior.DraftSourceAssistant, ExpiresAt: expires,
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func p10Sha(c string) string { return strings.Repeat(c, 64) }

// TC-D1 (REQ-7): the draft lifecycle; terminal states clear the body and set the finish time.
func TestP10DraftStore_LifecycleClearsBody(t *testing.T) {
	e, s := p10DraftEnv(t)
	now := time.Now()
	d := p10NewDraft(t, e, s, "🤖 nội dung", now.Add(time.Hour))
	if d.State != behavior.DraftPending || d.Body != "🤖 nội dung" || d.FinishedAt != nil {
		t.Fatalf("%+v", d)
	}
	if err := s.SetDraftPreview(e.ctx, workspaceA, d.ID, 500); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimDraftPost(e.ctx, workspaceA, d.ID, p10Sha("b"), 500, now); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("wrong hash: %v", err)
	}
	if _, err := s.ClaimDraftPost(e.ctx, workspaceA, d.ID, d.BodySHA256, 501, now); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("wrong preview message: %v", err)
	}
	c, err := s.ClaimDraftPost(e.ctx, workspaceA, d.ID, d.BodySHA256, 500, now)
	if err != nil || c.State != behavior.DraftPosting || c.Body == "" {
		t.Fatalf("claim %+v %v (the body is kept while posting)", c, err)
	}
	if _, err := s.ClaimDraftPost(e.ctx, workspaceA, d.ID, d.BodySHA256, 500, now); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("second claim: %v", err)
	}
	if _, err := s.CancelDraft(e.ctx, workspaceA, d.ID); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("cancel while posting: %v", err)
	}
	done, err := s.FinishDraftPosted(e.ctx, workspaceA, d.ID, "9001", "")
	if err != nil || done.State != behavior.DraftPosted || done.Body != "" || done.FinishedAt == nil || done.PostedCommentID != "9001" {
		t.Fatalf("finish %+v %v", done, err)
	}
	for name, f := range map[string]func() error{
		"fail after posted":   func() error { _, e2 := s.FailDraft(e.ctx, workspaceA, d.ID, "x"); return e2 },
		"cancel after posted": func() error { _, e2 := s.CancelDraft(e.ctx, workspaceA, d.ID); return e2 },
		"finish twice":        func() error { _, e2 := s.FinishDraftPosted(e.ctx, workspaceA, d.ID, "1", ""); return e2 },
		"claim after posted": func() error {
			_, e2 := s.ClaimDraftPost(e.ctx, workspaceA, d.ID, d.BodySHA256, 500, now)
			return e2
		},
	} {
		if err := f(); !errors.Is(err, behavior.ErrConflict) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Cancel clears the body too.
	d2 := p10NewDraft(t, e, s, "huỷ", now.Add(time.Hour))
	c2, err := s.CancelDraft(e.ctx, workspaceA, d2.ID)
	if err != nil || c2.State != behavior.DraftCancelled || c2.Body != "" || c2.FinishedAt == nil {
		t.Fatalf("%+v %v", c2, err)
	}
	if n := e.count(`SELECT count(*) FROM clickup_drafts WHERE state IN ('posted','cancelled','expired') AND body IS NOT NULL`); n != 0 {
		t.Fatalf("%d finished drafts keep a body", n)
	}
	// Another workspace sees and changes nothing.
	if _, err := s.GetDraft(e.ctx, workspaceB, d2.ID); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("foreign get: %v", err)
	}
	if _, err := s.CancelDraft(e.ctx, workspaceB, d.ID); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("foreign cancel: %v", err)
	}
}

// TC-D2 (REQ-7, human): a burst of claims has exactly one winner.
func TestP10DraftStore_ConcurrentClaimOneWinner(t *testing.T) {
	e, s := p10DraftEnv(t)
	d := p10NewDraft(t, e, s, "một", time.Now().Add(time.Hour))
	if err := s.SetDraftPreview(e.ctx, workspaceA, d.ID, 9); err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.ClaimDraftPost(e.ctx, workspaceA, d.ID, d.BodySHA256, 9, time.Now()); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d claims won", wins.Load())
	}
}

// TC-D3 (REQ-7): Sửa replaces the body of a pending draft only, with the right old hash, and clears the preview binding.
func TestP10DraftStore_ReviseRules(t *testing.T) {
	e, s := p10DraftEnv(t)
	now := time.Now()
	d := p10NewDraft(t, e, s, "cũ", now.Add(time.Hour))
	s.SetDraftPreview(e.ctx, workspaceA, d.ID, 5)
	if _, err := s.ReviseDraft(e.ctx, workspaceA, d.ID, p10Sha("c"), "mới", p10Sha("d"), now); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("stale hash: %v", err)
	}
	r, err := s.ReviseDraft(e.ctx, workspaceA, d.ID, d.BodySHA256, "mới", p10Sha("d"), now)
	if err != nil || r.Body != "mới" || r.BodySHA256 != p10Sha("d") || r.PreviewMessageID != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	// The old buttons (old hash, old message) can no longer claim.
	if _, err := s.ClaimDraftPost(e.ctx, workspaceA, d.ID, d.BodySHA256, 5, now); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("old buttons claimed: %v", err)
	}
	if _, err := s.ReviseDraft(e.ctx, workspaceA, d.ID, p10Sha("d"), "", p10Sha("e"), now); err == nil {
		t.Fatal("an empty body was accepted")
	}
	if _, err := s.ReviseDraft(e.ctx, workspaceA, d.ID, p10Sha("d"), "x", p10Sha("e"), now.Add(2*time.Hour)); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("revise after expiry: %v", err)
	}
	s.SetDraftPreview(e.ctx, workspaceA, d.ID, 6)
	s.ClaimDraftPost(e.ctx, workspaceA, d.ID, p10Sha("d"), 6, now)
	if _, err := s.ReviseDraft(e.ctx, workspaceA, d.ID, p10Sha("d"), "x", p10Sha("e"), now); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("revise while posting: %v", err)
	}
}

// TC-D5, TC-D6 (REQ-7): failed drafts may be claimed again (Thử lại); expiry clears pending and failed bodies,
// never a posting draft or one not due.
func TestP10DraftStore_FailRetryAndExpiry(t *testing.T) {
	e, s := p10DraftEnv(t)
	now := time.Now()
	retry := p10NewDraft(t, e, s, "thử lại", now.Add(time.Hour))
	s.SetDraftPreview(e.ctx, workspaceA, retry.ID, 3)
	if _, err := s.ClaimDraftPost(e.ctx, workspaceA, retry.ID, retry.BodySHA256, 3, now); err != nil {
		t.Fatal(err)
	}
	f, err := s.FailDraft(e.ctx, workspaceA, retry.ID, "rate_limited")
	if err != nil || f.State != behavior.DraftFailed || f.ReasonCode != "rate_limited" || f.Body == "" {
		t.Fatalf("%+v %v", f, err)
	}
	if _, err := s.ClaimDraftPost(e.ctx, workspaceA, retry.ID, retry.BodySHA256, 3, now); err != nil {
		t.Fatalf("retry claim: %v", err)
	}
	if _, err := s.FailDraft(e.ctx, workspaceA, retry.ID, "Bad Code!"); err == nil {
		t.Fatal("an unsafe reason code was stored")
	}
	due := p10NewDraft(t, e, s, "đến hạn", now.Add(time.Minute))
	failedDue := p10NewDraft(t, e, s, "lỗi đến hạn", now.Add(time.Minute))
	s.SetDraftPreview(e.ctx, workspaceA, failedDue.ID, 4)
	s.ClaimDraftPost(e.ctx, workspaceA, failedDue.ID, failedDue.BodySHA256, 4, now)
	s.FailDraft(e.ctx, workspaceA, failedDue.ID, "server_error")
	later := p10NewDraft(t, e, s, "chưa hạn", now.Add(48*time.Hour))
	expired, err := s.ExpireDrafts(e.ctx, now.Add(time.Hour+time.Minute+time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]behavior.ClickUpDraft{}
	for _, x := range expired {
		got[x.ID] = x
		if x.State != behavior.DraftExpired || x.Body != "" || x.FinishedAt == nil {
			t.Fatalf("expired draft %+v", x)
		}
	}
	if len(got) != 2 || got[due.ID].ID == 0 || got[failedDue.ID].ID == 0 {
		t.Fatalf("expired %v, want the pending and failed drafts that are due", got)
	}
	if g, _ := s.GetDraft(e.ctx, workspaceA, later.ID); g.State != behavior.DraftPending || g.Body == "" {
		t.Fatalf("a draft not due changed: %+v", g)
	}
	if g, _ := s.GetDraft(e.ctx, workspaceA, retry.ID); g.State != behavior.DraftPosting {
		t.Fatalf("a posting draft was expired: %+v", g)
	}
	if again, _ := s.ExpireDrafts(e.ctx, now.Add(time.Hour+time.Minute+time.Second), 10); len(again) != 0 {
		t.Fatalf("expired again: %d", len(again))
	}
	if x, err := s.ExpireDrafts(e.ctx, now, 0); err != nil || x != nil {
		t.Fatalf("limit 0: %v %v", x, err)
	}
	// A late claim on an expired draft is a conflict.
	if _, err := s.ClaimDraftPost(e.ctx, workspaceA, due.ID, due.BodySHA256, 1, now); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("claim after expiry: %v", err)
	}
}

// TC-D7 (REQ-7, human): a draft left in posting by a crash is returned once by the sweep, with its body.
func TestP10DraftStore_ReclaimStalePosting(t *testing.T) {
	e, s := p10DraftEnv(t)
	now := time.Now()
	d := p10NewDraft(t, e, s, "sập giữa chừng", now.Add(time.Hour))
	s.SetDraftPreview(e.ctx, workspaceA, d.ID, 8)
	if _, err := s.ClaimDraftPost(e.ctx, workspaceA, d.ID, d.BodySHA256, 8, now); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReclaimStalePosts(e.ctx, now.Add(-5*time.Minute), 10); err != nil || len(got) != 0 {
		t.Fatalf("a fresh post was reclaimed: %v %v", got, err)
	}
	mustExec(t, e.ctx, e.pool, `UPDATE clickup_drafts SET updated_at = now() - interval '10 minutes' WHERE id=$1`, d.ID)
	got, err := s.ReclaimStalePosts(e.ctx, time.Now().Add(-5*time.Minute), 10)
	if err != nil || len(got) != 1 || got[0].ID != d.ID || got[0].Body != "sập giữa chừng" {
		t.Fatalf("%+v %v", got, err)
	}
	// Touched: a second sweep right away does not take it again.
	if got, _ := s.ReclaimStalePosts(e.ctx, time.Now().Add(-5*time.Minute), 10); len(got) != 0 {
		t.Fatalf("taken twice: %d", len(got))
	}
	if x, err := s.ReclaimStalePosts(e.ctx, time.Now(), 0); err != nil || x != nil {
		t.Fatalf("limit 0: %v %v", x, err)
	}
}

// TC-D11 (REQ-7): table constraints (body only while postable, caps) and a missing orchestrator draft.
func TestP10DraftStore_ConstraintsAndOrchestratorLookup(t *testing.T) {
	e, s := p10DraftEnv(t)
	d := p10NewDraft(t, e, s, "x", time.Now().Add(time.Hour))
	_, err := e.pool.Exec(e.ctx, `UPDATE clickup_drafts SET state='posted' WHERE id=$1`, d.ID) // body still set
	requirePGCode(t, err, "23514")
	_, err = e.pool.Exec(e.ctx, `UPDATE clickup_drafts SET body=$2 WHERE id=$1`, d.ID, strings.Repeat("a", 8001))
	requirePGCode(t, err, "23514")
	_, err = e.pool.Exec(e.ctx, `UPDATE clickup_drafts SET mention_user_ids='{1,2,3,4,5,6}' WHERE id=$1`, d.ID)
	requirePGCode(t, err, "23514")
	_, err = e.pool.Exec(e.ctx, `UPDATE clickup_drafts SET parent_comment_id='12a' WHERE id=$1`, d.ID)
	requirePGCode(t, err, "23514")
	_, err = e.pool.Exec(e.ctx, `UPDATE clickup_drafts SET reason_code='Bad Code' WHERE id=$1`, d.ID)
	requirePGCode(t, err, "23514")
	if _, err := s.CreateDraft(e.ctx, behavior.NewClickUpDraft{Chat: behavior.ChatRef{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: -1001},
		TaskID: "86abc", Body: "", BodySHA256: p10Sha("a"), Source: behavior.DraftSourceAssistant, ExpiresAt: time.Now().Add(time.Hour)}); err == nil {
		t.Fatal("an empty body was stored")
	}
	if _, ok, err := s.OrchestratorDraft(e.ctx, workspaceA, 999, "run_01:w"); err != nil || ok {
		t.Fatalf("missing draft: %v %v", ok, err)
	}
	if _, _, err := s.OrchestratorDraft(e.ctx, workspaceA, 0, "run_01:w"); err == nil {
		t.Fatal("execution 0 accepted")
	}
}

// p10PostTool is the ClickUp comment tool of the audit test: it counts real posts.
type p10PostTool struct{ posts atomic.Int32 }

func (p *p10PostTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{Name: "clickup_post_comment", Description: "post",
		InputSchema: json.RawMessage(`{"type":"object"}`), Tier: tenant.TierWrite}
}
func (p *p10PostTool) Run(context.Context, tools.Call) (tools.ToolOutput, error) {
	p.posts.Add(1)
	return tools.ToolOutput{Text: "comment_id=77", Summary: "comment posted comment_id=77"}, nil
}

// TC-D10 (REQ-7): a confirmed post and a dry run leave a tool_runs row owned by the draft whose input is
// "sha256:<hex> runes:<n>" and holds no comment text; the same draft replays without a second post.
func TestP10DraftAudit_ToolRunHoldsHashNotText(t *testing.T) {
	e, s := p10DraftEnv(t)
	runs, err := NewClickUpDraftToolRunStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	post := &p10PostTool{}
	reg, err := tools.NewRegistry(post)
	if err != nil {
		t.Fatal(err)
	}
	factory := behavior.DraftExecutorFactory(reg, runs, agent.NewRedactor(nil), tools.DefaultLimits())
	secret := "🤖 mật khẩu là hunter2-riêng-tư"
	for _, dry := range []bool{false, true} {
		d := p10NewDraft(t, e, s, secret, time.Now().Add(time.Hour))
		policy := tenant.ToolPolicyConfig{Enabled: []string{"clickup_post_comment"}, DryRun: dry, MaxWritesPerJob: 2}
		caller := factory(workspaceA, d.ID, policy, "clickup")
		input, _ := json.Marshal(map[string]any{"task_id": "86abc", "text": secret})
		res := caller.Call(e.ctx, 1, "clickup_post_comment", input)
		if dry && res.Status != tools.StatusDryRun || !dry && res.Status != tools.StatusOK {
			t.Fatalf("dry=%v: %+v", dry, res)
		}
		if strings.Contains(res.Content, "hunter2") {
			t.Fatalf("dry=%v: the result shows the text: %q", dry, res.Content)
		}
		var owner int64
		var in, summary string
		if err := e.pool.QueryRow(e.ctx, `SELECT clickup_draft_id, COALESCE(input_redacted::text,''), COALESCE(result_summary,'') FROM tool_runs WHERE clickup_draft_id=$1`, d.ID).Scan(&owner, &in, &summary); err != nil {
			t.Fatalf("dry=%v: %v", dry, err)
		}
		if owner != d.ID || strings.Contains(in, "hunter2") || strings.Contains(in, "mật khẩu") || strings.Contains(summary, "hunter2") {
			t.Fatalf("dry=%v: audit row keeps text: input=%s summary=%s", dry, in, summary)
		}
		if !strings.Contains(in, "sha256:") || !strings.Contains(in, "runes:") {
			t.Fatalf("dry=%v: audit input %s", dry, in)
		}
		if !dry {
			// The same draft again replays; the tool is not run a second time.
			again := factory(workspaceA, d.ID, policy, "clickup").Call(e.ctx, 1, "clickup_post_comment", input)
			if again.Status != tools.StatusOK || post.posts.Load() != 1 {
				t.Fatalf("replay: %+v posts=%d", again, post.posts.Load())
			}
		}
	}
	// No tool_runs row anywhere holds the text.
	if n := e.count(`SELECT count(*) FROM tool_runs WHERE input_redacted::text LIKE '%hunter2%' OR COALESCE(result_summary,'') LIKE '%hunter2%'`); n != 0 {
		t.Fatalf("%d rows keep the text", n)
	}
}

// TC-D11 (REQ-7): migration 00032 Down removes the draft table, the owner column and the approval kind; Up restores them.
func TestP10Migration00032DownUp(t *testing.T) {
	e, s := p10DraftEnv(t)
	d := p10NewDraft(t, e, s, "x", time.Now().Add(time.Hour))
	mustExec(t, e.ctx, e.pool, `INSERT INTO approvals(workspace_id,kind,action,resource,payload,nonce,expires_at) VALUES($1,'clickup_draft','preview','clickup_draft:1','{}','nonce-0123456789abcdef',now()+interval '1 hour')`, workspaceA)
	_ = d
	mustExec(t, e.ctx, e.pool, downSection(t, "00032_clickup_drafts.sql"))
	if n := e.count(`SELECT count(*) FROM information_schema.tables WHERE table_name='clickup_drafts'`); n != 0 {
		t.Fatalf("Down left the table: %d", n)
	}
	if n := e.count(`SELECT count(*) FROM information_schema.columns WHERE table_name='tool_runs' AND column_name='clickup_draft_id'`); n != 0 {
		t.Fatalf("Down left the column: %d", n)
	}
	if n := e.count(`SELECT count(*) FROM approvals WHERE kind='clickup_draft'`); n != 0 {
		t.Fatalf("Down left %d draft approvals", n)
	}
	mustExec(t, e.ctx, e.pool, upSection(t, "00032_clickup_drafts.sql"))
	if n := e.count(`SELECT count(*) FROM clickup_drafts`); n != 0 {
		t.Fatalf("restored table has %d rows", n)
	}
	if n := e.count(`SELECT count(*) FROM information_schema.columns WHERE table_name='tool_runs' AND column_name='clickup_draft_id'`); n != 1 {
		t.Fatalf("Up did not restore the column: %d", n)
	}
	// The restored kind check accepts clickup_draft and still refuses unknown kinds.
	mustExec(t, e.ctx, e.pool, `INSERT INTO approvals(workspace_id,kind,action,resource,payload,nonce,expires_at) VALUES($1,'clickup_draft','preview','clickup_draft:1','{}','nonce-0123456789abcdeg',now()+interval '1 hour')`, workspaceA)
	_, err := e.pool.Exec(e.ctx, `INSERT INTO approvals(workspace_id,kind,action,resource,payload,nonce,expires_at) VALUES($1,'bogus','a','r','{}','nonce-0123456789abcdeh',now()+interval '1 hour')`, workspaceA)
	requirePGCode(t, err, "23514")
	// Fix A Q1: Up of 00032 restored the original table; re-apply 00034 so the schema ends at the latest version
	// (failed_final accepted by all three constraints).
	mustExec(t, e.ctx, e.pool, upSection(t, "00034_clickup_draft_failed_final.sql"))
	if n := e.count(`SELECT count(*) FROM pg_constraint WHERE conrelid='public.clickup_drafts'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%failed_final%'`); n != 3 {
		t.Fatalf("after re-applying 00034, %d constraints know failed_final, want 3", n)
	}
}

// TC-K1d (REQ-10): the orchestrator draft lookup finds the newest draft of an execution's plan task, and only that.
func TestP10DraftStore_OrchestratorDraftLookupFindsRealRow(t *testing.T) {
	e := newB2Env(t)
	mustExec(t, e.ctx, e.pool, `INSERT INTO integrations(id,workspace_id,platform) VALUES($1,$2,'telegram')`, integrationA, workspaceA)
	s, err := NewClickUpDraftStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	ex1, ex2 := e.startTask(t), e.startTask(t)
	mk := func(exec int64, source, key string) behavior.ClickUpDraft {
		d, err := s.CreateDraft(e.ctx, behavior.NewClickUpDraft{
			Chat:   behavior.ChatRef{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: -1001, ThreadID: 7},
			TaskID: "86abc", Body: "🤖 x", BodySHA256: p10Sha("a"), Source: source, ExecutionID: exec, PlanTaskKey: key,
			ExpiresAt: time.Now().Add(time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	first := mk(ex1, behavior.DraftSourceOrchestrator, "run_01:w")
	newest := mk(ex1, behavior.DraftSourceOrchestrator, "run_01:w")
	mk(ex1, behavior.DraftSourceOrchestrator, "run_02:w")
	mk(ex2, behavior.DraftSourceOrchestrator, "run_01:w")
	mk(ex1, behavior.DraftSourceAssistant, "run_01:w")
	got, ok, err := s.OrchestratorDraft(e.ctx, workspaceA, ex1, "run_01:w")
	if err != nil || !ok || got.ID != newest.ID || got.ID == first.ID {
		t.Fatalf("found %+v ok=%v err=%v, want draft %d", got, ok, err, newest.ID)
	}
	if got.ExecutionID != ex1 || got.PlanTaskKey != "run_01:w" || got.Source != behavior.DraftSourceOrchestrator || got.Body != "🤖 x" {
		t.Fatalf("draft %+v", got)
	}
	if _, ok, err := s.OrchestratorDraft(e.ctx, workspaceA, ex1, "run_09:w"); err != nil || ok {
		t.Fatalf("other key: %v %v", ok, err)
	}
	if _, ok, err := s.OrchestratorDraft(e.ctx, workspaceB, ex1, "run_01:w"); err != nil || ok {
		t.Fatalf("other workspace: %v %v", ok, err)
	}
	// An ended draft is still found (without its body), so a retried step sees the outcome.
	if _, err := s.CancelDraft(e.ctx, workspaceA, newest.ID); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.OrchestratorDraft(e.ctx, workspaceA, ex1, "run_01:w"); err != nil || !ok || got.State != behavior.DraftCancelled || got.Body != "" {
		t.Fatalf("ended draft %+v %v %v", got, ok, err)
	}
}
