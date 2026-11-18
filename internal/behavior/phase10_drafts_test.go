package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// Phase 10 Batch A, W1 (REQ-7): ClickUp draft and confirm. Fakes only: an
// in-memory draft store with the state machine of the SQL store, a fake
// tool caller that replays by draft id like the executor, a recording
// messenger.

const (
	p10WS   = "ws-p10"
	p10Bot  = "bot-p10"
	p10Chat = int64(-1009990001)
	p10Task = "86abc12"
)

type p10Edit struct {
	msgID   int64
	text    string
	buttons [][]Button
}

type p10Msgr struct {
	mu      sync.Mutex
	sent    []ttSent
	edits   []p10Edit
	removed []int64
	next    int64
	failAt  int // 1-based send number that fails; 0 never
	sends   int
}

func (m *p10Msgr) SendMessage(_ context.Context, to ChatRef, msg OutMessage) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sends++
	if m.failAt != 0 && m.sends == m.failAt {
		return 0, errors.New("send failed")
	}
	m.next++
	m.sent = append(m.sent, ttSent{to, msg})
	return 500 + m.next, nil
}
func (m *p10Msgr) RemoveButtons(_ context.Context, _ ChatRef, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, id)
	return nil
}
func (m *p10Msgr) AnswerCallback(context.Context, ChatRef, string, string) error { return nil }
func (m *p10Msgr) EditMessageWithButtons(_ context.Context, _ ChatRef, id int64, text string, b [][]Button) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.edits = append(m.edits, p10Edit{id, text, b})
	return nil
}
func (m *p10Msgr) lastEdit(t *testing.T) p10Edit {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.edits) == 0 {
		t.Fatal("no preview edit")
	}
	return m.edits[len(m.edits)-1]
}
func (m *p10Msgr) lastSent(t *testing.T) (int64, ttSent) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sent) == 0 {
		t.Fatal("nothing sent")
	}
	return 500 + int64(len(m.sent)), m.sent[len(m.sent)-1]
}

// p10Drafts is an in-memory ClickUpDraftStore with the SQL store's rules.
type p10Drafts struct {
	mu   sync.Mutex
	rows map[int64]*ClickUpDraft
	next int64
}

func newP10Drafts() *p10Drafts { return &p10Drafts{rows: map[int64]*ClickUpDraft{}} }

func (s *p10Drafts) end(d *ClickUpDraft, state string, now time.Time) {
	d.State, d.Body = state, ""
	d.FinishedAt = &now
}

