package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/google/uuid"
)

// This file builds the trace of one run (REQ-18) for /trace and the CLI,
// and the recent-run listing. A trace holds identifiers, codes, counts and
// the items as stored. Item text, proposals, non-trigger quotes and answers
// are the owner's own data and are shown only to him (his private chat or
// the CLI); every text field still passes through secret redaction. Quotes
// from triggering message/comment text are omitted. The context the run read
// is never part of a trace.

// MaxRunListLimit caps the run listing.
const MaxRunListLimit = 200

// MaxTraceText is the longest /trace reply, in UTF-16 code units (the
// Telegram limit is 4096).
const MaxTraceText = 4000

const traceMore = "…(xem CLI để đầy đủ)"

// ToolCallRecord is one tool audit row of a run, without inputs or result
// text.
type ToolCallRecord struct {
	ID         int64     `json:"id"`
	Step       int       `json:"step"`
	Tool       string    `json:"tool"`
	Tier       string    `json:"tier"`
	Status     string    `json:"status"`
	ErrorCode  string    `json:"error_code,omitempty"`
	DurationMS int64     `json:"duration_ms"`
	CreatedAt  time.Time `json:"created_at"`
}

// RunSummary is one line of the recent-run listing.
type RunSummary struct {
	ID            int64     `json:"id"`
	Behavior      string    `json:"behavior"`
	Version       int       `json:"version"`
	TriggerKind   string    `json:"trigger"`
	Subject       string    `json:"subject"`
	Status        RunStatus `json:"status"`
	ErrorCode     string    `json:"error_code,omitempty"`
	CorrelationID string    `json:"correlation_id"`
	AutoItems     int       `json:"auto"`
	NotifyItems   int       `json:"notify"`
	AskItems      int       `json:"ask"`
	// PRs are the pull requests of a chat-triggered run ("owner/repo#n"),
	// from its trusted trigger data.
	PRs []string `json:"prs,omitempty"`
	// RunActions are the run's run-level action outcomes (identifiers and
	// codes only).
	RunActions []RunActionOutcome `json:"run_actions,omitempty"`
	CreatedAt  time.Time          `json:"created_at"`
}

// RunPRRefs returns the pull requests of a run's stored trigger data as
// "owner/repo#n"; nil when there are none or the data is unreadable.
func RunPRRefs(trigger json.RawMessage) []string {
	td, err := DecodeTriggerData(trigger)
	if err != nil || len(td.PRs) == 0 {
		return nil
	}
	out := make([]string, 0, len(td.PRs))
	for _, p := range td.PRs {
		if tenant.ValidRepo(p.Repo) && p.Number > 0 {
			out = append(out, p.String())
		}
	}
	return out
}

// TraceStore is what a trace reads (storage.BehaviorStore).
type TraceStore interface {
	GetRun(ctx context.Context, workspaceID string, runID int64) (Run, error)
	LatestRunBySubject(ctx context.Context, workspaceID, subject string) (Run, error)
	LatestRunByCorrelation(ctx context.Context, workspaceID, correlationID string) (Run, error)
	ListItems(ctx context.Context, workspaceID string, runID int64) ([]Item, error)
	ListApprovals(ctx context.Context, workspaceID string, runID int64) ([]Approval, error)
	ListRunToolCalls(ctx context.Context, workspaceID string, runID int64) ([]ToolCallRecord, error)
}

// RunLister lists recent runs.
type RunLister interface {
	ListRecentRuns(ctx context.Context, workspaceID, behaviorName string, limit int) ([]RunSummary, error)
}

// Trace is one run and its items.
type Trace struct {
	Run   TraceRun    `json:"run"`
	Items []TraceItem `json:"items"`
	// RunActions are the outcomes of the run's run-level actions
	// (actions.run), identifiers and codes only.
	RunActions []RunActionOutcome `json:"run_actions,omitempty"`
	// Verdict is the run's model verdict (behaviors with a verdict block):
	// label, confidence, the model's summary (redacted) and the injection
	// flags, or the code of why there is none.
	Verdict   *RunVerdict      `json:"verdict,omitempty"`
	ToolCalls []ToolCallRecord `json:"tool_calls"`
}

