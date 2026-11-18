package behavior

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/people"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// This file is the ClickUp draft-and-confirm flow (Phase 10 REQ-7). Every
// ClickUp comment the hub writes on the owner's behalf (assistant, /task and
// /plan results, the orchestrator) is first a draft: an exact preview in the
// task topic with "Đăng" / "Sửa" / "Huỷ". Only "Đăng" posts, once, through
// the tool executor (policy, audit, idempotency key scope draft:<id>). The
// draft body is stored only while it may still be posted and is never
// logged.

// ApprovalClickUpDraft holds the buttons of one ClickUp draft preview (or of
// its failure message): "Đăng" (approve), "Sửa" (free text), "Huỷ" (reject),
// and "Thử lại" (approve) after a failed post.
const ApprovalClickUpDraft ApprovalKind = "clickup_draft"

// ClickUp draft states.
const (
	DraftPending   = "pending"
	DraftPosting   = "posting" // a confirm claimed it; the post is running
	DraftPosted    = "posted"
	DraftCancelled = "cancelled"
	DraftExpired   = "expired"
	DraftFailed    = "failed" // the post failed; "Thử lại" may post it
)

// ClickUp draft sources.
const (
	DraftSourceAssistant       = "assistant"
	DraftSourceExecutionReport = "execution_report"
	DraftSourceOrchestrator    = "orchestrator"
)

// DefaultClickUpDraftExpiry is how long a draft waits for the owner.
const DefaultClickUpDraftExpiry = 24 * time.Hour

const (
	maxDraftRunes    = 8000
	maxDraftMentions = 5
	draftMarker      = "🤖 "
	// draftChunkRunes keeps each preview message under Telegram's limit.
	draftChunkRunes = 3000
	draftPostBudget = 2 // the comment and the optional status change
)

var (
	draftCommentIDRe = regexp.MustCompile(`^[0-9]{1,20}$`)
	draftPostedIDRe  = regexp.MustCompile(`comment_id=([0-9A-Za-z_-]{1,64})`)
	draftPlanKeyRe   = regexp.MustCompile(`^[A-Za-z0-9_.:-]{0,64}$`)
)

// ClickUpDraft is a stored draft. Body is empty once the draft ended.
type ClickUpDraft struct {
	ID               int64
	WorkspaceID      string
	IntegrationID    string
	ChatID           int64
	ThreadID         int64
	TaskID           string
	ParentCommentID  string
	MentionUserIDs   []string
	NewStatus        string
	Body             string
	BodySHA256       string
	State            string
	Source           string
	ExecutionID      int64
	PlanTaskKey      string
	PreviewMessageID int64
	PostedCommentID  string
	ReasonCode       string
	ExpiresAt        time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
	FinishedAt       *time.Time
}

// Chat is where the draft's preview lives.
func (d ClickUpDraft) Chat() ChatRef {
	return ChatRef{WorkspaceID: d.WorkspaceID, IntegrationID: d.IntegrationID, ChatID: d.ChatID, ThreadID: d.ThreadID}
}

// NewClickUpDraft is a draft to store.
type NewClickUpDraft struct {
	Chat            ChatRef
	TaskID          string
	ParentCommentID string
	MentionUserIDs  []string
	NewStatus       string
	Body            string
	BodySHA256      string
	Source          string
	ExecutionID     int64
	PlanTaskKey     string
	ExpiresAt       time.Time
}

// ClickUpDraftStore persists drafts. Conditional transitions return
// ErrConflict when the draft is not in a state that allows them.
type ClickUpDraftStore interface {
	CreateDraft(ctx context.Context, d NewClickUpDraft) (ClickUpDraft, error)
	GetDraft(ctx context.Context, workspaceID string, id int64) (ClickUpDraft, error)
	// SetDraftPreview binds the message carrying the current buttons.
	SetDraftPreview(ctx context.Context, workspaceID string, id, messageID int64) error
	// ReviseDraft replaces the body of a pending, unexpired draft whose body
	// hash is still oldSHA; the preview binding is cleared.
	ReviseDraft(ctx context.Context, workspaceID string, id int64, oldSHA, body, sha string, now time.Time) (ClickUpDraft, error)
	// ClaimDraftPost moves a pending or failed, unexpired draft with this
	// exact body hash and preview message to posting; only one claim wins.
	ClaimDraftPost(ctx context.Context, workspaceID string, id int64, sha string, previewMessageID int64, now time.Time) (ClickUpDraft, error)
	FinishDraftPosted(ctx context.Context, workspaceID string, id int64, commentID, reasonCode string) (ClickUpDraft, error)
	FailDraft(ctx context.Context, workspaceID string, id int64, reasonCode string) (ClickUpDraft, error)
	CancelDraft(ctx context.Context, workspaceID string, id int64) (ClickUpDraft, error)
	// ExpireDrafts expires up to limit overdue pending or failed drafts.
	ExpireDrafts(ctx context.Context, now time.Time, limit int) ([]ClickUpDraft, error)
	// ReclaimStalePosts returns up to limit drafts left in posting since
	// before (an interrupted confirm), touching them so one sweep takes each.
	ReclaimStalePosts(ctx context.Context, before time.Time, limit int) ([]ClickUpDraft, error)
}