func (s *p10Drafts) CreateDraft(_ context.Context, n NewClickUpDraft) (ClickUpDraft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	d := &ClickUpDraft{ID: s.next, WorkspaceID: n.Chat.WorkspaceID, IntegrationID: n.Chat.IntegrationID, ChatID: n.Chat.ChatID,
		ThreadID: n.Chat.ThreadID, TaskID: n.TaskID, ParentCommentID: n.ParentCommentID, MentionUserIDs: n.MentionUserIDs,
		NewStatus: n.NewStatus, Body: n.Body, BodySHA256: n.BodySHA256, State: DraftPending, Source: n.Source,
		ExecutionID: n.ExecutionID, PlanTaskKey: n.PlanTaskKey, ExpiresAt: n.ExpiresAt, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	s.rows[d.ID] = d
	return *d, nil
}
func (s *p10Drafts) GetDraft(_ context.Context, ws string, id int64) (ClickUpDraft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.rows[id]
	if !ok || d.WorkspaceID != ws {
		return ClickUpDraft{}, ErrNotFound
	}
	return *d, nil
}
func (s *p10Drafts) SetDraftPreview(_ context.Context, ws string, id, msg int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.rows[id]
	if !ok || d.WorkspaceID != ws {
		return ErrNotFound
	}
	d.PreviewMessageID = msg
	return nil
}
func (s *p10Drafts) ReviseDraft(_ context.Context, ws string, id int64, oldSHA, body, sha string, now time.Time) (ClickUpDraft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.rows[id]
	if !ok || d.WorkspaceID != ws || d.State != DraftPending || !now.Before(d.ExpiresAt) || d.BodySHA256 != oldSHA {
		return ClickUpDraft{}, ErrConflict
	}
	d.Body, d.BodySHA256, d.PreviewMessageID = body, sha, 0
	return *d, nil
}
func (s *p10Drafts) ClaimDraftPost(_ context.Context, ws string, id int64, sha string, preview int64, now time.Time) (ClickUpDraft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.rows[id]
	if !ok || d.WorkspaceID != ws || (d.State != DraftPending && d.State != DraftFailed) || !now.Before(d.ExpiresAt) ||
		d.BodySHA256 != sha || d.PreviewMessageID != preview {
		return ClickUpDraft{}, ErrConflict
	}
	d.State, d.UpdatedAt = DraftPosting, now
	return *d, nil
}
func (s *p10Drafts) FinishDraftPosted(_ context.Context, ws string, id int64, comment, reason string) (ClickUpDraft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.rows[id]
	if !ok || d.WorkspaceID != ws || d.State != DraftPosting {
		return ClickUpDraft{}, ErrConflict
	}
	d.PostedCommentID, d.ReasonCode = comment, reason
	s.end(d, DraftPosted, time.Now())
	return *d, nil
}
func (s *p10Drafts) FailDraft(_ context.Context, ws string, id int64, code string) (ClickUpDraft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.rows[id]
	if !ok || d.WorkspaceID != ws || d.State != DraftPosting {
		return ClickUpDraft{}, ErrConflict
	}
	d.State, d.ReasonCode = DraftFailed, code
	return *d, nil
}
func (s *p10Drafts) CancelDraft(_ context.Context, ws string, id int64) (ClickUpDraft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.rows[id]
	if !ok || d.WorkspaceID != ws || (d.State != DraftPending && d.State != DraftFailed) {
		return ClickUpDraft{}, ErrConflict
	}
	s.end(d, DraftCancelled, time.Now())
	return *d, nil
}
func (s *p10Drafts) ExpireDrafts(_ context.Context, now time.Time, limit int) ([]ClickUpDraft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ClickUpDraft
	for _, d := range s.rows {
		if len(out) < limit && (d.State == DraftPending || d.State == DraftFailed) && !now.Before(d.ExpiresAt) {
			s.end(d, DraftExpired, now)
			out = append(out, *d)
		}
	}
	return out, nil
}
func (s *p10Drafts) ReclaimStalePosts(_ context.Context, before time.Time, limit int) ([]ClickUpDraft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ClickUpDraft
	for _, d := range s.rows {
		if len(out) < limit && d.State == DraftPosting && d.UpdatedAt.Before(before) {
			d.UpdatedAt = time.Now()
			out = append(out, *d)
		}
	}
	return out, nil
}

// p10Tool is the fake tool caller shared by every draft's confirm. Like
// the executor, a repeated call of the same draft and step replays the
// first success instead of posting again.
type p10Tool struct {
	mu       sync.Mutex
	calls    []p10Call
	posted   map[string]int // key -> real posts
	script   map[string]tools.CallResult
	policies []tenant.ToolPolicyConfig
	factory  []int64
}

type p10Call struct {
	draft int64
	name  string
	input map[string]any
}

func newP10Tool() *p10Tool {
	return &p10Tool{posted: map[string]int{}, script: map[string]tools.CallResult{}}
}

type p10Caller struct {
	t     *p10Tool
	draft int64
}

func (c p10Caller) Call(_ context.Context, _ int, name string, in json.RawMessage) tools.CallResult {
	c.t.mu.Lock()
	defer c.t.mu.Unlock()
	var m map[string]any
	_ = json.Unmarshal(in, &m)
	c.t.calls = append(c.t.calls, p10Call{c.draft, name, m})
	key := name + ":" + string(rune('0'+c.draft))
	if r, ok := c.t.script[name]; ok {
		return r
	}
	if c.t.posted[key] > 0 {
		return tools.CallResult{Content: "already done earlier in this job", Status: tools.StatusOK}
	}
	c.t.posted[key]++
	return tools.CallResult{Content: "ok comment_id=9001", Status: tools.StatusOK}
}

func (t *p10Tool) factoryFn() DraftCallerFactory {
	return func(_ string, draftID int64, policy tenant.ToolPolicyConfig, _ string) ToolCaller {
		t.mu.Lock()
		t.policies = append(t.policies, policy)
		t.factory = append(t.factory, draftID)
		t.mu.Unlock()
		return p10Caller{t, draftID}
	}
}

func (t *p10Tool) callsOf(name string) []p10Call {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []p10Call
	for _, c := range t.calls {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

func (t *p10Tool) realPosts() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for k, v := range t.posted {
		if strings.HasPrefix(k, ToolPostComment) {
			n += v
		}
	}
	return n
}

type p10Policy struct{ dryRun bool }

func (p p10Policy) RunPolicy(context.Context, string, string) (tenant.AgentConfig, error) {
	return tenant.AgentConfig{ClickUpTokenRef: "clickup", Tools: tenant.ToolPolicyConfig{DryRun: p.dryRun}}, nil
}

type p10Env struct {
	d     *ClickUpDrafts
	store *p10Drafts
	ap    *p4Approvals
	msg   *p10Msgr
	tool  *p10Tool
	now   time.Time
	out   []ClickUpDraftOutcome
	mu    sync.Mutex
}

func newP10Env(t *testing.T) *p10Env {
	t.Helper()
	e := &p10Env{store: newP10Drafts(), ap: newP4Approvals(), msg: &p10Msgr{}, tool: newP10Tool(), now: time.Now()}
	d, err := NewClickUpDrafts(ClickUpDraftDeps{Store: e.store, Approvals: e.ap, Messenger: e.msg, Policies: p10Policy{}, Callers: e.tool.factoryFn()}, ClickUpDraftSettings{})
	if err != nil {
		t.Fatal(err)
	}
	d.now = func() time.Time { return e.now }
	d.OnFinished(func(_ context.Context, o ClickUpDraftOutcome) {
		e.mu.Lock()
		e.out = append(e.out, o)
		e.mu.Unlock()
	})
	e.d = d
	return e
}

func (e *p10Env) outcomes() []ClickUpDraftOutcome {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]ClickUpDraftOutcome(nil), e.out...)
}

