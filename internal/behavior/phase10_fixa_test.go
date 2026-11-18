package behavior

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/orchestrator"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Phase 10 Fix A regressions (review A R-A-1..3, commit 0b49311). Fakes only:
// the in-memory draft store of phase10_drafts_test.go (it now also ends
// drafts for good, cancels drafts of ended executions and answers the
// unknown-outcome question, like the SQL store), a flaky wrapper that makes
// one storage step fail, and the recording messenger.

// --- fakes (Q2: the fake store implements the optional ClickUpDraftEndStore) ---

var _ ClickUpDraftEndStore = (*p10Drafts)(nil)

func (s *p10Drafts) EndFailedDraft(_ context.Context, ws string, id int64, code string) (ClickUpDraft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.rows[id]
	if !ok || d.WorkspaceID != ws || (d.State != DraftPosting && d.State != DraftFailed) {
		return ClickUpDraft{}, ErrConflict
	}
	d.ReasonCode = code
	s.end(d, DraftFailedFinal, time.Now())
	return *d, nil
}

func (s *p10Drafts) CancelEndedExecutionDrafts(_ context.Context, now time.Time, limit int) ([]ClickUpDraft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ClickUpDraft
	for _, d := range s.rows {
		if len(out) >= limit {
			break
		}
		if d.Source == DraftSourceOrchestrator && (d.State == DraftPending || d.State == DraftFailed) && s.execOver(d.ExecutionID, now) {
			d.ReasonCode = ReasonDraftExecutionEnded
			s.end(d, DraftCancelled, now)
			out = append(out, *d)
		}
	}
	return out, nil
}

func (s *p10Drafts) HasUnknownOutcomeDraft(_ context.Context, ws string, exec int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.rows {
		if d.WorkspaceID == ws && d.ExecutionID == exec && d.Source == DraftSourceOrchestrator &&
			d.State == DraftFailedFinal && d.ReasonCode == OutcomeUnknown {
			return true, nil
		}
	}
	return false, nil
}

// p10Flaky makes the next n calls of a storage step fail with a plain error
// (not a conflict) and counts the calls that reached the store.
type p10Flaky struct {
	*p10Drafts
	mu                             sync.Mutex
	failClaim, failCancel, failRev int
	claims, cancels, revises       int
}

var errP10DB = errors.New("db down")

func (f *p10Flaky) take(n *int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if *n > 0 {
		*n--
		return true
	}
	return false
}

func (f *p10Flaky) bump(n *int) {
	f.mu.Lock()
	*n++
	f.mu.Unlock()
}

func (f *p10Flaky) ClaimDraftPost(ctx context.Context, ws string, id int64, sha string, preview int64, now time.Time) (ClickUpDraft, error) {
	if f.take(&f.failClaim) {
		return ClickUpDraft{}, errP10DB
	}
	d, err := f.p10Drafts.ClaimDraftPost(ctx, ws, id, sha, preview, now)
	if err == nil {
		f.bump(&f.claims)
	}
	return d, err
}

func (f *p10Flaky) CancelDraft(ctx context.Context, ws string, id int64) (ClickUpDraft, error) {
	if f.take(&f.failCancel) {
		return ClickUpDraft{}, errP10DB
	}
	d, err := f.p10Drafts.CancelDraft(ctx, ws, id)
	if err == nil {
		f.bump(&f.cancels)
	}
	return d, err
}

func (f *p10Flaky) ReviseDraft(ctx context.Context, ws string, id int64, oldSHA, body, sha string, now time.Time) (ClickUpDraft, error) {
	if f.take(&f.failRev) {
		return ClickUpDraft{}, errP10DB
	}
	d, err := f.p10Drafts.ReviseDraft(ctx, ws, id, oldSHA, body, sha, now)
	if err == nil {
		f.bump(&f.revises)
	}
	return d, err
}

// p10NoEnd hides the optional end store: only the plain ClickUpDraftStore.
type p10NoEnd struct{ ClickUpDraftStore }

// p10EditFails is a messenger whose in-place edits always fail.
type p10EditFails struct{ *p10Msgr }

func (p10EditFails) EditMessageWithButtons(context.Context, ChatRef, int64, string, [][]Button) error {
	return errors.New("edit failed")
}

func newP10Flaky(e *p10Env) *p10Flaky {
	f := &p10Flaky{p10Drafts: e.store}
	e.d.d.Store = f
	return f
}

func (e *p10Env) createOrch(t *testing.T, text string, exec int64, key string) int64 {
	t.Helper()
	id, err := e.d.Create(context.Background(), DraftRequest{Chat: p10Chatref(), TaskID: p10Task, Text: text,
		Source: DraftSourceOrchestrator, ExecutionID: exec, PlanTaskKey: key})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return id
}

// pressNewest presses the label on the newest message that carries it.
func (e *p10Env) pressNewest(t *testing.T, kind CallbackKind, label string) (string, string, bool) {
	t.Helper()
	msgID, m := e.msg.lastSent(t)
	return e.press(kind, e.approvalOf(t, m, label), msgID)
}

// editWith returns the last edit of a message.
func (m *p10Msgr) editOf(id int64) (p10Edit, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.edits) - 1; i >= 0; i-- {
		if m.edits[i].msgID == id {
			return m.edits[i], true
		}
	}
	return p10Edit{}, false
}

var (
	p10ResDryRun  = tools.CallResult{Content: "dry run", Status: tools.StatusDryRun}
	p10ResUnknown = tools.CallResult{Content: "unknown", IsError: true, Status: tools.StatusUnknownOutcome}
	p10ResDenied  = tools.CallResult{Content: "error: tool_not_enabled", IsError: true, Status: tools.StatusDenied}
	p10ResRetry   = tools.CallResult{Content: "error: rate_limited", IsError: true, Status: "error"}
)