// TraceRun is the run part of a trace.
type TraceRun struct {
	ID          int64           `json:"id"`
	Behavior    string          `json:"behavior"`
	Version     int             `json:"version"`
	TriggerKind string          `json:"trigger"`
	Trigger     json.RawMessage `json:"trigger_data,omitempty"`
	Subject     string          `json:"subject"`
	Revision    string          `json:"revision"`
	// PRs are the pull requests of a chat-triggered run ("owner/repo#n").
	PRs           []string   `json:"prs,omitempty"`
	Status        RunStatus  `json:"status"`
	ErrorCode     string     `json:"error_code,omitempty"`
	CorrelationID string     `json:"correlation_id"`
	Attempts      int        `json:"attempts"`
	Usage         TraceUsage `json:"usage"`
	RunAt         time.Time  `json:"run_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	UndoneAt      *time.Time `json:"undone_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// TraceUsage is a run's model usage.
type TraceUsage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
	Steps            int `json:"steps"`
}

// TraceItem is one item of a trace.
type TraceItem struct {
	Key           string          `json:"key"`
	Label         string          `json:"label,omitempty"`
	Class         Class           `json:"class"`
	Rule          string          `json:"rule"`
	Confidence    float64         `json:"confidence"`
	Topic         string          `json:"topic,omitempty"`
	Informational bool            `json:"informational,omitempty"`
	Status        ItemStatus      `json:"status"`
	Rating        Rating          `json:"rating,omitempty"`
	RatingReason  string          `json:"rating_reason,omitempty"`
	Flags         []string        `json:"flags,omitempty"`
	Text          string          `json:"text"`
	Proposal      string          `json:"proposal,omitempty"`
	Citations     []Citation      `json:"citations"`
	Actions       []TraceAction   `json:"actions"`
	Decisions     []TraceDecision `json:"decisions,omitempty"`
}

// TraceAction is the result of one action of an item.
type TraceAction struct {
	Index  int    `json:"i"`
	Type   string `json:"type"`
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
	Block  string `json:"block,omitempty"`
}

// TraceDecision is one approval of an item and its decision.
type TraceDecision struct {
	Kind      ApprovalKind `json:"kind"`
	Decision  Decision     `json:"decision,omitempty"` // empty while open
	Answer    string       `json:"answer,omitempty"`
	Actor     string       `json:"actor,omitempty"`
	DecidedAt *time.Time   `json:"decided_at,omitempty"`
	ExpiresAt time.Time    `json:"expires_at"`
}

// redactText removes secret-shaped values from text shown in a trace.
func redactText(s string) string {
	s = secretProbe.RedactSecrets(s)
	return clickUpTokenRe.ReplaceAllString(s, "[REDACTED]")
}

// ResolveTraceRef finds the run a trace reference names: a run ID ("12" or
// "#12"), a correlation ID, a chat subject ("clickup:chat:<message id>",
// its latest run), or a ClickUp task ID or URL (its latest run). A number
// that is no run ID is tried as a task ID.
func ResolveTraceRef(ctx context.Context, store TraceStore, workspaceID, ref string) (Run, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Run{}, ErrInvalid
	}
	digits := strings.TrimPrefix(ref, "#")
	if id, err := strconv.ParseInt(digits, 10, 64); err == nil && id > 0 && digits[0] != '+' {
		run, err := store.GetRun(ctx, workspaceID, id)
		if err == nil || !errors.Is(err, ErrNotFound) || strings.HasPrefix(ref, "#") {
			return run, err
		}
	}
	if _, err := uuid.Parse(ref); err == nil && len(ref) == 36 {
		return store.LatestRunByCorrelation(ctx, workspaceID, strings.ToLower(ref))
	}
	if msgID, ok := strings.CutPrefix(ref, SubjectChatPrefix); ok {
		if !chatIDRe.MatchString(msgID) {
			return Run{}, ErrInvalid
		}
		return store.LatestRunBySubject(ctx, workspaceID, ChatSubject(msgID))
	}
	if taskID, ok := ParseTaskRef(ref); ok {
		return store.LatestRunBySubject(ctx, workspaceID, TaskSubject(taskID))
	}
	return Run{}, ErrInvalid
}