func p10Chatref() ChatRef {
	return ChatRef{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: p10Chat, ThreadID: 77}
}

func (e *p10Env) create(t *testing.T, text string) int64 {
	t.Helper()
	id, err := e.d.Create(context.Background(), DraftRequest{Chat: p10Chatref(), TaskID: p10Task, Text: text, Source: DraftSourceAssistant})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return id
}

// button returns the approval behind the button with this label on the
// newest message that carries it.
func (e *p10Env) approvalOf(t *testing.T, m ttSent, label string) Approval {
	t.Helper()
	for _, row := range m.msg.Buttons {
		for _, b := range row {
			if b.Text == label {
				cd, err := ParseCallbackData(b.Data)
				if err != nil {
					t.Fatalf("button data: %v", err)
				}
				a, err := e.ap.GetApprovalByNonce(context.Background(), p10WS, cd.Nonce)
				if err != nil {
					t.Fatal(err)
				}
				return a
			}
		}
	}
	t.Fatalf("no %q button", label)
	return Approval{}
}

func (e *p10Env) press(kind CallbackKind, a Approval, msgID int64) (string, string, bool) {
	// The inbound worker only passes open, unexpired approvals.
	cur, err := e.ap.GetApprovalByNonce(context.Background(), p10WS, a.Nonce)
	if err == nil {
		a = cur
	}
	if a.Decision != "" {
		return msgStale, "stale", true // the worker answers this before the handler
	}
	return e.d.HandleDraftPress(context.Background(), DraftPress{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: p10Chat,
		MessageID: msgID, Kind: kind, Approval: a, Actor: "owner"})
}

func (e *p10Env) draft(t *testing.T, id int64) ClickUpDraft {
	t.Helper()
	d, err := e.d.Get(context.Background(), p10WS, id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TC-D1 (REQ-7): a draft shows an exact preview with three buttons and posts nothing.
func TestP10Draft_CreateShowsExactPreviewAndPostsNothing(t *testing.T) {
	e := newP10Env(t)
	id := e.create(t, "Đã xong, mời anh kiểm tra 🙏")
	if n := len(e.tool.calls); n != 0 {
		t.Fatalf("a draft must not post; %d calls", n)
	}
	msgID, m := e.msg.lastSent(t)
	if m.to.ThreadID != 77 || m.to.ChatID != p10Chat {
		t.Fatalf("preview went to %+v", m.to)
	}
	var labels []string
	for _, b := range m.msg.Buttons[0] {
		labels = append(labels, b.Text)
		if len(b.Data) > 64 {
			t.Fatalf("callback data %d bytes", len(b.Data))
		}
	}
	if strings.Join(labels, "|") != "Đăng|Sửa|Huỷ" {
		t.Fatalf("buttons %v", labels)
	}
	if !strings.Contains(m.msg.Text, "🤖 Đã xong, mời anh kiểm tra 🙏") || !strings.Contains(m.msg.Text, p10Task) {
		t.Fatalf("preview lacks the exact body or task: %q", m.msg.Text)
	}
	d := e.draft(t, id)
	if d.State != DraftPending || d.PreviewMessageID != msgID || !strings.HasPrefix(d.Body, "🤖 ") {
		t.Fatalf("draft %+v", d)
	}
}

// TC-D1 (REQ-7): Đăng posts exactly once with the draft's body, edits the preview and clears the body.
func TestP10Draft_PostOnceEditsPreviewAndClearsBody(t *testing.T) {
	e := newP10Env(t)
	id := e.create(t, "xong rồi")
	msgID, m := e.msg.lastSent(t)
	ap := e.approvalOf(t, m, "Đăng")
	toast, code, _ := e.press(CallbackApprove, ap, msgID)
	if toast != msgDraftPosted || code != "" {
		t.Fatalf("toast %q code %q", toast, code)
	}
	calls := e.tool.callsOf(ToolPostComment)
	if len(calls) != 1 || calls[0].draft != id || calls[0].input["task_id"] != p10Task || calls[0].input["text"] != "🤖 xong rồi" {
		t.Fatalf("calls %+v", calls)
	}
	if _, has := calls[0].input["parent_comment_id"]; has {
		t.Fatal("no parent expected")
	}
	ed := e.msg.lastEdit(t)
	if ed.msgID != msgID || !strings.Contains(ed.text, "✅ Đã đăng") || !strings.Contains(ed.text, "https://app.clickup.com/t/"+p10Task) || len(ed.buttons) != 0 {
		t.Fatalf("edit %+v", ed)
	}
	d := e.draft(t, id)
	if d.State != DraftPosted || d.Body != "" || d.PostedCommentID != "9001" {
		t.Fatalf("draft %+v", d)
	}
	if o := e.outcomes(); len(o) != 1 || o[0].State != DraftPosted || o[0].PostedCommentID != "9001" || o[0].DraftID != id {
		t.Fatalf("outcomes %+v", o)
	}
	for _, p := range e.tool.policies {
		if p.MaxWritesPerJob != 2 || len(p.Enabled) != 1 || p.Enabled[0] != ToolPostComment {
			t.Fatalf("policy %+v", p)
		}
	}
}

// TC-D2 (REQ-7, human): the same press twice and a burst of presses post one comment.
func TestP10Draft_DoublePressAndBurstPostOnce(t *testing.T) {
	e := newP10Env(t)
	e.create(t, "một lần thôi")
	msgID, m := e.msg.lastSent(t)
	ap := e.approvalOf(t, m, "Đăng")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.d.HandleDraftPress(context.Background(), DraftPress{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: p10Chat,
				MessageID: msgID, Kind: CallbackApprove, Approval: ap, Actor: "owner"})
		}()
	}
	wg.Wait()
	if n := e.tool.realPosts(); n != 1 {
		t.Fatalf("%d comments posted", n)
	}
	if n := len(e.tool.callsOf(ToolPostComment)); n != 1 {
		t.Fatalf("%d tool calls", n)
	}
	// A later press by hand is stale and calls nothing.
	if _, code, _ := e.press(CallbackApprove, ap, msgID); code != "stale" {
		t.Fatalf("code %q", code)
	}
	if n := len(e.tool.callsOf(ToolPostComment)); n != 1 {
		t.Fatalf("%d tool calls after second press", n)
	}
}