// --- R-A-1: failures without a safe retry end the draft ---

// TC-FA1 (R-A-1): dry run, denied and unknown each end as failed_final with the body cleared, no buttons and
// exactly one OnFinished (State failed, with the reason); late presses and the sweep change nothing.
func TestP10FixA_NoRetryFailuresEndForGood(t *testing.T) {
	cases := []struct {
		name string
		res  tools.CallResult
		code string
	}{
		{"dry_run", p10ResDryRun, "policy_dry_run"},
		{"denied", p10ResDenied, "tool_not_enabled"},
		{"unknown", p10ResUnknown, "unknown_outcome"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newP10Env(t)
			id := e.create(t, "kết thúc hẳn")
			msgID, m := e.msg.lastSent(t)
			post, cancel := e.approvalOf(t, m, "Đăng"), e.approvalOf(t, m, "Huỷ")
			e.tool.script[ToolPostComment] = c.res
			toast, code, _ := e.press(CallbackApprove, post, msgID)
			if toast != msgDraftFailed || code != "clickup_failed" {
				t.Fatalf("toast %q code %q", toast, code)
			}
			d := e.draft(t, id)
			if d.State != DraftFailedFinal || d.Body != "" || d.FinishedAt == nil || d.ReasonCode != c.code {
				t.Fatalf("draft %+v", d)
			}
			ed, ok := e.msg.editOf(msgID)
			if !ok || len(ed.buttons) != 0 || !strings.Contains(ed.text, "❌ Không đăng được: "+c.code) || strings.Contains(ed.text, "Thử lại") {
				t.Fatalf("edit %+v ok=%v", ed, ok)
			}
			out := e.outcomes()
			if len(out) != 1 || out[0].State != DraftFailed || out[0].ReasonCode != c.code || out[0].DraftID != id {
				t.Fatalf("outcomes %+v", out)
			}
			// Late presses on the old buttons and a service cancel post and change nothing.
			if _, code, _ := e.d.HandleDraftPress(context.Background(), DraftPress{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: p10Chat,
				MessageID: msgID, Kind: CallbackApprove, Approval: post, Actor: "owner"}); code != "stale" {
				t.Fatalf("late Đăng code %q", code)
			}
			if _, code, _ := e.d.HandleDraftPress(context.Background(), DraftPress{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: p10Chat,
				MessageID: msgID, Kind: CallbackReject, Approval: cancel, Actor: "owner"}); code != "stale" {
				t.Fatalf("late Huỷ code %q", code)
			}
			if err := e.d.Cancel(context.Background(), p10WS, id); !errors.Is(err, ErrConflict) {
				t.Fatalf("cancel of an ended draft: %v", err)
			}
			e.now = e.now.Add(48 * time.Hour)
			if err := e.d.Sweep(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := e.draft(t, id); got.State != DraftFailedFinal {
				t.Fatalf("sweep changed the ended draft: %+v", got)
			}
			if n := len(e.outcomes()); n != 1 {
				t.Fatalf("%d outcomes after the sweep", n)
			}
			if n := len(e.tool.callsOf(ToolPostComment)); n != 1 {
				t.Fatalf("%d post calls", n)
			}
		})
	}
}