// DraftRequest asks for one ClickUp draft and its preview.
type DraftRequest struct {
	// Chat is where the preview goes (normally the task topic).
	Chat            ChatRef
	TaskID          string
	Text            string // at most 8,000 runes
	ParentCommentID string // a numeric ClickUp comment id: a threaded reply
	MentionUserIDs  []string
	NewStatus       string // optional status change made by the same confirm
	Source          string // DraftSource*
	ExecutionID     int64  // optional workflow execution
	PlanTaskKey     string // optional plan task of that execution
}

// ClickUpDraftOutcome is a draft's terminal state, reported to listeners.
type ClickUpDraftOutcome struct {
	WorkspaceID     string
	DraftID         int64
	State           string // DraftPosted, DraftCancelled or DraftExpired
	Source          string
	ExecutionID     int64
	PlanTaskKey     string
	TaskID          string
	PostedCommentID string
	ReasonCode      string
}

// ClickUpDraftListener is told when a draft reaches a terminal state. It
// must return quickly; errors are its own.
type ClickUpDraftListener func(ctx context.Context, o ClickUpDraftOutcome)

// MessageButtonEditor is the optional part of a Messenger that replaces a
// message's text and inline keyboard in one edit.
type MessageButtonEditor interface {
	EditMessageWithButtons(ctx context.Context, to ChatRef, messageID int64, text string, buttons [][]Button) error
}

// DraftCallerFactory builds the tool caller of one draft's confirmed post:
// audit rows owned by the draft, idempotency keys scoped to draft:<id>.
type DraftCallerFactory func(workspaceID string, draftID int64, policy tenant.ToolPolicyConfig, clickUpTokenRef string) ToolCaller

// DraftExecutorFactory returns a DraftCallerFactory over the Phase 2
// executor; the enabled ClickUp write tools run without a repo grant
// (UnscopedWrites), nothing dangerous.
func DraftExecutorFactory(reg *tools.Registry, store tools.ToolRunStore, san tools.Sanitizer, base tools.Limits) DraftCallerFactory {
	return func(ws string, draftID int64, policy tenant.ToolPolicyConfig, ref string) ToolCaller {
		limits := base
		limits.MaxCalls = 4
		limits.MaxDangerous = 0
		e := tools.NewExecutor(reg, policy, limits, false, store, san, draftID, ws)
		e.ClickUpTokenRef = ref
		e.KeyScope = DraftKeyScope(draftID)
		e.UnscopedWrites = append([]string(nil), policy.Enabled...)
		return e
	}
}

// DraftKeyScope is the idempotency key scope of a draft's post.
func DraftKeyScope(draftID int64) string { return "draft:" + strconv.FormatInt(draftID, 10) }

// ClickUpDraftDeps are the flow's collaborators.
type ClickUpDraftDeps struct {
	Store     ClickUpDraftStore
	Approvals ApprovalStore
	Messenger Messenger
	Policies  PolicySource
	Callers   DraftCallerFactory
	// People names mentioned users in the preview; nil shows ids only.
	People people.Resolver
	// TaskURL returns the task link shown after posting; nil or "" uses
	// https://app.clickup.com/t/<task id>.
	TaskURL func(ctx context.Context, workspaceID, taskID string) string
	// Redactor cleans the draft text (control characters, secrets, @
	// mentions); nil uses a default redactor.
	Redactor *agent.Redactor
}

// ClickUpDraftSettings tune the flow.
type ClickUpDraftSettings struct {
	Expiry         time.Duration // default DefaultClickUpDraftExpiry
	SweepInterval  time.Duration // default 1m
	StalePost      time.Duration // a posting draft older than this is finished by the sweep; default 5m
	RequestTimeout time.Duration // one Telegram call; default 10s
}

// ClickUpDrafts is the draft-and-confirm flow. Its Run sweeps expired
// drafts and finishes interrupted posts.
type ClickUpDrafts struct {
	d   ClickUpDraftDeps
	s   ClickUpDraftSettings
	now func() time.Time

	mu        sync.Mutex
	listeners []ClickUpDraftListener
}