// TC-D3 (REQ-7, human): Sửa then a reply replaces the text; the old buttons stop working.
func TestP10Draft_EditReplacesTextAndOldButtonsDie(t *testing.T) {
	e := newP10Env(t)
	id := e.create(t, "bản đầu")
	oldMsg, m1 := e.msg.lastSent(t)
	oldPost := e.approvalOf(t, m1, "Đăng")
	oldEdit := e.approvalOf(t, m1, "Sửa")
	toast, _, _ := e.press(CallbackFreeText, oldEdit, oldMsg)
	if toast != msgDraftEditHint {
		t.Fatalf("toast %q", toast)
	}
	editAp, _ := e.ap.GetApprovalByNonce(context.Background(), p10WS, oldEdit.Nonce)
	if editAp.ReplyMessageID != oldMsg {
		t.Fatalf("reply message %d want %d", editAp.ReplyMessageID, oldMsg)
	}
	reply, err := e.d.HandleDraftReply(context.Background(), DraftReply{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: p10Chat,
		Approval: editAp, Text: "bản mới", Actor: "owner"})
	if err != nil || reply != msgDraftRevised {
		t.Fatalf("reply %q err %v", reply, err)
	}
	newMsg, m2 := e.msg.lastSent(t)
	if newMsg == oldMsg || !strings.Contains(m2.msg.Text, "🤖 bản mới") {
		t.Fatalf("new preview %q", m2.msg.Text)
	}
	if ed := e.msg.lastEdit(t); ed.msgID != oldMsg || len(ed.buttons) != 0 {
		t.Fatalf("old preview not retired: %+v", ed)
	}
	// Old Đăng: stale, nothing posted.
	if _, code, _ := e.press(CallbackApprove, oldPost, oldMsg); code != "stale" {
		t.Fatalf("old press: code %q", code)
	}
	// Old Đăng even if the worker passed a still-open approval snapshot.
	if _, code, _ := e.d.HandleDraftPress(context.Background(), DraftPress{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: p10Chat,
		MessageID: oldMsg, Kind: CallbackApprove, Approval: oldPost, Actor: "owner"}); code != "stale" {
		t.Fatalf("snapshot press code %q", code)
	}
	if n := len(e.tool.callsOf(ToolPostComment)); n != 0 {
		t.Fatalf("posted with old buttons: %d", n)
	}
	e.press(CallbackApprove, e.approvalOf(t, m2, "Đăng"), newMsg)
	calls := e.tool.callsOf(ToolPostComment)
	if len(calls) != 1 || calls[0].input["text"] != "🤖 bản mới" || calls[0].draft != id {
		t.Fatalf("calls %+v", calls)
	}
}

// TC-D3 (REQ-7, abuse): an empty or oversized edit keeps the draft and its buttons.
func TestP10Draft_BadEditTextKeepsDraft(t *testing.T) {
	e := newP10Env(t)
	e.create(t, "gốc")
	msgID, m := e.msg.lastSent(t)
	editAp := e.approvalOf(t, m, "Sửa")
	for _, txt := range []string{"", "   \n", strings.Repeat("a", 8000)} { // 8000 + marker > 8000
		reply, err := e.d.HandleDraftReply(context.Background(), DraftReply{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: p10Chat,
			Approval: editAp, Text: txt, Actor: "owner"})
		if err != nil || !strings.Contains(reply, "8000") {
			t.Fatalf("text %d runes: reply %q err %v", len(txt), reply, err)
		}
	}
	if d := e.draft(t, 1); d.Body != "🤖 gốc" || d.PreviewMessageID != msgID {
		t.Fatalf("draft changed: %+v", d)
	}
	e.press(CallbackApprove, e.approvalOf(t, m, "Đăng"), msgID)
	if n := len(e.tool.callsOf(ToolPostComment)); n != 1 {
		t.Fatalf("original buttons broke: %d calls", n)
	}
}