// TC-FA2 (R-A-1): the orchestrator task is BLOCKED with the reason of each no-retry end, the waiting step is
// woken once, and a failed draft with no reason is draft_failed.
func TestP10FixA_OrchestratorTaskBlockedAndWoken(t *testing.T) {
	cases := []struct {
		name   string
		res    tools.CallResult
		reason string
	}{
		{"dry_run", p10ResDryRun, "policy_dry_run"},
		{"denied", p10ResDenied, "tool_not_enabled"},
		{"unknown", p10ResUnknown, ReasonActionDraftUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newP10Env(t)
			w := &p10Waker{}
			e.d.OnFinished(OrchestratorDraftWaker(w))
			a := &OrchestratorActions{Drafts: e.d}
			run := p10Run(orchestrator.CapClickUpWrite)
			out, err := a.RunTask(context.Background(), run)
			if err != nil || out.Wait != "draft:1" {
				t.Fatalf("run %+v %v", out, err)
			}
			e.tool.script[ToolPostComment] = c.res
			e.pressNewest(t, CallbackApprove, "Đăng")
			out, err = a.ResumeTask(context.Background(), run, "draft:1")
			if err != nil || !out.Blocked || out.Succeeded || out.Wait != "" || out.ReasonCode != c.reason {
				t.Fatalf("resume %+v %v want blocked %q", out, err, c.reason)
			}
			if len(w.woken) != 1 || w.woken[0] != p10WS+"|run_01" {
				t.Fatalf("woken %v", w.woken)
			}
		})
	}
	t.Run("no reason code", func(t *testing.T) {
		out := draftOutcome(ClickUpDraft{ID: 4, State: DraftFailedFinal})
		if !out.Blocked || out.ReasonCode != ReasonActionDraftFailed || out.Wait != "" {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("a failed draft with a retry keeps waiting", func(t *testing.T) {
		out := draftOutcome(ClickUpDraft{ID: 4, State: DraftFailed, ReasonCode: "rate_limited"})
		if out.Wait != "draft:4" || out.Blocked {
			t.Fatalf("%+v", out)
		}
	})
}

// TC-FA3 (R-A-1): after an unknown outcome the next clickup.write task of the execution is blocked with
// draft_outcome_unknown_earlier and makes no draft; other executions and earlier dry runs do not block.
func TestP10FixA_UnknownOutcomeBlocksLaterDrafts(t *testing.T) {
	e := newP10Env(t)
	a := &OrchestratorActions{Drafts: e.d}
	run1 := p10Run(orchestrator.CapClickUpWrite)
	if _, err := a.RunTask(context.Background(), run1); err != nil {
		t.Fatal(err)
	}
	e.tool.script[ToolPostComment] = p10ResUnknown
	e.pressNewest(t, CallbackApprove, "Đăng")
	if n := len(e.tool.callsOf(ToolPostComment)); n != 1 {
		t.Fatalf("%d posts", n)
	}
	sends := e.msg.sends
	run2 := p10Run(orchestrator.CapClickUpWrite)
	run2.StepKey = "run_02"
	out, err := a.RunTask(context.Background(), run2)
	if err != nil || !out.Blocked || out.ReasonCode != ReasonActionDraftUnknownEarlier || out.Wait != "" {
		t.Fatalf("second task %+v %v", out, err)
	}
	if len(e.store.rows) != 1 || e.msg.sends != sends || len(e.tool.callsOf(ToolPostComment)) != 1 {
		t.Fatalf("a draft, preview or post was made: rows %d sends %d->%d", len(e.store.rows), sends, e.msg.sends)
	}
	// Another execution is free to draft.
	other := p10Run(orchestrator.CapClickUpWrite)
	other.ExecutionID = 13
	if out, err := a.RunTask(context.Background(), other); err != nil || out.Wait == "" {
		t.Fatalf("other execution %+v %v", out, err)
	}
	// A dry run is not an unknown outcome: the next task may draft.
	e2 := newP10Env(t)
	a2 := &OrchestratorActions{Drafts: e2.d}
	if _, err := a2.RunTask(context.Background(), run1); err != nil {
		t.Fatal(err)
	}
	e2.tool.script[ToolPostComment] = p10ResDryRun
	e2.pressNewest(t, CallbackApprove, "Đăng")
	if out, err := a2.RunTask(context.Background(), run2); err != nil || out.Wait == "" || out.Blocked {
		t.Fatalf("after a dry run %+v %v", out, err)
	}
}

type p10UnknownPort struct {
	*p10Port
	unknown bool
	err     error
}

func (p *p10UnknownPort) UnknownOutcomeEarlier(context.Context, string, int64) (bool, error) {
	return p.unknown, p.err
}

// TC-FA3 (R-A-1): the unknown-outcome check: not available means draft, an error is returned for a retry, true blocks.
func TestP10FixA_UnknownCheckResults(t *testing.T) {
	run := p10Run(orchestrator.CapClickUpWrite)
	t.Run("not available drafts", func(t *testing.T) {
		p := &p10UnknownPort{p10Port: newP10Port(), err: ErrNotAvailable}
		out, err := (&OrchestratorActions{Drafts: p}).RunTask(context.Background(), run)
		if err != nil || out.Wait == "" || len(p.created) != 1 {
			t.Fatalf("%+v %v drafts %d", out, err, len(p.created))
		}
	})
	t.Run("error is returned and nothing is drafted", func(t *testing.T) {
		p := &p10UnknownPort{p10Port: newP10Port(), err: errors.New("db")}
		if _, err := (&OrchestratorActions{Drafts: p}).RunTask(context.Background(), run); err == nil || len(p.created) != 0 {
			t.Fatalf("err %v drafts %d", err, len(p.created))
		}
	})
	t.Run("unknown blocks", func(t *testing.T) {
		p := &p10UnknownPort{p10Port: newP10Port(), unknown: true}
		out, err := (&OrchestratorActions{Drafts: p}).RunTask(context.Background(), run)
		if err != nil || !out.Blocked || out.ReasonCode != ReasonActionDraftUnknownEarlier || len(p.created) != 0 {
			t.Fatalf("%+v %v drafts %d", out, err, len(p.created))
		}
	})
	t.Run("a store without the end store cannot tell", func(t *testing.T) {
		e := newP10Env(t)
		e.d.d.Store = p10NoEnd{e.store}
		if _, err := e.d.UnknownOutcomeEarlier(context.Background(), p10WS, 12); !errors.Is(err, ErrNotAvailable) {
			t.Fatalf("err %v", err)
		}
	})
}

// TC-FA4 (R-A-1): a retryable error shows Thử lại and Huỷ and keeps waiting; Huỷ cancels, clears the body and
// fires OnFinished cancelled once; a late Thử lại is stale.
func TestP10FixA_RetryableErrorThenCancel(t *testing.T) {
	e := newP10Env(t)
	id := e.create(t, "lỗi tạm thời")
	msgID, m := e.msg.lastSent(t)
	e.tool.script[ToolPostComment] = p10ResRetry
	e.press(CallbackApprove, e.approvalOf(t, m, "Đăng"), msgID)
	ed := e.msg.lastEdit(t)
	if len(ed.buttons) != 1 || len(ed.buttons[0]) != 2 || ed.buttons[0][0].Text != "Thử lại" || ed.buttons[0][1].Text != "Huỷ" {
		t.Fatalf("buttons %+v", ed.buttons)
	}
	if d := e.draft(t, id); d.State != DraftFailed || d.Body == "" || len(e.outcomes()) != 0 {
		t.Fatalf("a retryable failure must wait: %+v outcomes %v", d, e.outcomes())
	}
	cd, _ := ParseCallbackData(ed.buttons[0][0].Data)
	retry, _ := e.ap.GetApprovalByNonce(context.Background(), p10WS, cd.Nonce)
	cd2, _ := ParseCallbackData(ed.buttons[0][1].Data)
	huy, _ := e.ap.GetApprovalByNonce(context.Background(), p10WS, cd2.Nonce)
	if toast, _, _ := e.press(CallbackReject, huy, msgID); toast != "Đã huỷ." {
		t.Fatalf("toast %q", toast)
	}
	d := e.draft(t, id)
	if d.State != DraftCancelled || d.Body != "" || d.FinishedAt == nil {
		t.Fatalf("draft %+v", d)
	}
	out := e.outcomes()
	if len(out) != 1 || out[0].State != DraftCancelled {
		t.Fatalf("outcomes %+v", out)
	}
	if _, code, _ := e.press(CallbackApprove, retry, msgID); code != "stale" {
		t.Fatalf("late Thử lại code %q", code)
	}
	if n := len(e.tool.callsOf(ToolPostComment)); n != 1 || len(e.outcomes()) != 1 {
		t.Fatalf("posts %d outcomes %d", n, len(e.outcomes()))
	}
}

// TC-FA5 (R-A-1): when the retry buttons cannot be shown (edit and send both fail) the draft ends failed_final
// instead of waiting where nobody can act on it.
func TestP10FixA_ButtonsBindFailureEndsDraft(t *testing.T) {
	e := newP10Env(t)
	id := e.create(t, "không có nút")
	msgID, m := e.msg.lastSent(t)
	post := e.approvalOf(t, m, "Đăng")
	e.d.d.Messenger = p10EditFails{e.msg}
	e.msg.failAt = e.msg.sends + 1
	e.tool.script[ToolPostComment] = p10ResRetry
	e.press(CallbackApprove, post, msgID)
	d := e.draft(t, id)
	if d.State != DraftFailedFinal || d.Body != "" || d.FinishedAt == nil || d.ReasonCode != "rate_limited" {
		t.Fatalf("draft %+v", d)
	}
	out := e.outcomes()
	if len(out) != 1 || out[0].State != DraftFailed || out[0].ReasonCode != "rate_limited" {
		t.Fatalf("outcomes %+v", out)
	}
	e.msg.mu.Lock()
	removed := append([]int64(nil), e.msg.removed...)
	e.msg.mu.Unlock()
	if len(removed) == 0 || removed[len(removed)-1] != msgID {
		t.Fatalf("buttons of the preview not removed: %v", removed)
	}
}

// TC-FA6 (R-A-1): a store without the end store keeps the older behaviour (the draft stays failed until it expires).
func TestP10FixA_StoreWithoutEndStoreKeepsFailed(t *testing.T) {
	e := newP10Env(t)
	e.d.d.Store = p10NoEnd{e.store}
	id := e.create(t, "cửa hàng cũ")
	e.tool.script[ToolPostComment] = p10ResUnknown
	e.pressNewest(t, CallbackApprove, "Đăng")
	if d := e.draft(t, id); d.State != DraftFailed || d.ReasonCode != "unknown_outcome" {
		t.Fatalf("draft %+v", d)
	}
	if err := e.d.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// --- R-A-2: drafts of ended executions ---

// TC-FA7 (R-A-2): the sweep cancels pending and failed orchestrator drafts of ended and past-deadline executions
// (with reason execution_ended and a visible note), and never touches posting drafts, assistant or execution_report
// drafts, posted drafts or drafts of a live execution; a second sweep does nothing.
func TestP10FixA_SweepCancelsDraftsOfEndedExecutions(t *testing.T) {
	e := newP10Env(t)
	w := &p10Waker{}
	e.d.OnFinished(OrchestratorDraftWaker(w))
	ctx := context.Background()
	e.store.execEnded = map[int64]bool{}
	e.store.execDeadline = map[int64]time.Time{}

	pending := e.createOrch(t, "đang chờ", 12, "run_01:w")
	failed := e.createOrch(t, "lỗi có thử lại", 12, "run_02:w")
	e.tool.script[ToolPostComment] = p10ResRetry
	e.pressNewest(t, CallbackApprove, "Đăng")
	delete(e.tool.script, ToolPostComment)
	if d := e.draft(t, failed); d.State != DraftFailed {
		t.Fatalf("setup: %+v", d)
	}
	posting := e.createOrch(t, "đang đăng", 12, "run_03:w")
	pd := e.draft(t, posting)
	if _, err := e.store.ClaimDraftPost(ctx, p10WS, posting, pd.BodySHA256, pd.PreviewMessageID, e.now); err != nil {
		t.Fatal(err)
	}
	deadline := e.createOrch(t, "quá hạn", 14, "run_04:w")
	e.store.execDeadline[14] = e.now.Add(-time.Minute)
	assistant, err := e.d.Create(ctx, DraftRequest{Chat: p10Chatref(), TaskID: p10Task, Text: "của trợ lý", Source: DraftSourceAssistant, ExecutionID: 12})
	if err != nil {
		t.Fatal(err)
	}
	report, err := e.d.Create(ctx, DraftRequest{Chat: p10Chatref(), TaskID: p10Task, Text: "báo cáo", Source: DraftSourceExecutionReport, ExecutionID: 12})
	if err != nil {
		t.Fatal(err)
	}
	live := e.createOrch(t, "execution còn chạy", 13, "run_01:w")
	e.store.execEnded[12] = true

	if err := e.d.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{pending, failed, deadline} {
		d := e.draft(t, id)
		if d.State != DraftCancelled || d.Body != "" || d.FinishedAt == nil || d.ReasonCode != ReasonDraftExecutionEnded {
			t.Fatalf("draft %d: %+v", id, d)
		}
		ed, ok := e.msg.editOf(d.PreviewMessageID)
		if !ok || len(ed.buttons) != 0 || !strings.Contains(ed.text, "🚫") || !strings.Contains(ed.text, workflow.FormatRef(d.ExecutionID)) {
			t.Fatalf("draft %d preview edit %+v ok=%v", id, ed, ok)
		}
	}
	for id, want := range map[int64]string{posting: DraftPosting, assistant: DraftPending, report: DraftPending, live: DraftPending} {
		if d := e.draft(t, id); d.State != want || d.Body == "" {
			t.Fatalf("draft %d must stay %s: %+v", id, want, d)
		}
	}
	out := e.outcomes()
	if len(out) != 3 {
		t.Fatalf("outcomes %+v", out)
	}
	for _, o := range out {
		if o.State != DraftCancelled || o.ReasonCode != ReasonDraftExecutionEnded || o.Source != DraftSourceOrchestrator {
			t.Fatalf("outcome %+v", o)
		}
	}
	if len(w.woken) != 3 {
		t.Fatalf("woken %v", w.woken)
	}
	if err := e.d.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.outcomes()) != 3 {
		t.Fatalf("second sweep made outcomes: %d", len(e.outcomes()))
	}
	if n := len(e.tool.callsOf(ToolPostComment)); n != 1 {
		t.Fatalf("%d post calls (only the failed draft's first try)", n)
	}
}

// TC-FA8 (R-A-2): a claim after the execution ended (or its deadline passed) is stale and posts nothing, for a
// pending draft and for a failed one with Thử lại; the sweep then cancels them.
func TestP10FixA_ClaimAfterExecutionEndedIsStale(t *testing.T) {
	e := newP10Env(t)
	e.store.execEnded = map[int64]bool{}
	e.store.execDeadline = map[int64]time.Time{}
	pending := e.createOrch(t, "chờ", 12, "run_01:w")
	pMsg, pm := e.msg.lastSent(t)
	pPost := e.approvalOf(t, pm, "Đăng")
	failed := e.createOrch(t, "lỗi", 12, "run_02:w")
	e.tool.script[ToolPostComment] = p10ResRetry
	e.pressNewest(t, CallbackApprove, "Đăng")
	delete(e.tool.script, ToolPostComment)
	fEdit := e.msg.lastEdit(t)
	cd, _ := ParseCallbackData(fEdit.buttons[0][0].Data)
	retry, _ := e.ap.GetApprovalByNonce(context.Background(), p10WS, cd.Nonce)
	calls := len(e.tool.callsOf(ToolPostComment))

	e.store.execEnded[12] = true
	toast, code, remove := e.press(CallbackApprove, pPost, pMsg)
	if toast != msgDraftStale || code != "stale" || !remove {
		t.Fatalf("pending press: %q %q %v", toast, code, remove)
	}
	if _, code, _ := e.press(CallbackApprove, retry, fEdit.msgID); code != "stale" {
		t.Fatalf("retry press code %q", code)
	}
	if n := len(e.tool.callsOf(ToolPostComment)); n != calls {
		t.Fatalf("posted after the execution ended: %d calls, was %d", n, calls)
	}
	for _, id := range []int64{pending, failed} {
		if d := e.draft(t, id); d.State == DraftPosting || d.State == DraftPosted {
			t.Fatalf("draft %d moved on: %+v", id, d)
		}
	}
	if err := e.d.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{pending, failed} {
		if d := e.draft(t, id); d.State != DraftCancelled {
			t.Fatalf("draft %d: %+v", id, d)
		}
	}

	// A past deadline counts like an ended execution.
	late := e.createOrch(t, "quá hạn", 15, "run_01:w")
	lMsg, lm := e.msg.lastSent(t)
	e.store.execDeadline[15] = e.now.Add(-time.Second)
	if _, code, _ := e.press(CallbackApprove, e.approvalOf(t, lm, "Đăng"), lMsg); code != "stale" {
		t.Fatalf("past deadline press code %q", code)
	}
	if d := e.draft(t, late); d.State != DraftPending {
		t.Fatalf("draft %+v", d)
	}
	// The assistant's draft is never tied to an execution's life.
	e.store.execEnded[16] = true
	as, err := e.d.Create(context.Background(), DraftRequest{Chat: p10Chatref(), TaskID: p10Task, Text: "trợ lý", Source: DraftSourceAssistant, ExecutionID: 16})
	if err != nil {
		t.Fatal(err)
	}
	if toast, _, _ := e.pressNewest(t, CallbackApprove, "Đăng"); toast != msgDraftPosted {
		t.Fatalf("assistant draft %d toast %q", as, toast)
	}
}

// --- R-A-3: replay after a storage error between Decide and Claim/Cancel/Revise ---

func (e *p10Env) reload(t *testing.T, a Approval) Approval {
	t.Helper()
	cur, err := e.ap.GetApprovalByNonce(context.Background(), p10WS, a.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	return cur
}

func (e *p10Env) pressRaw(kind CallbackKind, a Approval, msgID int64, actor string) (string, string, bool) {
	return e.d.HandleDraftPress(context.Background(), DraftPress{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: p10Chat,
		MessageID: msgID, Kind: kind, Approval: a, Actor: actor})
}

// TC-FA9 (R-A-3): Decide succeeds, Claim fails: the toast asks for a retry (code retry, not storage), the draft is
// untouched, and the replay by the same owner posts exactly once; other owners and other buttons stay stale.
func TestP10FixA_PostReplayAfterClaimError(t *testing.T) {
	e := newP10Env(t)
	f := newP10Flaky(e)
	id := e.create(t, "claim lỗi")
	msgID, m := e.msg.lastSent(t)
	post := e.approvalOf(t, m, "Đăng")
	f.failClaim = 1
	if toast, code, remove := e.pressRaw(CallbackApprove, post, msgID, "owner"); toast != msgFailed || code != "retry" || remove {
		t.Fatalf("first press: %q %q %v", toast, code, remove)
	}
	if d := e.draft(t, id); d.State != DraftPending || d.Body == "" || len(e.tool.callsOf(ToolPostComment)) != 0 {
		t.Fatalf("draft %+v after the failed claim", d)
	}
	decided := e.reload(t, post)
	if decided.Decision != DecisionApproved || decided.DecidedBy != "owner" {
		t.Fatalf("approval %+v", decided)
	}
	// Not the owner, and not the same direction: stale, nothing posts.
	if _, code, _ := e.pressRaw(CallbackApprove, decided, msgID, "intruder"); code != "stale" {
		t.Fatalf("other actor code %q", code)
	}
	if _, code, _ := e.pressRaw(CallbackReject, decided, msgID, "owner"); code != "stale" {
		t.Fatalf("other direction code %q", code)
	}
	if len(e.tool.callsOf(ToolPostComment)) != 0 || e.draft(t, id).State != DraftPending {
		t.Fatal("a wrong replay changed the draft")
	}
	if toast, code, _ := e.pressRaw(CallbackApprove, decided, msgID, "owner"); toast != msgDraftPosted || code != "" {
		t.Fatalf("replay: %q %q", toast, code)
	}
	if e.tool.realPosts() != 1 || e.draft(t, id).State != DraftPosted || f.claims != 1 {
		t.Fatalf("posts %d state %s claims %d", e.tool.realPosts(), e.draft(t, id).State, f.claims)
	}
	// A third press (a duplicate delivery) posts nothing more.
	if _, code, _ := e.pressRaw(CallbackApprove, e.reload(t, post), msgID, "owner"); code != "stale" {
		t.Fatalf("third press code %q", code)
	}
	if e.tool.realPosts() != 1 || len(e.tool.callsOf(ToolPostComment)) != 1 || len(e.outcomes()) != 1 {
		t.Fatalf("posts %d calls %d outcomes %d", e.tool.realPosts(), len(e.tool.callsOf(ToolPostComment)), len(e.outcomes()))
	}
}

// TC-FA10 (R-A-3): Decide succeeds, Cancel fails: the replay cancels exactly once, clears the body and fires
// OnFinished cancelled once.
func TestP10FixA_CancelReplayAfterCancelError(t *testing.T) {
	e := newP10Env(t)
	f := newP10Flaky(e)
	id := e.create(t, "huỷ lỗi")
	msgID, m := e.msg.lastSent(t)
	huy := e.approvalOf(t, m, "Huỷ")
	f.failCancel = 1
	if toast, code, remove := e.pressRaw(CallbackReject, huy, msgID, "owner"); toast != msgFailed || code != "retry" || remove {
		t.Fatalf("first press: %q %q %v", toast, code, remove)
	}
	if d := e.draft(t, id); d.State != DraftPending || len(e.outcomes()) != 0 {
		t.Fatalf("draft %+v outcomes %v", d, e.outcomes())
	}
	if toast, code, _ := e.pressRaw(CallbackReject, e.reload(t, huy), msgID, "owner"); toast != "Đã huỷ." || code != "" {
		t.Fatalf("replay: %q %q", toast, code)
	}
	d := e.draft(t, id)
	if d.State != DraftCancelled || d.Body != "" || f.cancels != 1 {
		t.Fatalf("draft %+v cancels %d", d, f.cancels)
	}
	if _, code, _ := e.pressRaw(CallbackReject, e.reload(t, huy), msgID, "owner"); code != "stale" {
		t.Fatalf("third press code %q", code)
	}
	if out := e.outcomes(); len(out) != 1 || out[0].State != DraftCancelled {
		t.Fatalf("outcomes %+v", out)
	}
	if len(e.tool.calls) != 0 {
		t.Fatal("a cancel posted")
	}
}

// TC-FA11 (R-A-3): the replay of a decided press never posts a draft that was edited or expired meanwhile.
func TestP10FixA_ReplayDoesNotPostChangedOrExpiredDraft(t *testing.T) {
	e := newP10Env(t)
	f := newP10Flaky(e)
	id := e.create(t, "gốc")
	msgID, m := e.msg.lastSent(t)
	post := e.approvalOf(t, m, "Đăng")
	f.failClaim = 1
	e.pressRaw(CallbackApprove, post, msgID, "owner")
	e.now = e.now.Add(25 * time.Hour)
	if _, code, _ := e.pressRaw(CallbackApprove, e.reload(t, post), msgID, "owner"); code != "stale" {
		t.Fatalf("expired replay code %q", code)
	}
	if len(e.tool.calls) != 0 || e.draft(t, id).State == DraftPosted {
		t.Fatal("an expired draft was posted")
	}
}

// TC-FA12 (R-A-3): on the Sửa reply, Revise fails: ErrDraftRetry, the old draft and buttons stay; the replay gives
// one revise and one preview, and the old buttons are dead.
func TestP10FixA_EditReplayAfterReviseError(t *testing.T) {
	e := newP10Env(t)
	f := newP10Flaky(e)
	id := e.create(t, "bản đầu")
	oldMsg, m := e.msg.lastSent(t)
	oldPost := e.approvalOf(t, m, "Đăng")
	e.press(CallbackFreeText, e.approvalOf(t, m, "Sửa"), oldMsg)
	editAp := e.reload(t, e.approvalOf(t, m, "Sửa"))
	reply := DraftReply{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: p10Chat, Approval: editAp, Text: "bản mới", Actor: "owner"}
	sends := e.msg.sends
	f.failRev = 1
	if txt, err := e.d.HandleDraftReply(context.Background(), reply); !errors.Is(err, ErrDraftRetry) || txt != "" {
		t.Fatalf("first reply: %q %v", txt, err)
	}
	d := e.draft(t, id)
	if d.Body != "🤖 bản đầu" || d.PreviewMessageID != oldMsg || d.State != DraftPending || e.msg.sends != sends {
		t.Fatalf("draft %+v sends %d->%d", d, sends, e.msg.sends)
	}
	if e.reload(t, editAp).Decision != "" || e.reload(t, oldPost).Decision != "" {
		t.Fatal("an approval was closed before the revise was saved")
	}
	if txt, err := e.d.HandleDraftReply(context.Background(), reply); err != nil || txt != msgDraftRevised {
		t.Fatalf("replay: %q %v", txt, err)
	}
	if f.revises != 1 || e.msg.sends != sends+1 {
		t.Fatalf("revises %d sends %d->%d, want 1 and one new preview", f.revises, sends, e.msg.sends)
	}
	nd := e.draft(t, id)
	if nd.Body != "🤖 bản mới" || nd.PreviewMessageID == 0 || nd.PreviewMessageID == oldMsg {
		t.Fatalf("draft %+v", nd)
	}
	if e.reload(t, editAp).Decision != DecisionAnswered {
		t.Fatalf("reply approval %+v", e.reload(t, editAp))
	}
	if _, code, _ := e.pressRaw(CallbackApprove, oldPost, oldMsg, "owner"); code != "stale" {
		t.Fatalf("old Đăng code %q", code)
	}
	if n := len(e.tool.callsOf(ToolPostComment)); n != 0 {
		t.Fatalf("%d posts with dead buttons", n)
	}
	newMsg, nm := e.msg.lastSent(t)
	e.press(CallbackApprove, e.approvalOf(t, nm, "Đăng"), newMsg)
	if c := e.tool.callsOf(ToolPostComment); len(c) != 1 || c[0].input["text"] != "🤖 bản mới" {
		t.Fatalf("calls %+v", c)
	}
}

// TC-FA13 (R-A-3): on the Sửa reply, Revise works but the preview send fails: ErrDraftRetry; the replay sends the
// one missing preview (no second revise), a duplicate replay sends nothing more.
func TestP10FixA_EditReplayAfterPreviewSendError(t *testing.T) {
	e := newP10Env(t)
	f := newP10Flaky(e)
	id := e.create(t, "bản đầu")
	oldMsg, m := e.msg.lastSent(t)
	e.press(CallbackFreeText, e.approvalOf(t, m, "Sửa"), oldMsg)
	editAp := e.reload(t, e.approvalOf(t, m, "Sửa"))
	reply := DraftReply{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: p10Chat, Approval: editAp, Text: "bản mới", Actor: "owner"}
	e.msg.failAt = e.msg.sends + 1
	if txt, err := e.d.HandleDraftReply(context.Background(), reply); !errors.Is(err, ErrDraftRetry) || txt != "" {
		t.Fatalf("first reply: %q %v", txt, err)
	}
	d := e.draft(t, id)
	if d.Body != "🤖 bản mới" || d.PreviewMessageID != 0 || d.State != DraftPending || f.revises != 1 {
		t.Fatalf("draft %+v revises %d", d, f.revises)
	}
	if ed, ok := e.msg.editOf(oldMsg); !ok || len(ed.buttons) != 0 || !strings.Contains(ed.text, "✏️") {
		t.Fatalf("old preview %+v ok=%v", ed, ok)
	}
	sends := e.msg.sends
	if txt, err := e.d.HandleDraftReply(context.Background(), reply); err != nil || txt != msgDraftRevised {
		t.Fatalf("replay: %q %v", txt, err)
	}
	if f.revises != 1 || e.msg.sends != sends+1 {
		t.Fatalf("revises %d sends %d->%d, want one more preview", f.revises, sends, e.msg.sends)
	}
	nd := e.draft(t, id)
	if nd.PreviewMessageID == 0 || nd.Body != "🤖 bản mới" {
		t.Fatalf("draft %+v", nd)
	}
	if e.reload(t, editAp).Decision != DecisionAnswered {
		t.Fatalf("reply approval %+v", e.reload(t, editAp))
	}
	// A duplicate replay: nothing more is sent.
	if _, err := e.d.HandleDraftReply(context.Background(), reply); !errors.Is(err, ErrDecided) || e.msg.sends != sends+1 {
		t.Fatalf("duplicate: %v sends %d", err, e.msg.sends)
	}
	// A replay with different text never revises again.
	other := reply
	other.Text = "khác"
	if _, err := e.d.HandleDraftReply(context.Background(), other); err == nil && e.draft(t, id).Body != "🤖 bản mới" {
		t.Fatal("a replay with other text changed the draft")
	}
	if f.revises != 1 {
		t.Fatalf("revises %d", f.revises)
	}
}

// --- inbox level: the staged replies and decided presses are replayed ---

func p10InboxWorker(e *p10Env) *InboxWorker {
	w := NewInboxWorker(InboxDeps{Approvals: e.ap, ClickUpDrafts: e.d, Messenger: e.msg}, InboxSettings{})
	w.now = func() time.Time { return e.now }
	return w
}

func p10ChatTo() ChatRef { return ChatRef{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: p10Chat} }

func p10CallbackUpdate(label string, m ttSent, msgID int64, attempts int) InboundUpdate {
	var data string
	for _, row := range m.msg.Buttons {
		for _, b := range row {
			if b.Text == label {
				data = b.Data
			}
		}
	}
	return InboundUpdate{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: p10Chat, Kind: UpdateCallback, Command: data,
		Args: "qid " + itoa(int(msgID)), Attempts: attempts}
}

// TC-FA14 (R-A-3): through the inbox worker a storage error after Decide is retried (not given up), and the retried
// press posts or cancels exactly once.
func TestP10FixA_InboxRetriesDecidedPress(t *testing.T) {
	t.Run("post", func(t *testing.T) {
		e := newP10Env(t)
		f := newP10Flaky(e)
		id := e.create(t, "qua hộp thư")
		msgID, m := e.msg.lastSent(t)
		w := p10InboxWorker(e)
		f.failClaim = 1
		out := w.callback(context.Background(), p10CallbackUpdate("Đăng", m, msgID, 1), p10ChatTo(), "owner", "c")
		if !out.retry || out.code != "storage" {
			t.Fatalf("first delivery %+v", out)
		}
		out = w.callback(context.Background(), p10CallbackUpdate("Đăng", m, msgID, 2), p10ChatTo(), "owner", "c")
		if out.retry || out.code != "" {
			t.Fatalf("second delivery %+v", out)
		}
		if e.tool.realPosts() != 1 || e.draft(t, id).State != DraftPosted {
			t.Fatalf("posts %d state %s", e.tool.realPosts(), e.draft(t, id).State)
		}
		// A third delivery of the same press is stale and posts nothing.
		if out := w.callback(context.Background(), p10CallbackUpdate("Đăng", m, msgID, 3), p10ChatTo(), "owner", "c"); out.code != "stale" {
			t.Fatalf("third delivery %+v", out)
		}
		if e.tool.realPosts() != 1 || len(e.tool.callsOf(ToolPostComment)) != 1 {
			t.Fatal("a duplicate delivery posted again")
		}
	})
	t.Run("another actor cannot take over the decided press", func(t *testing.T) {
		e := newP10Env(t)
		f := newP10Flaky(e)
		e.create(t, "chỉ chủ")
		msgID, m := e.msg.lastSent(t)
		w := p10InboxWorker(e)
		f.failClaim = 1
		w.callback(context.Background(), p10CallbackUpdate("Đăng", m, msgID, 1), p10ChatTo(), "owner", "c")
		if out := w.callback(context.Background(), p10CallbackUpdate("Đăng", m, msgID, 2), p10ChatTo(), "intruder", "c"); out.code != "stale" {
			t.Fatalf("intruder %+v", out)
		}
		if len(e.tool.calls) != 0 {
			t.Fatal("an intruder posted")
		}
	})
	t.Run("cancel", func(t *testing.T) {
		e := newP10Env(t)
		f := newP10Flaky(e)
		id := e.create(t, "huỷ qua hộp thư")
		msgID, m := e.msg.lastSent(t)
		w := p10InboxWorker(e)
		f.failCancel = 1
		out := w.callback(context.Background(), p10CallbackUpdate("Huỷ", m, msgID, 1), p10ChatTo(), "owner", "c")
		if !out.retry || out.code != "storage" {
			t.Fatalf("first delivery %+v", out)
		}
		out = w.callback(context.Background(), p10CallbackUpdate("Huỷ", m, msgID, 2), p10ChatTo(), "owner", "c")
		if out.retry || out.code != "" {
			t.Fatalf("second delivery %+v", out)
		}
		if d := e.draft(t, id); d.State != DraftCancelled || d.Body != "" || len(e.outcomes()) != 1 {
			t.Fatalf("draft %+v outcomes %v", d, e.outcomes())
		}
	})
}

// TC-FA15 (R-A-3): through the inbox worker a failed Revise keeps the staged reply and runs it again (one revise,
// one new preview); when the attempts are used up the reply is cleared and answered with a failure.
func TestP10FixA_InboxRetriesStagedEditReply(t *testing.T) {
	setup := func(t *testing.T) (*p10Env, *p10Flaky, *InboxWorker, int64, string) {
		e := newP10Env(t)
		f := newP10Flaky(e)
		id := e.create(t, "bản đầu")
		oldMsg, m := e.msg.lastSent(t)
		sua := e.approvalOf(t, m, "Sửa")
		e.press(CallbackFreeText, sua, oldMsg)
		e.ap.mu.Lock()
		e.ap.byNonce[sua.Nonce].PendingAnswer = "bản mới"
		e.ap.mu.Unlock()
		return e, f, p10InboxWorker(e), id, sua.Nonce
	}
	update := func(nonce string, attempts int) InboundUpdate {
		return InboundUpdate{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: p10Chat, Kind: UpdateText, Args: nonce, Attempts: attempts}
	}
	t.Run("retried once", func(t *testing.T) {
		e, f, w, id, nonce := setup(t)
		f.failRev = 1
		out := w.freeText(context.Background(), update(nonce, 1), p10ChatTo(), "owner", "c")
		if !out.retry || out.code != "storage" {
			t.Fatalf("first delivery %+v", out)
		}
		if e.draft(t, id).Body != "🤖 bản đầu" {
			t.Fatal("the draft changed before the revise")
		}
		sends := e.msg.sends
		out = w.freeText(context.Background(), update(nonce, 2), p10ChatTo(), "owner", "c")
		if out.retry || out.code != "" {
			t.Fatalf("second delivery %+v", out)
		}
		if f.revises != 1 || e.draft(t, id).Body != "🤖 bản mới" || e.draft(t, id).PreviewMessageID == 0 {
			t.Fatalf("revises %d draft %+v", f.revises, e.draft(t, id))
		}
		if e.msg.sends < sends+2 { // the new preview and the confirmation reply
			t.Fatalf("sends %d->%d", sends, e.msg.sends)
		}
	})
	t.Run("attempts used up", func(t *testing.T) {
		e, f, w, id, nonce := setup(t)
		f.failRev = 1
		out := w.freeText(context.Background(), update(nonce, 3), p10ChatTo(), "owner", "c")
		if out.retry || out.code != "handler_failed" {
			t.Fatalf("outcome %+v", out)
		}
		if e.draft(t, id).Body != "🤖 bản đầu" || f.revises != 0 {
			t.Fatalf("draft %+v revises %d", e.draft(t, id), f.revises)
		}
	})
}