// BuildTrace reads a run's trace. With itemKey set, only that item is
// returned (ErrNotFound when the run has no such item).
func BuildTrace(ctx context.Context, store TraceStore, workspaceID, ref, itemKey string) (Trace, error) {
	run, err := ResolveTraceRef(ctx, store, workspaceID, ref)
	if err != nil {
		return Trace{}, err
	}
	items, err := store.ListItems(ctx, workspaceID, run.ID)
	if err != nil {
		return Trace{}, err
	}
	approvals, err := store.ListApprovals(ctx, workspaceID, run.ID)
	if err != nil {
		return Trace{}, err
	}
	calls, err := store.ListRunToolCalls(ctx, workspaceID, run.ID)
	if err != nil {
		return Trace{}, err
	}
	res := DecodeRunActions(run.RunActions)
	t := Trace{Run: traceRun(run), Items: []TraceItem{}, RunActions: res.Actions, ToolCalls: calls}
	if v := res.Verdict; v != nil {
		tv := *v
		tv.Summary = redactText(tv.Summary)
		t.Verdict = &tv
	}
	if t.ToolCalls == nil {
		t.ToolCalls = []ToolCallRecord{}
	}
	byItem := map[int64][]Approval{}
	for _, a := range approvals {
		if a.ItemID != 0 {
			byItem[a.ItemID] = append(byItem[a.ItemID], a)
		}
	}
	for _, it := range items {
		if itemKey != "" && it.Key != itemKey {
			continue
		}
		t.Items = append(t.Items, traceItem(it, byItem[it.ID]))
	}
	if itemKey != "" && len(t.Items) == 0 {
		return t, ErrNotFound
	}
	return t, nil
}

func traceRun(r Run) TraceRun {
	tr := TraceRun{
		ID: r.ID, Behavior: r.BehaviorName, Version: r.Version, TriggerKind: r.TriggerKind,
		Subject: redactText(r.Subject), Revision: redactText(r.Revision), Status: r.Status, ErrorCode: r.ErrorCode,
		CorrelationID: r.CorrelationID, Attempts: r.Attempts,
		Usage: TraceUsage{
			InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens,
			CacheReadTokens: r.Usage.CacheReadTokens, CacheWriteTokens: r.Usage.CacheWriteTokens, Steps: r.Usage.Steps,
		},
		RunAt: r.RunAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, UndoneAt: r.UndoneAt, CreatedAt: r.CreatedAt,
		PRs: RunPRRefs(r.Trigger),
	}
	// The trigger holds trusted identifiers only; it is kept when it is
	// still valid JSON after redaction.
	if len(r.Trigger) > 0 {
		if red := redactText(string(r.Trigger)); json.Valid([]byte(red)) {
			tr.Trigger = json.RawMessage(red)
		}
	}
	return tr
}

func traceItem(it Item, approvals []Approval) TraceItem {
	ti := TraceItem{
		Key: redactText(it.Key), Label: redactText(it.Label), Class: it.Class, Rule: it.Rule, Confidence: it.Confidence,
		Topic: redactText(it.Topic), Informational: it.Informational, Status: it.Status, Rating: it.Rating,
		RatingReason: redactText(it.RatingReason), Flags: it.Flags, Text: redactText(it.Text), Proposal: redactText(it.Proposal),
		Citations: make([]Citation, 0, len(it.Citations)), Actions: []TraceAction{},
	}
	for _, c := range it.Citations {
		locator := redactText(c.Locator)
		quote := redactText(c.Quote)
		if triggerTextCitation(locator) {
			quote = ""
		}
		ti.Citations = append(ti.Citations, Citation{Locator: locator, Quote: quote})
	}
	for _, o := range decodeResult(it.ActionResult).Actions {
		ti.Actions = append(ti.Actions, TraceAction{Index: o.Index, Type: o.Type, Status: o.Status, Code: o.Code, Block: o.Block})
	}
	for _, a := range approvals {
		d := TraceDecision{Kind: a.Kind, Decision: a.Decision, Actor: a.DecidedBy, DecidedAt: a.DecidedAt, ExpiresAt: a.ExpiresAt}
		if a.Decision == DecisionAnswered || a.Kind == ApprovalFeedbackReason {
			d.Answer = redactText(a.Answer)
		}
		ti.Decisions = append(ti.Decisions, d)
	}
	return ti
}