// TC-D4 (REQ-7, abuse): presses from another chat, another message or another workspace do nothing.
func TestP10Draft_PressBoundToChatMessageAndWorkspace(t *testing.T) {
	e := newP10Env(t)
	e.create(t, "chỉ đúng chỗ")
	msgID, m := e.msg.lastSent(t)
	ap := e.approvalOf(t, m, "Đăng")
	base := DraftPress{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: p10Chat, MessageID: msgID, Kind: CallbackApprove, Approval: ap, Actor: "x"}
	cases := map[string]func(DraftPress) DraftPress{
		"other chat":        func(p DraftPress) DraftPress { p.ChatID = p10Chat - 1; return p },
		"other message":     func(p DraftPress) DraftPress { p.MessageID = msgID + 5; return p },
		"zero message":      func(p DraftPress) DraftPress { p.MessageID = 0; return p },
		"other workspace":   func(p DraftPress) DraftPress { p.WorkspaceID = "ws-other"; return p },
		"other integration": func(p DraftPress) DraftPress { p.IntegrationID = "bot-other"; return p },
		"foreign kind":      func(p DraftPress) DraftPress { p.Approval.Kind = ApprovalItemAnswer; return p },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			p := mod(base)
			if _, code, _ := e.d.HandleDraftPress(context.Background(), p); code != "stale" {
				t.Fatalf("code %q", code)
			}
		})
	}
	if n := len(e.tool.calls); n != 0 {
		t.Fatalf("%d tool calls", n)
	}
	if d := e.draft(t, 1); d.State != DraftPending {
		t.Fatalf("state %s", d.State)
	}
}

// TC-D5 (REQ-7): Huỷ and expiry post nothing, clear the body and notify listeners.
func TestP10Draft_CancelAndExpiryPostNothing(t *testing.T) {
	e := newP10Env(t)
	cancelID := e.create(t, "sẽ huỷ")
	cMsg, cm := e.msg.lastSent(t)
	expID := e.create(t, "sẽ hết hạn")
	eMsg, em := e.msg.lastSent(t)
	if toast, _, _ := e.press(CallbackReject, e.approvalOf(t, cm, "Huỷ"), cMsg); toast != "Đã huỷ." {
		t.Fatalf("toast %q", toast)
	}
	if ed := e.msg.lastEdit(t); ed.msgID != cMsg || !strings.Contains(ed.text, "🚫") {
		t.Fatalf("edit %+v", ed)
	}
	// Late Đăng on the cancelled draft does nothing.
	if _, code, _ := e.press(CallbackApprove, e.approvalOf(t, cm, "Đăng"), cMsg); code != "stale" {
		t.Fatalf("code %q", code)
	}
	e.now = e.now.Add(25 * time.Hour)
	if err := e.d.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ed := e.msg.lastEdit(t); ed.msgID != eMsg || !strings.Contains(ed.text, "⌛") {
		t.Fatalf("edit %+v", ed)
	}
	// Late Đăng after expiry: nothing posted.
	if _, code, _ := e.d.HandleDraftPress(context.Background(), DraftPress{WorkspaceID: p10WS, IntegrationID: p10Bot, ChatID: p10Chat,
		MessageID: eMsg, Kind: CallbackApprove, Approval: e.approvalOf(t, em, "Đăng"), Actor: "owner"}); code != "stale" {
		t.Fatalf("code %q", code)
	}
	if n := len(e.tool.calls); n != 0 {
		t.Fatalf("%d tool calls", n)
	}
	for _, id := range []int64{cancelID, expID} {
		if d := e.draft(t, id); d.Body != "" || d.FinishedAt == nil {
			t.Fatalf("draft %d keeps body or has no finish time: %+v", id, d)
		}
	}
	st := map[string]bool{}
	for _, o := range e.outcomes() {
		st[o.State] = true
	}
	if !st[DraftCancelled] || !st[DraftExpired] || len(e.outcomes()) != 2 {
		t.Fatalf("outcomes %+v", e.outcomes())
	}
}