func NewClickUpDrafts(d ClickUpDraftDeps, s ClickUpDraftSettings) (*ClickUpDrafts, error) {
	if d.Store == nil || d.Approvals == nil || d.Messenger == nil || d.Policies == nil || d.Callers == nil {
		return nil, errors.New("clickup drafts: missing dependency")
	}
	if d.Redactor == nil {
		d.Redactor = agent.NewRedactor(nil)
	}
	if s.Expiry <= 0 {
		s.Expiry = DefaultClickUpDraftExpiry
	}
	if s.SweepInterval <= 0 {
		s.SweepInterval = time.Minute
	}
	if s.StalePost <= 0 {
		s.StalePost = 5 * time.Minute
	}
	if s.RequestTimeout <= 0 {
		s.RequestTimeout = 10 * time.Second
	}
	return &ClickUpDrafts{d: d, s: s, now: time.Now}, nil
}

// OnFinished adds a listener for terminal states (posted, cancelled,
// expired). Call it before the flow runs.
func (c *ClickUpDrafts) OnFinished(fn ClickUpDraftListener) {
	if fn == nil {
		return
	}
	c.mu.Lock()
	c.listeners = append(c.listeners, fn)
	c.mu.Unlock()
}

func (c *ClickUpDrafts) finished(ctx context.Context, d ClickUpDraft) {
	c.mu.Lock()
	ls := slices.Clone(c.listeners)
	c.mu.Unlock()
	o := ClickUpDraftOutcome{
		WorkspaceID: d.WorkspaceID, DraftID: d.ID, State: d.State, Source: d.Source, ExecutionID: d.ExecutionID,
		PlanTaskKey: d.PlanTaskKey, TaskID: d.TaskID, PostedCommentID: d.PostedCommentID, ReasonCode: d.ReasonCode,
	}
	for _, fn := range ls {
		fn(ctx, o)
	}
}

// Get returns a draft (its body only while it may still be posted).
func (c *ClickUpDrafts) Get(ctx context.Context, workspaceID string, id int64) (ClickUpDraft, error) {
	return c.d.Store.GetDraft(ctx, workspaceID, id)
}

func draftHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// draftBody cleans the owner's or the model's text and puts the bot marker
// first, so the hub's own comment never comes back as ClickUp activity
// (FromOwnBot). The result is exactly what is posted.
func (c *ClickUpDrafts) draftBody(text string) (string, error) {
	t := strings.TrimSpace(c.d.Redactor.CleanOutbound(strings.TrimSpace(text), true))
	t = strings.TrimSpace(strings.TrimPrefix(t, strings.TrimSpace(draftMarker)))
	if t == "" || !utf8.ValidString(t) {
		return "", ErrInvalid
	}
	body := draftMarker + t
	if utf8.RuneCountInString(body) > maxDraftRunes {
		return "", ErrInvalid
	}
	return body, nil
}

func validDraftStatus(s string) bool {
	return s == "" || (strings.TrimSpace(s) != "" && utf8.ValidString(s) && utf8.RuneCountInString(s) <= 100 &&
		!strings.ContainsAny(s, "\n\r\t"))
}

func validDraftRequest(r DraftRequest) bool {
	ch := r.Chat
	if ch.WorkspaceID == "" || ch.IntegrationID == "" || ch.ChatID == 0 || ch.ThreadID < 0 {
		return false
	}
	if !taskIDRe.MatchString(r.TaskID) || (r.ParentCommentID != "" && !draftCommentIDRe.MatchString(r.ParentCommentID)) {
		return false
	}
	if len(r.MentionUserIDs) > maxDraftMentions || !validDraftStatus(r.NewStatus) || !draftPlanKeyRe.MatchString(r.PlanTaskKey) {
		return false
	}
	seen := map[string]bool{}
	for _, id := range r.MentionUserIDs {
		if !draftCommentIDRe.MatchString(id) || strings.TrimLeft(id, "0") == "" || seen[id] {
			return false
		}
		seen[id] = true
	}
	switch r.Source {
	case DraftSourceAssistant, DraftSourceExecutionReport, DraftSourceOrchestrator:
	default:
		return false
	}
	return r.ExecutionID >= 0
}