// triggerTextCitation reports whether a locator names the untrusted text of
// the message or comment that triggered a run. Its locator remains useful in
// traces, but its quote must not repeat that triggering text.
func triggerTextCitation(locator string) bool {
	base, ok := strings.CutSuffix(locator, "#text")
	if !ok {
		return false
	}
	if id, ok := strings.CutPrefix(base, "clickup:chat/"); ok {
		return chatIDRe.MatchString(id)
	}
	if id, ok := strings.CutPrefix(base, "clickup:comment/"); ok {
		return commentIDRe.MatchString(id)
	}
	return false
}

// --- text rendering ---

// traceText renders a trace for Telegram in short Vietnamese.
type traceText struct {
	loc *time.Location
	b   strings.Builder
}

func (w *traceText) line(format string, args ...any) {
	fmt.Fprintf(&w.b, format, args...)
	w.b.WriteByte('\n')
}

func (w *traceText) time(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.In(w.loc).Format("2006-01-02 15:04:05")
}

func (w *traceText) timePtr(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return w.time(*t)
}

func ratingMark(r Rating) string {
	switch r {
	case RatingUp:
		return "👍"
	case RatingDown:
		return "👎"
	}
	return ""
}

func actionCodes(as []TraceAction) string {
	if len(as) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(as))
	for _, a := range as {
		p := a.Type + ":" + a.Status
		if a.Code != "" && a.Code != a.Status {
			p += "/" + a.Code
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ", ")
}

func (w *traceText) runHeader(r TraceRun) {
	w.line("Run #%d · %s v%d", r.ID, r.Behavior, r.Version)
	w.line("Trigger: %s", r.TriggerKind)
	w.line("Đối tượng: %s", r.Subject)
	w.line("Revision: %s", r.Revision)
	if r.TriggerKind == TriggerChatMessage {
		w.chatTrigger(r)
	}
	if r.ErrorCode != "" {
		w.line("Trạng thái: %s (%s)", r.Status, r.ErrorCode)
	} else {
		w.line("Trạng thái: %s", r.Status)
	}
	w.line("Bắt đầu: %s · Xong: %s", w.timePtr(r.StartedAt), w.timePtr(r.FinishedAt))
	if r.UndoneAt != nil {
		w.line("Đã hoàn tác: %s", w.time(*r.UndoneAt))
	}
	w.line("Corr: %s", r.CorrelationID)
	u := r.Usage
	w.line("Token: vào %d · ra %d · cache %d/%d · %d bước", u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.Steps)
}

// RenderTraceText renders a run with one line per item, or, when item is
// true, the trace's items in full, within MaxTraceText (for Telegram).
func RenderTraceText(t Trace, loc *time.Location, item bool) string {
	return capTraceText(renderTrace(t, loc, item, false))
}

// RenderTraceFull renders the same text without a length cap and with the
// run's tool calls (for the CLI).
func RenderTraceFull(t Trace, loc *time.Location, item bool) string {
	return strings.TrimRight(renderTrace(t, loc, item, true), "\n") + "\n"
}