// TC-D6 (REQ-7): a ClickUp error shows the code and Thử lại; retry posts; unknown and dry-run offer none.
func TestP10Draft_ErrorShowsCodeAndRetryPosts(t *testing.T) {
	e := newP10Env(t)
	id := e.create(t, "thử lại sau")
	msgID, m := e.msg.lastSent(t)
	e.tool.script[ToolPostComment] = tools.CallResult{Content: "error: rate_limited", IsError: true, Status: "error"}
	if toast, code, _ := e.press(CallbackApprove, e.approvalOf(t, m, "Đăng"), msgID); toast != msgDraftFailed || code != "clickup_failed" {
		t.Fatalf("toast %q code %q", toast, code)
	}
	ed := e.msg.lastEdit(t)
	if !strings.Contains(ed.text, "❌ Không đăng được: rate_limited") || len(ed.buttons) != 1 || len(ed.buttons[0]) != 2 || ed.buttons[0][0].Text != "Thử lại" {
		t.Fatalf("edit %+v", ed)
	}
	if d := e.draft(t, id); d.State != DraftFailed || d.Body == "" {
		t.Fatalf("draft %+v", d)
	}
	cd, _ := ParseCallbackData(ed.buttons[0][0].Data)
	retry, err := e.ap.GetApprovalByNonce(context.Background(), p10WS, cd.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	delete(e.tool.script, ToolPostComment)
	if toast, _, _ := e.press(CallbackApprove, retry, msgID); toast != msgDraftPosted {
		t.Fatalf("retry toast %q", toast)
	}
	if e.tool.realPosts() != 1 || e.draft(t, id).State != DraftPosted {
		t.Fatalf("posts %d state %s", e.tool.realPosts(), e.draft(t, id).State)
	}
}

func TestP10Draft_NoRetryAfterUnknownOutcomeOrDryRun(t *testing.T) {
	cases := map[string]tools.CallResult{
		"unknown_outcome": {Content: "unknown", IsError: true, Status: tools.StatusUnknownOutcome},
		"policy_dry_run":  {Content: "dry run", Status: tools.StatusDryRun},
	}
	for want, res := range cases {
		t.Run(want, func(t *testing.T) {
			e := newP10Env(t)
			id := e.create(t, "không thử lại")
			msgID, m := e.msg.lastSent(t)
			e.tool.script[ToolPostComment] = res
			e.press(CallbackApprove, e.approvalOf(t, m, "Đăng"), msgID)
			ed := e.msg.lastEdit(t)
			if !strings.Contains(ed.text, want) || len(ed.buttons) != 0 || strings.Contains(ed.text, "Thử lại") {
				t.Fatalf("edit %+v", ed)
			}
			if d := e.draft(t, id); d.State != DraftFailed || d.ReasonCode != want {
				t.Fatalf("draft %+v", d)
			}
			// The failure text must never carry the comment body.
			if strings.Contains(ed.text, "không thử lại\n") && strings.Contains(ed.text, "🤖") {
				t.Fatalf("failure shows body: %q", ed.text)
			}
		})
	}
}

// TC-D7 (REQ-7, human): a crash in posting is finished by the sweep with the same draft id, one comment.
func TestP10Draft_StalePostingIsReplayedOnce(t *testing.T) {
	e := newP10Env(t)
	id := e.create(t, "sập giữa chừng")
	msgID, m := e.msg.lastSent(t)
	_ = m
	d := e.draft(t, id)
	// The confirm claimed the draft and the comment reached ClickUp, then the process died.
	if _, err := e.store.ClaimDraftPost(context.Background(), p10WS, id, d.BodySHA256, msgID, e.now); err != nil {
		t.Fatal(err)
	}
	e.tool.posted[ToolPostComment+":"+string(rune('0'+id))] = 1
	e.now = e.now.Add(2 * time.Minute)
	e.store.mu.Lock()
	e.store.rows[id].UpdatedAt = e.now.Add(-10 * time.Minute)
	e.store.mu.Unlock()
	if err := e.d.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.tool.realPosts() != 1 {
		t.Fatalf("%d comments after replay", e.tool.realPosts())
	}
	if len(e.tool.factory) != 1 || e.tool.factory[0] != id {
		t.Fatalf("replay used key scope of drafts %v, want [%d]", e.tool.factory, id)
	}
	if got := e.draft(t, id); got.State != DraftPosted || got.Body != "" {
		t.Fatalf("draft %+v", got)
	}
	// A second sweep finds nothing to do.
	if err := e.d.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.tool.callsOf(ToolPostComment)) != 1 {
		t.Fatal("second sweep called the tool again")
	}
}

// TC-D8 (REQ-7): new_status runs after the comment; a status failure still counts as posted, with a warning.
func TestP10Draft_StatusChangeAfterComment(t *testing.T) {
	for _, fail := range []bool{false, true} {
		e := newP10Env(t)
		id, err := e.d.Create(context.Background(), DraftRequest{Chat: p10Chatref(), TaskID: p10Task, Text: "chuyển review",
			NewStatus: "in review", Source: DraftSourceAssistant})
		if err != nil {
			t.Fatal(err)
		}
		msgID, m := e.msg.lastSent(t)
		if !strings.Contains(m.msg.Text, "Đổi trạng thái: in review") {
			t.Fatalf("preview lacks the status: %q", m.msg.Text)
		}
		if fail {
			e.tool.script[ToolSetStatus] = tools.CallResult{Content: "error: invalid_status", IsError: true, Status: "error"}
		}
		e.press(CallbackApprove, e.approvalOf(t, m, "Đăng"), msgID)
		post, st := e.tool.callsOf(ToolPostComment), e.tool.callsOf(ToolSetStatus)
		if len(post) != 1 || len(st) != 1 || st[0].input["status"] != "in review" {
			t.Fatalf("calls post %d status %d", len(post), len(st))
		}
		if e.tool.calls[0].name != ToolPostComment {
			t.Fatal("status changed before the comment")
		}
		d := e.draft(t, id)
		ed := e.msg.lastEdit(t)
		if d.State != DraftPosted || (fail && (d.ReasonCode != "status_invalid_status" || !strings.Contains(ed.text, "⚠️"))) || (!fail && strings.Contains(ed.text, "⚠️")) {
			t.Fatalf("fail=%v draft %+v edit %q", fail, d, ed.text)
		}
	}
}