// Create stores a draft and sends its preview to r.Chat. It never posts.
// The draft id is returned even when the preview could not be sent (the
// draft then expires unposted).
func (c *ClickUpDrafts) Create(ctx context.Context, r DraftRequest) (int64, error) {
	if !validDraftRequest(r) {
		return 0, ErrInvalid
	}
	body, err := c.draftBody(r.Text)
	if err != nil {
		return 0, err
	}
	d, err := c.d.Store.CreateDraft(ctx, NewClickUpDraft{
		Chat: r.Chat, TaskID: r.TaskID, ParentCommentID: r.ParentCommentID, MentionUserIDs: slices.Clone(r.MentionUserIDs),
		NewStatus: r.NewStatus, Body: body, BodySHA256: draftHash(body), Source: r.Source, ExecutionID: r.ExecutionID,
		PlanTaskKey: r.PlanTaskKey, ExpiresAt: c.now().Add(c.s.Expiry),
	})
	if err != nil {
		return 0, err
	}
	if err := c.sendPreview(ctx, d); err != nil {
		log.Printf("clickup draft %d: preview not sent (%s)", d.ID, sendCode(err))
		return d.ID, err
	}
	return d.ID, nil
}

// Cancel ends a pending or failed draft without posting (for callers such
// as an ended execution). ErrConflict when it already ended or is posting.
func (c *ClickUpDrafts) Cancel(ctx context.Context, workspaceID string, id int64) error {
	d, err := c.d.Store.CancelDraft(ctx, workspaceID, id)
	if err != nil {
		return err
	}
	c.editPreview(ctx, d, draftCancelledText(d), nil)
	c.finished(ctx, d)
	return nil
}

// --- approvals and messages ---

// draftPayload is the payload of a clickup_draft approval: the draft and
// the exact body and chat its buttons were shown for.
type draftPayload struct {
	DraftID int64  `json:"draft_id"`
	SHA     string `json:"sha"`
	ChatID  int64  `json:"chat_id"`
	Purpose string `json:"purpose"` // "preview" or "retry"
}

func (c *ClickUpDrafts) newApproval(ctx context.Context, d ClickUpDraft, purpose string) (Approval, error) {
	nonce, err := NewNonce()
	if err != nil {
		return Approval{}, err
	}
	payload, _ := json.Marshal(draftPayload{DraftID: d.ID, SHA: d.BodySHA256, ChatID: d.ChatID, Purpose: purpose})
	return c.d.Approvals.CreateApproval(ctx, d.WorkspaceID, NewApproval{
		Kind: ApprovalClickUpDraft, Action: purpose, Resource: "clickup_draft:" + strconv.FormatInt(d.ID, 10),
		Payload: payload, Nonce: nonce, ExpiresAt: d.ExpiresAt,
	})
}

func encodeButtons(nonce string, specs ...any) ([]Button, error) {
	var row []Button
	for i := 0; i+1 < len(specs); i += 2 {
		data, err := (CallbackData{Kind: specs[i+1].(CallbackKind), Nonce: nonce, Option: -1}).Encode()
		if err != nil {
			return nil, err
		}
		row = append(row, Button{Text: specs[i].(string), Data: data})
	}
	return row, nil
}

func (c *ClickUpDrafts) send(ctx context.Context, to ChatRef, m OutMessage) (int64, error) {
	sctx, cancel := context.WithTimeout(ctx, c.s.RequestTimeout)
	defer cancel()
	return c.d.Messenger.SendMessage(sctx, to, m)
}

// sendPreview sends the exact body (in parts when it is long) and the
// header with "Đăng" / "Sửa" / "Huỷ", then binds the buttons' message.
func (c *ClickUpDrafts) sendPreview(ctx context.Context, d ClickUpDraft) error {
	ap, err := c.newApproval(ctx, d, "preview")
	if err != nil {
		return err
	}
	row, err := encodeButtons(ap.Nonce, btnDraftPost, CallbackApprove, btnDraftEdit, CallbackFreeText, btnDraftCancel, CallbackReject)
	if err != nil {
		return err
	}
	chunks := chunkRunes(d.Body, draftChunkRunes)
	chat := d.Chat()
	for i, part := range chunks[:len(chunks)-1] {
		if _, err := c.send(ctx, chat, OutMessage{Text: fmt.Sprintf("D-%d (%d/%d)\n%s", d.ID, i+1, len(chunks), part)}); err != nil {
			return err
		}
	}
	text := c.previewText(ctx, d, chunks)
	id, err := c.send(ctx, chat, OutMessage{Text: text, Buttons: [][]Button{row}})
	if err != nil {
		return err
	}
	if err := c.d.Approvals.SetApprovalMessage(ctx, d.WorkspaceID, ap.ID, id); err != nil {
		return err
	}
	return c.d.Store.SetDraftPreview(ctx, d.WorkspaceID, d.ID, id)
}

// chunkRunes splits s into parts of at most n runes (at least one part).
func chunkRunes(s string, n int) []string {
	r := []rune(s)
	if len(r) <= n {
		return []string{s}
	}
	var out []string
	for len(r) > 0 {
		k := min(n, len(r))
		out = append(out, string(r[:k]))
		r = r[k:]
	}
	return out
}