func renderTrace(t Trace, loc *time.Location, item, calls bool) string {
	if loc == nil {
		loc = time.UTC
	}
	w := &traceText{loc: loc}
	w.runHeader(t.Run)
	w.verdict(t.Verdict)
	if !item {
		w.runActions(t.RunActions)
		w.line("Items (%d):", len(t.Items))
		for _, it := range t.Items {
			label := it.Label
			if label == "" {
				label = "-"
			}
			mark := ""
			if m := ratingMark(it.Rating); m != "" {
				mark = " · " + m
			}
			w.line("• %s · %s · %s · %s · %.2f · %s%s · %s", it.Key, clip(label, 60), it.Class, it.Rule, it.Confidence, it.Status, mark, actionCodes(it.Actions))
		}
		if calls {
			w.toolCalls(t.ToolCalls)
		}
		return w.b.String()
	}
	for _, it := range t.Items {
		w.line("")
		w.line("Item %s", it.Key)
		if it.Label != "" {
			w.line("Nhãn: %s", it.Label)
		}
		topic := it.Topic
		if topic == "" {
			topic = "-"
		}
		w.line("Class: %s · rule: %s · độ tin cậy %.2f · topic: %s", it.Class, it.Rule, it.Confidence, topic)
		status := "Trạng thái: " + string(it.Status)
		if it.Rating != "" {
			status += " · đánh giá: " + ratingMark(it.Rating)
			if it.RatingReason != "" {
				status += " (" + it.RatingReason + ")"
			}
		}
		w.line("%s", status)
		if len(it.Flags) > 0 {
			w.line("Cờ: %s", strings.Join(it.Flags, ", "))
		}
		w.line("Nội dung:\n%s", it.Text)
		if it.Proposal != "" {
			w.line("Đề xuất:\n%s", it.Proposal)
		}
		if len(it.Citations) > 0 {
			w.line("Trích dẫn:")
			for i, c := range it.Citations {
				if triggerTextCitation(c.Locator) {
					w.line("%d. %s", i+1, c.Locator)
				} else {
					w.line("%d. %s: \"%s\"", i+1, c.Locator, c.Quote)
				}
			}
		}
		if len(it.Actions) > 0 {
			w.line("Hành động:")
			for _, a := range it.Actions {
				s := fmt.Sprintf("#%d %s: %s", a.Index, a.Type, a.Status)
				if a.Code != "" && a.Code != a.Status {
					s += " (" + a.Code + ")"
				}
				if a.Block != "" {
					s += " · block " + a.Block
				}
				w.line("- %s", s)
			}
		}
		if len(it.Decisions) > 0 {
			w.line("Quyết định:")
			for _, d := range it.Decisions {
				if d.Decision == "" {
					w.line("- %s: đang chờ, hết hạn %s", d.Kind, w.time(d.ExpiresAt))
					continue
				}
				s := fmt.Sprintf("- %s: %s", d.Kind, d.Decision)
				if d.Actor != "" {
					s += " bởi " + d.Actor
				}
				s += " lúc " + w.timePtr(d.DecidedAt)
				w.line("%s", s)
				if d.Answer != "" {
					w.line("  Trả lời: %s", d.Answer)
				}
			}
		}
	}
	return w.b.String()
}