// TC-D8 (REQ-7): a status-only failure never posts the comment twice on retry.
func TestP10Draft_CommentFailureSkipsStatus(t *testing.T) {
	e := newP10Env(t)
	e.d.Create(context.Background(), DraftRequest{Chat: p10Chatref(), TaskID: p10Task, Text: "x", NewStatus: "done", Source: DraftSourceAssistant})
	msgID, m := e.msg.lastSent(t)
	e.tool.script[ToolPostComment] = tools.CallResult{Content: "error: server_error", IsError: true, Status: "error"}
	e.press(CallbackApprove, e.approvalOf(t, m, "Đăng"), msgID)
	if n := len(e.tool.callsOf(ToolSetStatus)); n != 0 {
		t.Fatalf("status changed although the comment failed (%d)", n)
	}
}

// TC-D9 (REQ-7, human): length limits, chunking and input validation.
func TestP10Draft_LengthChunksAndValidation(t *testing.T) {
	e := newP10Env(t)
	ok := strings.Repeat("ệ", 8000-utf8.RuneCountInString("🤖 "))
	id, err := e.d.Create(context.Background(), DraftRequest{Chat: p10Chatref(), TaskID: p10Task, Text: ok, Source: DraftSourceAssistant})
	if err != nil {
		t.Fatalf("max body: %v", err)
	}
	if got := utf8.RuneCountInString(e.draft(t, id).Body); got != 8000 {
		t.Fatalf("body runes %d", got)
	}
	e.msg.mu.Lock()
	n := len(e.msg.sent)
	var parts []string
	for _, s := range e.msg.sent {
		parts = append(parts, s.msg.Text)
	}
	e.msg.mu.Unlock()
	if n != 3 { // 8000 runes in 3000-rune parts: 3 messages
		t.Fatalf("%d messages for the long body", n)
	}
	for i, p := range parts {
		if !utf8.ValidString(p) {
			t.Fatalf("part %d is not valid UTF-8", i)
		}
		if utf8.RuneCountInString(p) > 4096 {
			t.Fatalf("part %d has %d runes, over the Telegram limit", i, utf8.RuneCountInString(p))
		}
	}
	if len(e.msg.sent[len(e.msg.sent)-1].msg.Buttons) != 1 {
		t.Fatal("buttons must be on the last part only")
	}
	bad := map[string]DraftRequest{
		"too long":          {Text: strings.Repeat("a", 8000)},
		"empty":             {Text: ""},
		"spaces":            {Text: " \n "},
		"six mentions":      {Text: "x", MentionUserIDs: []string{"1", "2", "3", "4", "5", "6"}},
		"duplicate mention": {Text: "x", MentionUserIDs: []string{"7", "7"}},
		"text mention id":   {Text: "x", MentionUserIDs: []string{"abc"}},
		"zero mention id":   {Text: "x", MentionUserIDs: []string{"0"}},
		"bad parent":        {Text: "x", ParentCommentID: "12; DROP"},
		"bad task":          {Text: "x", TaskID: "../x"},
		"multiline status":  {Text: "x", NewStatus: "a\nb"},
		"unknown source":    {Text: "x", Source: "web"},
		"no chat":           {Text: "x", Chat: ChatRef{}},
	}
	for name, r := range bad {
		t.Run(name, func(t *testing.T) {
			if r.Chat.ChatID == 0 && name != "no chat" {
				r.Chat = p10Chatref()
			}
			if r.TaskID == "" {
				r.TaskID = p10Task
			}
			if r.Source == "" {
				r.Source = DraftSourceAssistant
			}
			before := len(e.store.rows)
			if _, err := e.d.Create(context.Background(), r); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err %v", err)
			}
			if len(e.store.rows) != before {
				t.Fatal("an invalid request stored a draft")
			}
		})
	}
	// Five distinct mentions, a parent id and Vietnamese text are accepted and reach the tool.
	id5, err := e.d.Create(context.Background(), DraftRequest{Chat: p10Chatref(), TaskID: p10Task, Text: "Anh Nam xem giúp",
		ParentCommentID: "90210", MentionUserIDs: []string{"1", "2", "3", "4", "5"}, Source: DraftSourceAssistant})
	if err != nil {
		t.Fatal(err)
	}
	msgID, m := e.msg.lastSent(t)
	e.press(CallbackApprove, e.approvalOf(t, m, "Đăng"), msgID)
	calls := e.tool.callsOf(ToolPostComment)
	if len(calls) != 1 || calls[0].draft != id5 || calls[0].input["parent_comment_id"] != "90210" {
		t.Fatalf("calls %+v", calls)
	}
	if ids, _ := calls[0].input["mention_user_ids"].([]any); len(ids) != 5 {
		t.Fatalf("mentions %v", calls[0].input["mention_user_ids"])
	}
}