const (
	btnDraftPost   = "Đăng"
	btnDraftEdit   = "Sửa"
	btnDraftCancel = "Huỷ"
	btnDraftRetry  = "Thử lại"
	draftRule      = "────────"
)

func (c *ClickUpDrafts) mentionLabel(ctx context.Context, ws, id string) string {
	if c.d.People != nil {
		if name, ok := c.d.People.DisplayName(ctx, ws, "clickup", id); ok && strings.TrimSpace(name) != "" {
			return people.CleanDisplayName(name) + " (" + id + ")"
		}
	}
	return id
}

func (c *ClickUpDrafts) previewText(ctx context.Context, d ClickUpDraft, chunks []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📝 Bản nháp bình luận ClickUp (D-%d) — chưa đăng\n", d.ID)
	b.WriteString("Task: " + d.TaskID + "\n")
	if d.ParentCommentID != "" {
		b.WriteString("Trả lời bình luận: " + d.ParentCommentID + "\n")
	}
	if len(d.MentionUserIDs) > 0 {
		labels := make([]string, 0, len(d.MentionUserIDs))
		for _, id := range d.MentionUserIDs {
			labels = append(labels, c.mentionLabel(ctx, d.WorkspaceID, id))
		}
		b.WriteString("Tag (ở cuối bình luận): " + strings.Join(labels, ", ") + "\n")
	}
	if d.NewStatus != "" {
		b.WriteString("Đổi trạng thái: " + d.NewStatus + "\n")
	}
	b.WriteString("Hết hạn: " + d.ExpiresAt.UTC().Format("15:04 02/01 UTC") + "\n")
	b.WriteString(draftRule + "\n")
	if len(chunks) > 1 {
		fmt.Fprintf(&b, "(%d/%d)\n", len(chunks), len(chunks))
	}
	b.WriteString(chunks[len(chunks)-1] + "\n")
	b.WriteString(draftRule + "\n")
	b.WriteString("Đăng: đăng đúng nội dung trên. Sửa: bấm Sửa rồi trả lời tin nhắn này bằng nội dung mới.")
	return b.String()
}

func (c *ClickUpDrafts) taskLink(ctx context.Context, d ClickUpDraft) string {
	if c.d.TaskURL != nil {
		if u := strings.TrimSpace(c.d.TaskURL(ctx, d.WorkspaceID, d.TaskID)); strings.HasPrefix(u, "https://") {
			return u
		}
	}
	// U3: ClickUp returns no comment link; the link goes to the task.
	return "https://app.clickup.com/t/" + d.TaskID
}

func draftCancelledText(d ClickUpDraft) string {
	return fmt.Sprintf("🚫 Đã huỷ bản nháp D-%d, không đăng lên ClickUp.", d.ID)
}

func draftExpiredText(d ClickUpDraft) string {
	return fmt.Sprintf("⌛ Bản nháp D-%d đã hết hạn, không đăng lên ClickUp.", d.ID)
}

// editPreview replaces the preview's text (and buttons; nil removes them).
// Best effort: a failed edit only removes the buttons.
func (c *ClickUpDrafts) editPreview(ctx context.Context, d ClickUpDraft, text string, buttons [][]Button) bool {
	if d.PreviewMessageID <= 0 {
		return false
	}
	ectx, cancel := context.WithTimeout(ctx, c.s.RequestTimeout)
	defer cancel()
	chat := d.Chat()
	if be, ok := c.d.Messenger.(MessageButtonEditor); ok {
		if err := be.EditMessageWithButtons(ectx, chat, d.PreviewMessageID, text, buttons); err == nil {
			return true
		}
	} else if len(buttons) == 0 {
		if ed, ok := c.d.Messenger.(MessageEditor); ok {
			if err := ed.EditMessageText(ectx, chat, d.PreviewMessageID, text); err == nil {
				return true
			}
		}
	}
	if len(buttons) == 0 {
		_ = c.d.Messenger.RemoveButtons(ectx, chat, d.PreviewMessageID)
	}
	return false
}

// --- button presses and replies (called by the inbound worker) ---

// DraftPress is a press on a clickup_draft approval's button.
type DraftPress struct {
	WorkspaceID   string
	IntegrationID string
	ChatID        int64
	MessageID     int64
	Kind          CallbackKind
	Approval      Approval // open and unexpired when read
	Actor         string
}

// DraftReply is the owner's reply to a preview in edit mode.
type DraftReply struct {
	WorkspaceID   string
	IntegrationID string
	ChatID        int64
	Approval      Approval
	Text          string
	Actor         string
}