// chatTrigger renders the trusted identifiers of a chat-triggered run:
// channel type, sender and message IDs, the channel name (channel
// messages), whether the owner is tagged, and the pull requests. The message
// text is never stored, so it cannot be shown.
func (w *traceText) chatTrigger(r TraceRun) {
	td, err := DecodeTriggerData(r.Trigger)
	if err == nil {
		w.line("Chat: %s · người gửi %s · tin nhắn %s", orDash(td.ChannelType), orDash(td.SenderID), orDash(td.MessageID))
		if td.ChannelName != "" {
			w.line("Kênh: %s", td.ChannelName)
		}
		if td.Mentioned {
			w.line("Nhắc tới: owner")
		}
	}
	if len(r.PRs) > 0 {
		w.line("PR: %s", strings.Join(r.PRs, ", "))
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// runActions renders the run-level action outcomes: type, status and code,
// each pull request's result and review ID, and the reaction.
func (w *traceText) runActions(as []RunActionOutcome) {
	if len(as) == 0 {
		return
	}
	w.line("Hành động run (%d):", len(as))
	for _, a := range as {
		s := fmt.Sprintf("- #%d %s: %s", a.Index, a.Type, a.Status)
		if a.Code != "" && a.Code != a.Status {
			s += " (" + a.Code + ")"
		}
		if a.Reaction != "" {
			s += " · reaction " + a.Reaction
		}
		if a.Result != "" {
			s += " · " + a.Result
		}
		if a.TelegramMessageID != 0 {
			s += fmt.Sprintf(" · tin %d", a.TelegramMessageID)
		}
		if a.VerdictConfidence != nil {
			s += fmt.Sprintf(" · AI %s %.2f", a.VerdictLabel, *a.VerdictConfidence)
		}
		w.line("%s", s)
		for _, p := range a.PRs {
			ps := fmt.Sprintf("  • %s: %s", PRRef{Repo: p.Repo, Number: p.Number}.String(), p.Status)
			if p.Result != "" {
				ps += " · " + p.Result
			}
			if p.Code != "" && p.Code != p.Status {
				ps += " (" + p.Code + ")"
			}
			if p.ReviewID != 0 {
				ps += fmt.Sprintf(" · review %d", p.ReviewID)
			}
			w.line("%s", ps)
		}
	}
}

// verdict renders the run's model verdict: label, confidence, flags and
// summary, or why there is none.
func (w *traceText) verdict(v *RunVerdict) {
	if v == nil {
		return
	}
	if !v.Available() {
		w.line("AI: không có đánh giá (%s)", verdictCode(v))
		return
	}
	s := fmt.Sprintf("AI: %s (%.2f)", v.Label, v.Confidence)
	if len(v.Flags) > 0 {
		s += " · cờ: " + strings.Join(v.Flags, ", ")
	}
	w.line("%s", s)
	if v.Summary != "" {
		w.line("Tóm tắt: %s", v.Summary)
	}
}

func (w *traceText) toolCalls(calls []ToolCallRecord) {
	w.line("Tool calls (%d):", len(calls))
	for _, c := range calls {
		s := fmt.Sprintf("- #%d bước %d %s [%s] %s", c.ID, c.Step, c.Tool, c.Tier, c.Status)
		if c.ErrorCode != "" {
			s += " (" + c.ErrorCode + ")"
		}
		w.line("%s · %dms · %s", s, c.DurationMS, w.time(c.CreatedAt))
	}
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += max(utf16.RuneLen(r), 1)
	}
	return n
}

// capTraceText keeps the text within MaxTraceText, cutting at a line end
// when it can and saying where the full trace is.
func capTraceText(s string) string {
	s = strings.TrimRight(s, "\n")
	if utf16Len(s) <= MaxTraceText {
		return s
	}
	budget := MaxTraceText - utf16Len(traceMore) - 1
	n, cut := 0, 0
	for i, r := range s {
		n += max(utf16.RuneLen(r), 1)
		if n > budget {
			break
		}
		cut = i + utf8.RuneLen(r)
	}
	head := s[:cut]
	if i := strings.LastIndexByte(head, '\n'); i > len(head)/2 {
		head = head[:i]
	}
	return head + "\n" + traceMore
}

// --- Tracer for Telegram ---

// TraceService implements Tracer over a TraceStore. Profiles, when set,
// gives the owner's time zone.
type TraceService struct {
	Store    TraceStore
	Profiles ProfileStore
}

var _ Tracer = (*TraceService)(nil)

const usageTraceRef = "Cú pháp: /trace <run id | task id/URL | clickup:chat:<message id> | correlation id> [item]"

func (s *TraceService) location(ctx context.Context, workspaceID string) *time.Location {
	tz := DefaultProfile().Timezone
	if s.Profiles != nil {
		if p, err := s.Profiles.GetProfile(ctx, workspaceID); err == nil && p.Timezone != "" {
			tz = p.Timezone
		}
	}
	if loc, err := time.LoadLocation(tz); err == nil {
		return loc
	}
	return time.UTC
}

func (s *TraceService) Trace(ctx context.Context, r TraceRequest) (string, error) {
	if s == nil || s.Store == nil {
		return "", ErrNotAvailable
	}
	if len(r.Item) > 200 {
		return "", &UsageError{Usage: usageTraceRef}
	}
	t, err := BuildTrace(ctx, s.Store, r.WorkspaceID, r.Ref, r.Item)
	switch {
	case errors.Is(err, ErrInvalid):
		return "", &UsageError{Usage: usageTraceRef}
	case errors.Is(err, ErrNotFound) && r.Item != "" && t.Run.ID != 0:
		return fmt.Sprintf("Run #%d không có item %s.", t.Run.ID, clip(redactText(r.Item), 60)), nil
	case errors.Is(err, ErrNotFound):
		return "Không tìm thấy run nào.", nil
	case err != nil:
		return "", err
	}
	return RenderTraceText(t, s.location(ctx, r.WorkspaceID), r.Item != ""), nil
}