// TC-D9 (REQ-7, abuse): a secret and an @everyone in the text are neutralised before the preview and the post.
func TestP10Draft_SecretAndMentionAreCleaned(t *testing.T) {
	e := newP10Env(t)
	id := e.create(t, "token ghp_abcdefghijklmnopqrstuvwxyz0123456789 gửi @channel ngay")
	_, m := e.msg.lastSent(t)
	if strings.Contains(m.msg.Text, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("secret in preview: %q", m.msg.Text)
	}
	if strings.Contains(e.draft(t, id).Body, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatal("secret stored in the draft")
	}
	if strings.Contains(m.msg.Text, " @channel ") {
		t.Fatalf("live @channel in preview: %q", m.msg.Text)
	}
}

// TC-D9 (REQ-7): the preview send failing still returns the draft id and posts nothing.
func TestP10Draft_PreviewSendFailureKeepsDraftUnposted(t *testing.T) {
	e := newP10Env(t)
	e.msg.failAt = 1
	id, err := e.d.Create(context.Background(), DraftRequest{Chat: p10Chatref(), TaskID: p10Task, Text: "x", Source: DraftSourceAssistant})
	if err == nil || id == 0 {
		t.Fatalf("id %d err %v", id, err)
	}
	if len(e.tool.calls) != 0 || e.draft(t, id).State != DraftPending {
		t.Fatal("a failed preview must leave a pending, unposted draft")
	}
}

// TC-D10 (REQ-7): the audit input of a draft's post holds a hash and a rune count, never the text.
func TestP10Draft_AuditInputHoldsHashNotText(t *testing.T) {
	body := "🤖 mật khẩu là hunter2 và số điện thoại 0909"
	in, _ := json.Marshal(map[string]any{"task_id": p10Task, "text": body, "parent_comment_id": "42", "mention_user_ids": []string{"7"}})
	out := draftAuditInput(ToolPostComment, in)
	s := string(out)
	if strings.Contains(s, "hunter2") || strings.Contains(s, "0909") || strings.Contains(s, "mật khẩu") {
		t.Fatalf("audit input keeps text: %s", s)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	want := "sha256:" + draftHash(body) + " runes:" + itoa(utf8.RuneCountInString(body))
	if m["text"] != want || m["task_id"] != p10Task || m["parent_comment_id"] != "42" {
		t.Fatalf("audit %v want text %q", m, want)
	}
	if got := string(draftAuditInput("x", json.RawMessage(`not json`))); got != "{}" {
		t.Fatalf("undecodable input kept as %q", got)
	}
	var w map[string]any
	_ = json.Unmarshal(draftAuditInput("x", json.RawMessage(`{"text":123}`)), &w)
	if w["text"] != "withheld" {
		t.Fatalf("non-string text: %v", w)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// TC-D10 (REQ-7): the confirmed post and the dry run reach the tool with the draft id as owner and key scope.
func TestP10Draft_DraftKeyScopeAndOwner(t *testing.T) {
	if DraftKeyScope(12) != "draft:12" {
		t.Fatalf("scope %q", DraftKeyScope(12))
	}
	e := newP10Env(t)
	e.d.d.Policies = p10Policy{dryRun: true}
	e.create(t, "dry")
	msgID, m := e.msg.lastSent(t)
	e.press(CallbackApprove, e.approvalOf(t, m, "Đăng"), msgID)
	if len(e.tool.policies) != 1 || !e.tool.policies[0].DryRun || e.tool.factory[0] != 1 {
		t.Fatalf("policies %+v factory %v", e.tool.policies, e.tool.factory)
	}
}

// TC-D1 (REQ-7): Cancel (service API) ends a pending draft, and a posted draft cannot be cancelled.
func TestP10Draft_ServiceCancel(t *testing.T) {
	e := newP10Env(t)
	id := e.create(t, "api huỷ")
	if err := e.d.Cancel(context.Background(), p10WS, id); err != nil {
		t.Fatal(err)
	}
	if err := e.d.Cancel(context.Background(), p10WS, id); !errors.Is(err, ErrConflict) {
		t.Fatalf("second cancel: %v", err)
	}
	if o := e.outcomes(); len(o) != 1 || o[0].State != DraftCancelled {
		t.Fatalf("outcomes %+v", o)
	}
	if err := e.d.Cancel(context.Background(), "ws-other", id); !errors.Is(err, ErrConflict) && !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace cancel: %v", err)
	}
}