// ClickUpDraftHandler is what the inbound worker calls.
type ClickUpDraftHandler interface {
	// HandleDraftPress returns the toast, an update code and whether to
	// remove the pressed message's buttons.
	HandleDraftPress(ctx context.Context, p DraftPress) (toast, code string, remove bool)
	HandleDraftReply(ctx context.Context, r DraftReply) (string, error)
}

var _ ClickUpDraftHandler = (*ClickUpDrafts)(nil)

const (
	msgDraftStale    = "Bản nháp này đã xử lý, đã sửa hoặc đã hết hạn."
	msgDraftPosted   = "Đã đăng lên ClickUp."
	msgDraftFailed   = "Không đăng được lên ClickUp."
	msgDraftEditHint = "Trả lời tin nhắn xem trước bằng nội dung mới."
	msgDraftRevised  = "Đã cập nhật bản nháp. Xem bản mới và bấm Đăng nếu đúng."
)

// boundDraft checks that a press or reply belongs to the draft and the chat
// its approval was shown in, for the exact body the approval names.
func (c *ClickUpDrafts) boundDraft(ctx context.Context, ws, integration string, chat int64, a Approval) (ClickUpDraft, draftPayload, error) {
	var p draftPayload
	if a.Kind != ApprovalClickUpDraft || a.WorkspaceID != ws || json.Unmarshal(a.Payload, &p) != nil || p.DraftID <= 0 || p.ChatID != chat {
		return ClickUpDraft{}, p, ErrDecided
	}
	d, err := c.d.Store.GetDraft(ctx, ws, p.DraftID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ClickUpDraft{}, p, ErrDecided
		}
		return ClickUpDraft{}, p, err
	}
	if d.WorkspaceID != ws || d.IntegrationID != integration || d.ChatID != chat || d.BodySHA256 != p.SHA {
		return ClickUpDraft{}, p, ErrDecided
	}
	return d, p, nil
}

// closeApproval closes a stale approval so its buttons never act again.
func (c *ClickUpDrafts) closeApproval(ctx context.Context, ws, nonce, actor string) {
	if _, err := c.d.Approvals.Decide(ctx, ws, nonce, DecisionExpired, "", actor, c.now()); err != nil &&
		!errors.Is(err, ErrDecided) && !errors.Is(err, ErrExpired) && !errors.Is(err, ErrNotFound) {
		log.Print("clickup draft: stale buttons not closed")
	}
}

func (c *ClickUpDrafts) HandleDraftPress(ctx context.Context, p DraftPress) (string, string, bool) {
	a := p.Approval
	d, pl, err := c.boundDraft(ctx, p.WorkspaceID, p.IntegrationID, p.ChatID, a)
	if err != nil {
		if errors.Is(err, ErrDecided) {
			c.closeApproval(ctx, p.WorkspaceID, a.Nonce, p.Actor)
			return msgDraftStale, "stale", true
		}
		return msgFailed, "storage", false
	}
	if p.MessageID <= 0 || d.PreviewMessageID != p.MessageID {
		c.closeApproval(ctx, p.WorkspaceID, a.Nonce, p.Actor)
		return msgDraftStale, "stale", true
	}
	switch p.Kind {
	case CallbackApprove:
		if pl.Purpose == "retry" && d.State != DraftFailed || pl.Purpose == "preview" && d.State != DraftPending {
			c.closeApproval(ctx, p.WorkspaceID, a.Nonce, p.Actor)
			return msgDraftStale, "stale", true
		}
		if _, err := c.d.Approvals.Decide(ctx, p.WorkspaceID, a.Nonce, DecisionApproved, "", p.Actor, c.now()); err != nil {
			return msgDraftStale, "stale", true
		}
		return c.confirm(ctx, d, p.MessageID)
	case CallbackReject:
		if d.State != DraftPending && d.State != DraftFailed {
			c.closeApproval(ctx, p.WorkspaceID, a.Nonce, p.Actor)
			return msgDraftStale, "stale", true
		}
		if _, err := c.d.Approvals.Decide(ctx, p.WorkspaceID, a.Nonce, DecisionRejected, "", p.Actor, c.now()); err != nil {
			return msgDraftStale, "stale", true
		}
		cd, err := c.d.Store.CancelDraft(ctx, p.WorkspaceID, d.ID)
		if err != nil {
			if errors.Is(err, ErrConflict) {
				return msgDraftStale, "stale", true
			}
			return msgFailed, "storage", false
		}
		log.Printf("clickup draft %d: cancelled by owner", d.ID)
		c.editPreview(ctx, cd, draftCancelledText(cd), nil)
		c.finished(ctx, cd)
		return "Đã huỷ.", "", true
	case CallbackFreeText:
		if pl.Purpose != "preview" || d.State != DraftPending {
			return msgDraftStale, "stale", false
		}
		if err := c.d.Approvals.SetReplyMessage(ctx, p.WorkspaceID, a.ID, p.MessageID); err != nil {
			if errors.Is(err, ErrNotFound) {
				return msgDraftStale, "stale", true
			}
			return msgFailed, "storage", false
		}
		return msgDraftEditHint, "", false
	}
	return msgNotAvailable, "not_available", false
}

// confirm claims the draft for this exact body and preview, posts it and
// shows the outcome on the preview.
func (c *ClickUpDrafts) confirm(ctx context.Context, d ClickUpDraft, previewMessageID int64) (string, string, bool) {
	claimed, err := c.d.Store.ClaimDraftPost(ctx, d.WorkspaceID, d.ID, d.BodySHA256, previewMessageID, c.now())
	if err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			return msgDraftStale, "stale", true
		}
		return msgFailed, "storage", false
	}
	if c.post(ctx, claimed) {
		return msgDraftPosted, "", false
	}
	return msgDraftFailed, "clickup_failed", false
}

// post runs the claimed draft's writes through the executor: the comment,
// then the optional status change. Run again for the same draft, the
// executor replays the comment instead of posting it twice. It reports
// whether the comment was posted.
func (c *ClickUpDrafts) post(ctx context.Context, d ClickUpDraft) bool {
	cfg, err := c.d.Policies.RunPolicy(ctx, d.WorkspaceID, "")
	if err != nil {
		return c.fail(ctx, d, CodePolicyUnavailable, true)
	}
	enabled := []string{ToolPostComment}
	if d.NewStatus != "" {
		enabled = append(enabled, ToolSetStatus)
	}
	policy := tenant.ToolPolicyConfig{Enabled: enabled, DryRun: cfg.Tools.DryRun, MaxWritesPerJob: draftPostBudget}
	caller := c.d.Callers(d.WorkspaceID, d.ID, policy, cfg.ClickUpTokenRef)
	input := map[string]any{"task_id": d.TaskID, "text": d.Body}
	if d.ParentCommentID != "" {
		input["parent_comment_id"] = d.ParentCommentID
	}
	if len(d.MentionUserIDs) > 0 {
		input["mention_user_ids"] = d.MentionUserIDs
	}
	raw, _ := json.Marshal(input)
	res := caller.Call(ctx, 1, ToolPostComment, raw)
	switch {
	case res.Status == tools.StatusDryRun:
		return c.fail(ctx, d, OutcomePolicyDryRun, false)
	case res.Status == tools.StatusUnknownOutcome:
		// It may have been posted: never offer a retry that could post twice.
		return c.fail(ctx, d, OutcomeUnknown, false)
	case res.IsError:
		return c.fail(ctx, d, resultCode(res), res.Status != tools.StatusDenied)
	}
	commentID := ""
	if m := draftPostedIDRe.FindStringSubmatch(res.Content); m != nil {
		commentID = m[1]
	}
	reason := ""
	if d.NewStatus != "" {
		sraw, _ := json.Marshal(map[string]any{"task_id": d.TaskID, "status": d.NewStatus})
		sres := caller.Call(ctx, 2, ToolSetStatus, sraw)
		if sres.IsError || sres.Status != tools.StatusOK {
			reason = capRunes("status_"+firstNonEmpty(resultCodeOr(sres), "failed"), 64)
		}
	}
	done, err := c.d.Store.FinishDraftPosted(ctx, d.WorkspaceID, d.ID, commentID, reason)
	if err != nil {
		// The comment is posted; the sweep finishes the row (the executor
		// replays the post, it never posts again).
		log.Printf("clickup draft %d: posted, state not saved", d.ID)
		return true
	}
	log.Printf("clickup draft %d: posted on task %s", d.ID, d.TaskID)
	text := fmt.Sprintf("✅ Đã đăng lên ClickUp (D-%d)\nTask: %s", d.ID, c.taskLink(ctx, done))
	if commentID != "" {
		text += "\nBình luận: " + commentID
	}
	if reason != "" {
		text += "\n⚠️ Không đổi được trạng thái: " + strings.TrimPrefix(reason, "status_")
	}
	c.editPreview(ctx, done, text, nil)
	c.finished(ctx, done)
	return true
}

func resultCodeOr(res tools.CallResult) string {
	if res.Status == tools.StatusDryRun {
		return OutcomePolicyDryRun
	}
	if res.Status == tools.StatusUnknownOutcome {
		return OutcomeUnknown
	}
	return resultCode(res)
}

// fail records a failed post and shows "❌ Không đăng được: <code>", with
// "Thử lại" / "Huỷ" when a retry cannot post twice.
func (c *ClickUpDrafts) fail(ctx context.Context, d ClickUpDraft, code string, retry bool) bool {
	if !codeRe.MatchString(code) {
		code = "tool_error"
	}
	failed, err := c.d.Store.FailDraft(ctx, d.WorkspaceID, d.ID, code)
	if err != nil {
		log.Printf("clickup draft %d: failure not saved", d.ID)
		return false
	}
	log.Printf("clickup draft %d: post failed (%s)", d.ID, code)
	text := fmt.Sprintf("❌ Không đăng được: %s (D-%d)", code, d.ID)
	if !retry {
		text += "\nKhông thử lại tự động để tránh đăng hai lần."
		c.editPreview(ctx, failed, text, nil)
		return false
	}
	ap, err := c.newApproval(ctx, failed, "retry")
	if err != nil {
		c.editPreview(ctx, failed, text, nil)
		return false
	}
	row, err := encodeButtons(ap.Nonce, btnDraftRetry, CallbackApprove, btnDraftCancel, CallbackReject)
	if err != nil {
		c.editPreview(ctx, failed, text, nil)
		return false
	}
	msgID := failed.PreviewMessageID
	if !c.editPreview(ctx, failed, text, [][]Button{row}) {
		// No in-place edit: the failure is a new message that carries the
		// buttons, and the draft binds to it.
		id, serr := c.send(ctx, failed.Chat(), OutMessage{Text: text, Buttons: [][]Button{row}})
		if serr != nil {
			return false
		}
		msgID = id
		if err := c.d.Store.SetDraftPreview(ctx, failed.WorkspaceID, failed.ID, id); err != nil {
			return false
		}
	}
	if msgID > 0 {
		if err := c.d.Approvals.SetApprovalMessage(ctx, failed.WorkspaceID, ap.ID, msgID); err != nil {
			log.Printf("clickup draft %d: retry buttons not bound", d.ID)
		}
	}
	return false
}

// HandleDraftReply replaces a pending draft's text with the owner's reply
// to its preview ("Sửa"); the old buttons stop working and a new preview is
// sent.
func (c *ClickUpDrafts) HandleDraftReply(ctx context.Context, r DraftReply) (string, error) {
	d, pl, err := c.boundDraft(ctx, r.WorkspaceID, r.IntegrationID, r.ChatID, r.Approval)
	if err != nil {
		return "", err
	}
	if pl.Purpose != "preview" || d.State != DraftPending {
		return "", ErrDecided
	}
	body, err := c.draftBody(r.Text)
	if err != nil {
		return "Nội dung mới trống hoặc quá dài (tối đa 8000 ký tự).", nil
	}
	if _, err := c.d.Approvals.Decide(ctx, r.WorkspaceID, r.Approval.Nonce, DecisionAnswered, "", r.Actor, c.now()); err != nil {
		return "", err
	}
	old := d
	nd, err := c.d.Store.ReviseDraft(ctx, r.WorkspaceID, d.ID, d.BodySHA256, body, draftHash(body), c.now())
	if err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			return "", ErrDecided
		}
		return "", err
	}
	c.editPreview(ctx, old, fmt.Sprintf("✏️ Bản nháp D-%d đã được sửa; xem bản mới bên dưới.", d.ID), nil)
	if err := c.sendPreview(ctx, nd); err != nil {
		log.Printf("clickup draft %d: new preview not sent (%s)", d.ID, sendCode(err))
		return "", err
	}
	return msgDraftRevised, nil
}

// --- sweep ---

// Run expires overdue drafts and finishes interrupted posts until ctx ends.
func (c *ClickUpDrafts) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		if err := c.Sweep(ctx); err != nil && ctx.Err() == nil {
			log.Print("clickup drafts: sweep failed; retried next period")
		}
		t := time.NewTimer(c.s.SweepInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
	return nil
}

// Sweep expires overdue pending and failed drafts (their bodies are
// cleared, nothing is posted) and finishes posts left in posting by a crash
// with the same idempotency key.
func (c *ClickUpDrafts) Sweep(ctx context.Context) error {
	const batch = 50
	for {
		list, err := c.d.Store.ExpireDrafts(ctx, c.now(), batch)
		if err != nil {
			return err
		}
		for _, d := range list {
			log.Printf("clickup draft %d: expired", d.ID)
			c.editPreview(ctx, d, draftExpiredText(d), nil)
			c.finished(ctx, d)
		}
		if len(list) < batch {
			break
		}
	}
	stale, err := c.d.Store.ReclaimStalePosts(ctx, c.now().Add(-c.s.StalePost), 20)
	if err != nil {
		return err
	}
	for _, d := range stale {
		log.Printf("clickup draft %d: finishing an interrupted post", d.ID)
		c.post(ctx, d)
	}
	return nil
}
