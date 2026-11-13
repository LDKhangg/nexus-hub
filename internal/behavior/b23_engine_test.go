package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/descblock"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// Embedded interfaces deliberately fail closed (panic) if a test unexpectedly
// reaches a dependency it did not configure. No network or database is used.
type b23Store struct {
	ActionStore
	run       Run
	items     []Item
	approvals []Approval
	messages  map[string]int64
	decisions []NewDecision
}

func (s *b23Store) GetRun(_ context.Context, ws string, id int64) (Run, error) {
	if ws != s.run.WorkspaceID || id != s.run.ID {
		return Run{}, ErrNotFound
	}
	return s.run, nil
}
func (s *b23Store) SetSnapshot(_ context.Context, _ string, _ int64, raw json.RawMessage) (bool, error) {
	if len(s.run.Snapshot) > 0 {
		return false, nil
	}
	s.run.Snapshot = append(json.RawMessage(nil), raw...)
	return true, nil
}
func (s *b23Store) MarkUndone(_ context.Context, _ string, _ int64) error {
	if s.run.UndoneAt != nil {
		return ErrConflict
	}
	n := time.Now()
	s.run.UndoneAt = &n
	return nil
}
func (s *b23Store) GetItem(_ context.Context, _ string, id int64) (Item, error) {
	for _, it := range s.items {
		if it.ID == id {
			return it, nil
		}
	}
	return Item{}, ErrNotFound
}
func (s *b23Store) ListItems(context.Context, string, int64) ([]Item, error) {
	return append([]Item(nil), s.items...), nil
}
func (s *b23Store) TransitionItem(_ context.Context, _ string, id int64, from []ItemStatus, to ItemStatus, raw json.RawMessage) (Item, error) {
	for i := range s.items {
		it := &s.items[i]
		if it.ID == id {
			if !slices.Contains(from, it.Status) {
				return Item{}, ErrConflict
			}
			it.Status = to
			if raw != nil {
				it.ActionResult = raw
			}
			return *it, nil
		}
	}
	return Item{}, ErrNotFound
}
func (s *b23Store) RateItem(_ context.Context, _ string, id int64, r Rating, reason string) error {
	for i := range s.items {
		if s.items[i].ID == id {
			s.items[i].Rating = r
			s.items[i].RatingReason = reason
			return nil
		}
	}
	return ErrNotFound
}
func (s *b23Store) ListApprovals(context.Context, string, int64) ([]Approval, error) {
	return append([]Approval(nil), s.approvals...), nil
}
func (s *b23Store) CreateApproval(_ context.Context, ws string, n NewApproval) (Approval, error) {
	a := Approval{ID: int64(len(s.approvals) + 1), WorkspaceID: ws, Kind: n.Kind, RunID: n.RunID, ItemID: n.ItemID, Action: n.Action, Resource: n.Resource, Nonce: n.Nonce, ExpiresAt: n.ExpiresAt, Payload: n.Payload, Options: n.Options}
	s.approvals = append(s.approvals, a)
	return a, nil
}
func (s *b23Store) GetApprovalByNonce(_ context.Context, ws, nonce string) (Approval, error) {
	for _, a := range s.approvals {
		if a.WorkspaceID == ws && a.Nonce == nonce {
			return a, nil
		}
	}
	return Approval{}, ErrNotFound
}
func (s *b23Store) Decide(_ context.Context, ws, nonce string, d Decision, answer, actor string, now time.Time) (Approval, error) {
	for i := range s.approvals {
		a := &s.approvals[i]
		if a.WorkspaceID == ws && a.Nonce == nonce {
			if a.Decision != "" {
				return Approval{}, ErrDecided
			}
			if !now.Before(a.ExpiresAt) {
				return Approval{}, ErrExpired
			}
			a.Decision = d
			a.Answer = answer
			a.DecidedBy = actor
			a.DecidedAt = &now
			return *a, nil
		}
	}
	return Approval{}, ErrNotFound
}
func (s *b23Store) SetApprovalMessage(_ context.Context, _ string, id, msg int64) error {
	for i := range s.approvals {
		if s.approvals[i].ID == id {
			s.approvals[i].TelegramMessageID = msg
			return nil
		}
	}
	return ErrNotFound
}
func (s *b23Store) SetReplyMessage(_ context.Context, _ string, id, msg int64) error {
	for i := range s.approvals {
		if s.approvals[i].ID == id {
			s.approvals[i].ReplyMessageID = msg
			return nil
		}
	}
	return ErrNotFound
}
func (s *b23Store) ClaimMessage(_ context.Context, _ string, _ int64, key string) (bool, int64, error) {
	if id, ok := s.messages[key]; ok {
		return false, id, nil
	}
	s.messages[key] = 0
	return true, 0, nil
}
func (s *b23Store) SetMessageID(_ context.Context, _ string, _ int64, key string, id int64) error {
	s.messages[key] = id
	return nil
}
func (s *b23Store) ReleaseMessage(_ context.Context, _ string, _ int64, key string) error {
	delete(s.messages, key)
	return nil
}
func (s *b23Store) RecordDecision(_ context.Context, _ string, d NewDecision) error {
	s.decisions = append(s.decisions, d)
	return nil
}
func (s *b23Store) ExpireDue(_ context.Context, now time.Time, limit int) ([]Approval, error) {
	var out []Approval
	for i := range s.approvals {
		a := &s.approvals[i]
		if len(out) < limit && a.Decision == "" && !now.Before(a.ExpiresAt) {
			a.Decision = DecisionExpired
			out = append(out, *a)
		}
	}
	return out, nil
}

type b23Defs struct {
	DefinitionStore
	versions  []Version
	requested []int64
}

func (d *b23Defs) GetVersionByID(_ context.Context, _ string, id int64) (Version, error) {
	d.requested = append(d.requested, id)
	for _, v := range d.versions {
		if v.ID == id {
			return v, nil
		}
	}
	return Version{}, ErrNotFound
}
func (d *b23Defs) EnabledVersions(context.Context, string) ([]Version, error) { return d.versions, nil }

type b23Profile struct {
	ProfileStore
	p Profile
}

func (p b23Profile) GetProfile(context.Context, string) (Profile, error) { return p.p, nil }

type b23Policy struct{ cfg tenant.AgentConfig }

func (p b23Policy) RunPolicy(context.Context, string, string) (tenant.AgentConfig, error) {
	return p.cfg, nil
}

type b23Tasks struct {
	state TaskState
	reads int
}

func (p *b23Tasks) ReadTask(context.Context, string, string, string) (TaskState, error) {
	p.reads++
	return p.state, nil
}

type b23Chats struct{}

func (b23Chats) OwnerChat(context.Context, string, TriggerData) (ChatRef, error) {
	return ChatRef{WorkspaceID: "ws", IntegrationID: "bot", ChatID: 42}, nil
}

type b23Messenger struct {
	sent    []OutMessage
	removed []int64
	fail    error
}

func (m *b23Messenger) SendMessage(_ context.Context, _ ChatRef, out OutMessage) (int64, error) {
	if m.fail != nil {
		return 0, m.fail
	}
	m.sent = append(m.sent, out)
	return int64(len(m.sent) + 100), nil
}
func (m *b23Messenger) RemoveButtons(_ context.Context, _ ChatRef, id int64) error {
	m.removed = append(m.removed, id)
	return nil
}
func (m *b23Messenger) AnswerCallback(context.Context, ChatRef, string, string) error { return nil }

type b23Caller func(context.Context, int, string, json.RawMessage) tools.CallResult

func (f b23Caller) Call(c context.Context, n int, s string, r json.RawMessage) tools.CallResult {
	return f(c, n, s, r)
}

type b23Write struct {
	tool, scope string
	input       map[string]any
	policy      tenant.ToolPolicyConfig
	run         Run
}
type b23Harness struct {
	e      *Engine
	s      *b23Store
	tasks  *b23Tasks
	m      *b23Messenger
	rc     RunContext
	writes []b23Write
	now    time.Time
}

func b23NewEngine(t *testing.T, doc Doc, items ...Item) *b23Harness {
	t.Helper()
	v := b23Version(t, doc, 1)
	rd, err := ResolveVersion(v)
	if err != nil {
		t.Fatal(err)
	}
	rd.Language = "vi"
	h := &b23Harness{now: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC), tasks: &b23Tasks{state: TaskState{ID: "abc", Status: "open", AssigneeIDs: []string{"7"}, Description: "Human introduction\nHuman footer", Markdown: "Human introduction\nHuman footer"}}, m: &b23Messenger{}}
	h.s = &b23Store{run: Run{ID: 11, WorkspaceID: "ws", VersionID: 1, Version: 1, Subject: TaskSubject("abc"), CorrelationID: "019458ab-1234-7000-8000-000000000001", Trigger: json.RawMessage(`{"type":"telegram.command","list_id":"list-1"}`)}, items: items, messages: map[string]int64{}}
	for i := range h.s.items {
		h.s.items[i].RunID = 11
	}
	h.rc = RunContext{Run: h.s.run, Doc: rd, Profile: DefaultProfile(), Trigger: TriggerData{ListID: "list-1"}}
	h.e, err = NewEngine(ActionDeps{Store: h.s, Defs: &b23Defs{versions: []Version{v}}, Profiles: b23Profile{p: DefaultProfile()}, Policies: b23Policy{}, Tasks: h.tasks, Chats: b23Chats{}, Messenger: h.m, Decisions: h.s,
		Callers: func(run Run, scope string, policy tenant.ToolPolicyConfig, _ string) ToolCaller {
			return b23Caller(func(_ context.Context, _ int, name string, raw json.RawMessage) tools.CallResult {
				var in map[string]any
				if err := json.Unmarshal(raw, &in); err != nil {
					t.Fatal(err)
				}
				h.writes = append(h.writes, b23Write{tool: name, scope: scope, input: in, policy: policy, run: run})
				if name == ToolSetDescriptionBlock {
					var next string
					var err error
					if in["remove"] == true {
						next, err = descblock.Remove(h.tasks.state.Markdown, in["block"].(string))
					} else {
						next, err = descblock.Replace(h.tasks.state.Markdown, in["block"].(string), in["body"].(string))
					}
					if err != nil {
						return tools.CallResult{IsError: true, Content: "error: block_markers_invalid"}
					}
					h.tasks.state.Markdown = next
					hash, _ := descblock.OutsideHash(next)
					return tools.CallResult{Content: "desc_sha256=" + descblock.Hash(next) + " outside_sha256=" + hash}
				}
				return tools.CallResult{Content: "ok"}
			})
		}}, ActionSettings{OwnerClickUpUserID: "7"})
	if err != nil {
		t.Fatal(err)
	}
	h.e.now = func() time.Time { return h.now }
	return h
}
func (h *b23Harness) dispatch(t *testing.T) {
	t.Helper()
	if err := h.e.Dispatch(context.Background(), h.rc, h.s.items); err != nil {
		t.Fatal(err)
	}
}
func (h *b23Harness) press(a Approval, k CallbackKind) (CallbackResult, error) {
	return h.e.HandleCallback(context.Background(), CallbackAction{WorkspaceID: "ws", IntegrationID: "bot", ChatID: 42, Actor: "telegram:42", Approval: a, Data: CallbackData{Kind: k, Nonce: a.Nonce, Option: 0}})
}
func (h *b23Harness) approval(t *testing.T, k ApprovalKind) Approval {
	t.Helper()
	for _, a := range h.s.approvals {
		if a.Kind == k {
			return a
		}
	}
	t.Fatalf("no %s approval", k)
	return Approval{}
}

// P3-07 (REQ-12, REQ-13, REQ-14, REQ-19, REQ-23): drafts require an owner decision; stale UI cannot post twice.
func TestB23DraftApprovalIsBoundAndWritesOnlyOnce(t *testing.T) {
	h := b23NewEngine(t, b23Doc(), Item{ID: 1, Key: "q", Text: "Question", Proposal: "Answer", Topic: "design", Class: ClassAuto, Status: ItemDrafted})
	h.dispatch(t)
	h.dispatch(t)
	if len(h.writes) != 0 || len(h.m.sent) != 1 {
		t.Fatal("dry-run wrote or repeated its draft")
	}
	a := h.approval(t, ApprovalPostDraft)
	if a.RunID != 11 || a.ItemID != 1 || a.Action != "auto" || a.Resource != TaskSubject("abc") || !a.ExpiresAt.Equal(h.now.Add(72*time.Hour)) {
		t.Fatalf("unbound approval: %+v", a)
	}
	if !strings.Contains(h.m.sent[0].Buttons[0][0].Text, "Đăng") {
		t.Fatal("no post button")
	}
	if _, err := h.press(a, CallbackApprove); err != nil {
		t.Fatal(err)
	}
	if _, err := h.press(a, CallbackApprove); !errors.Is(err, ErrDecided) {
		t.Fatalf("second press: %v", err)
	}
	if len(h.writes) != 1 || h.s.items[0].Status != ItemDone {
		t.Fatal("post did not have exactly one effect")
	}
	w := h.writes[0]
	if w.tool != ToolPostComment || w.input["task_id"] != "abc" || w.input["text"] != "Answer" || w.scope != "behavior_action:1:0" || !slices.Equal(w.policy.Enabled, []string{ToolPostComment}) || w.run.CorrelationID != h.rc.Run.CorrelationID {
		t.Fatalf("write escaped scoped action contract: %+v", w)
	}
	dec := h.s.approvals[0]
	if dec.DecidedBy != "telegram:42" || dec.DecidedAt == nil || !dec.DecidedAt.Equal(h.now) {
		t.Fatal("actor/time missing")
	}
	if len(h.s.decisions) != 1 || h.s.decisions[0].Source != SourceAutoPosted || h.s.decisions[0].Scope.Behavior != "owner-review" || h.s.decisions[0].Scope.List != "list-1" {
		t.Fatal("posted decision not remembered in scope")
	}
}

// P3-08 (REQ-14): expiry includes the exact boundary; rejection and arbitrary replies are irreversible decisions.
func TestB23AskExpirySkipAndArbitraryReply(t *testing.T) {
	for _, mode := range []string{"expiry", "skip", "reply"} {
		t.Run(mode, func(t *testing.T) {
			h := b23NewEngine(t, b23Doc(), Item{ID: 1, Key: "q", Text: "Chọn cách xử lý?", Proposal: "Suggested", Class: ClassAsk, Status: ItemDrafted, Citations: []Citation{{Locator: "clickup:task/abc#desc:L1", Quote: "Human introduction"}}})
			h.dispatch(t)
			a := h.approval(t, ApprovalItemAnswer)
			if h.s.items[0].Status != ItemAsked || !strings.Contains(h.m.sent[0].Text, "Suggested") || !strings.Contains(h.m.sent[0].Text, "clickup:task/abc#desc:L1") {
				t.Fatal("ask omitted suggestion/evidence")
			}
			switch mode {
			case "expiry":
				h.now = a.ExpiresAt
				if _, err := h.press(a, CallbackOption); !errors.Is(err, ErrExpired) {
					t.Fatalf("late press: %v", err)
				}
				if n, err := h.e.Sweep(context.Background()); err != nil || n != 1 {
					t.Fatalf("sweep %d %v", n, err)
				}
				if h.s.items[0].Status != ItemExpired {
					t.Fatal("item not expired")
				}
			case "skip":
				if _, err := h.press(a, CallbackReject); err != nil {
					t.Fatal(err)
				}
				if h.s.items[0].Status != ItemSkipped {
					t.Fatal("skip did not move item")
				}
				if _, err := h.press(a, CallbackOption); !errors.Is(err, ErrDecided) {
					t.Fatal("skip could later be approved")
				}
			case "reply":
				if _, err := h.press(a, CallbackFreeText); err != nil {
					t.Fatal(err)
				}
				if !h.m.sent[len(h.m.sent)-1].ForceReply || h.s.approvals[0].ReplyMessageID == 0 {
					t.Fatal("free text not bound to a reply message")
				}
				ans := FreeTextAnswer{WorkspaceID: "ws", IntegrationID: "bot", ChatID: 42, Approval: a, Actor: "telegram:42", Answer: "  Câu trả lời khác ✅  "}
				if _, err := h.e.HandleFreeText(context.Background(), ans); err != nil {
					t.Fatal(err)
				}
				if _, err := h.e.HandleFreeText(context.Background(), ans); !errors.Is(err, ErrDecided) {
					t.Fatal("reply replay wrote twice")
				}
				if len(h.writes) != 1 || h.writes[0].input["text"] != "Câu trả lời khác ✅" || h.s.items[0].Status != ItemAnswered || len(h.s.decisions) != 1 || h.s.decisions[0].Answer != "Câu trả lời khác ✅" {
					t.Fatal("arbitrary answer was not the effect")
				}
			}
			if mode != "reply" && len(h.writes) != 0 {
				t.Fatal("expired or rejected answer wrote")
			}
		})
	}
}

// P3-09 (REQ-15, REQ-16): human edits and broken markers fail closed; edits inside a valid block do not conflict.
func TestB23TargetRevalidationDistinguishesHumanEditsFromManagedBlock(t *testing.T) {
	for _, tc := range []struct {
		name, edit string
		conflict   bool
	}{
		{"outside", "Human changed the task", true},
		{"missing-end", "Human introduction\n⟦nexus:owner-review:start⟧\nold", true},
		{"duplicate", "Human introduction\n⟦nexus:owner-review:start⟧\nx\n⟦nexus:owner-review:end⟧\n⟦nexus:owner-review:start⟧\ny\n⟦nexus:owner-review:end⟧", true},
		{"inside", "Human introduction\nHuman footer\n\n⟦nexus:owner-review:start⟧\nChanged block\n⟦nexus:owner-review:end⟧", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := b23Doc()
			d.Actions["auto"] = []ActionDoc{{Type: ActionDescriptionBlock}}
			h := b23NewEngine(t, d, Item{ID: 1, Key: "q", Proposal: "Answer", Class: ClassAuto, Status: ItemDrafted})
			h.dispatch(t)
			h.tasks.state.Markdown = tc.edit
			r, err := h.press(h.approval(t, ApprovalPostDraft), CallbackApprove)
			if err != nil {
				t.Fatal(err)
			}
			if tc.conflict {
				if len(h.writes) != 0 || h.s.items[0].Status != ItemConflict || r.Reply == "" {
					t.Fatal("conflict wrote, was not persisted, or did not notify")
				}
			} else {
				if len(h.writes) != 1 || h.s.items[0].Status != ItemDone || !strings.Contains(h.tasks.state.Markdown, "Human footer") {
					t.Fatal("valid block update damaged outside or was refused")
				}
				if h.writes[0].input["if_outside_sha256"] == "" {
					t.Fatal("missing write-time guard")
				}
			}
		})
	}
}

// P3-09 (REQ-15): even live auto status/unassign actions need exact-change confirmation and a new read.
func TestB23StatusAndUnassignAlwaysNeedConfirmation(t *testing.T) {
	d := b23Doc()
	d.DryRun = b23Ptr(false)
	d.Actions["auto"] = []ActionDoc{{Type: ActionSetStatus, Params: map[string]string{"status": "closed"}}, {Type: ActionUnassign, Params: map[string]string{"user": "me"}}}
	h := b23NewEngine(t, d, Item{ID: 1, Key: "q", Proposal: "OK", Class: ClassAuto, Status: ItemDrafted})
	h.dispatch(t)
	if len(h.writes) != 0 {
		t.Fatal("dangerous auto action bypassed confirmation")
	}
	a := h.approval(t, ApprovalConfirmAction)
	msg := h.m.sent[0].Text
	for _, word := range []string{"abc", "open", "closed", "7"} {
		if !strings.Contains(msg, word) {
			t.Errorf("confirmation missing exact change %s", word)
		}
	}
	h.tasks.state.Status = "in progress"
	h.tasks.state.AssigneeIDs = nil
	if _, err := h.press(a, CallbackApprove); err != nil {
		t.Fatal(err)
	}
	if len(h.writes) != 0 {
		t.Fatal("confirmation overwrote a newer status/assignment")
	}
	res := decodeResult(h.s.items[0].ActionResult)
	if len(res.Actions) != 2 || res.Actions[0].Status != OutcomeChanged || res.Actions[1].Status != OutcomeAlready {
		t.Fatalf("stale changes not explained: %+v", res)
	}
}

// P3-10 (REQ-16, REQ-23): undo restores the previous block once, but refuses a subsequent human edit.
func TestB23UndoRestoresOnlyUnchangedDescription(t *testing.T) {
	for _, edited := range []bool{false, true} {
		t.Run(fmt.Sprint(edited), func(t *testing.T) {
			d := b23Doc()
			d.Actions["auto"] = []ActionDoc{{Type: ActionDescriptionBlock}}
			h := b23NewEngine(t, d, Item{ID: 1, Key: "q", Text: "Question", Proposal: "Answer", Class: ClassAuto, Status: ItemDrafted})
			before := "Human introduction\n⟦nexus:owner-review:start⟧\nPrevious answer\n⟦nexus:owner-review:end⟧\nHuman footer"
			h.tasks.state.Markdown = before
			h.dispatch(t)
			if _, err := h.press(h.approval(t, ApprovalPostDraft), CallbackApprove); err != nil {
				t.Fatal(err)
			}
			if edited {
				h.tasks.state.Markdown += "\nNew human edit"
			}
			current := h.tasks.state.Markdown
			if _, err := h.e.Undo(context.Background(), UndoRequest{WorkspaceID: "ws", RunID: 11, Actor: "telegram:42"}); err != nil {
				t.Fatal(err)
			}
			if edited {
				if len(h.writes) != 1 || h.tasks.state.Markdown != current || h.s.run.UndoneAt != nil {
					t.Fatal("undo clobbered newer description")
				}
			} else {
				if h.tasks.state.Markdown != before || len(h.writes) != 2 || h.s.run.UndoneAt == nil {
					t.Fatal("undo did not restore snapshot")
				}
				if h.writes[1].input["if_current_sha256"] != descblock.Hash(current) {
					t.Fatal("undo omitted exact current guard")
				}
				if _, err := h.e.Undo(context.Background(), UndoRequest{WorkspaceID: "ws", RunID: 11}); err != nil {
					t.Fatal(err)
				}
				if len(h.writes) != 2 {
					t.Fatal("double undo wrote again")
				}
				if len(h.s.decisions) != 2 || h.s.decisions[1].Source != SourceAutoUndone {
					t.Fatal("undo memory missing")
				}
			}
		})
	}
}

// P3-11 (REQ-17, REQ-25): a retried run has one digest containing all notify items.
func TestB23DigestContainsEveryItemOnceAndFeedbackButtons(t *testing.T) {
	h := b23NewEngine(t, b23Doc(), Item{ID: 1, Key: "first", Text: "First notice", Class: ClassNotify, Status: ItemDrafted}, Item{ID: 2, Key: "second", Text: "Second notice", Class: ClassNotify, Status: ItemDrafted})
	h.dispatch(t)
	h.dispatch(t)
	if len(h.m.sent) != 1 || !strings.Contains(h.m.sent[0].Text, "First notice") || !strings.Contains(h.m.sent[0].Text, "Second notice") {
		t.Fatal("digest lost or duplicated items")
	}
	// REQ-25 says EVERY behavior message, including multi-item digests.
	if len(h.m.sent[0].Buttons) == 0 {
		t.Error("multi-item digest has no feedback buttons (REQ-25)")
	}
}

// P3-12 (REQ-25): rating plus a private reason changes the stored item, not its decision or action.
func TestB23FeedbackStoresRatingAndRedactsPastedCredential(t *testing.T) {
	h := b23NewEngine(t, b23Doc(), Item{ID: 1, Key: "q", Text: "Question", Proposal: "Answer", Class: ClassAsk, Status: ItemDrafted})
	h.dispatch(t)
	a := h.approval(t, ApprovalItemAnswer)
	if _, err := h.press(a, CallbackRateDown); err != nil {
		t.Fatal(err)
	}
	fb := h.approval(t, ApprovalFeedbackReason)
	fake := "ghp_" + strings.Repeat("A", 36)
	if _, err := h.e.HandleFreeText(context.Background(), FreeTextAnswer{WorkspaceID: "ws", Approval: fb, Actor: "telegram:42", Answer: "Sai vì " + fake}); err != nil {
		t.Fatal(err)
	}
	if h.s.items[0].Rating != RatingDown || h.s.items[0].RatingReason == "" || strings.Contains(h.s.items[0].RatingReason, fake) {
		t.Fatal("feedback lost or leaked credential")
	}
	if h.s.items[0].Status != ItemAsked || h.s.approvals[0].Decision != "" || len(h.writes) != 0 {
		t.Fatal("feedback answered the ask")
	}
	if _, err := h.press(a, CallbackRateUp); err != nil {
		t.Fatal(err)
	}
	if h.s.items[0].Rating != RatingUp || h.s.items[0].RatingReason != "" {
		t.Fatal("rating correction retained stale reason")
	}
}

// P3-07 (REQ-13): a failed Telegram send is retryable without losing the draft.
func TestB23FailedDraftSendCanBeRetriedWithoutWriting(t *testing.T) {
	h := b23NewEngine(t, b23Doc(), Item{ID: 1, Key: "q", Proposal: "Answer", Class: ClassAuto, Status: ItemDrafted})
	h.m.fail = errors.New("fake unavailable")
	if err := h.e.Dispatch(context.Background(), h.rc, h.s.items); err == nil {
		t.Fatal("send failure hidden")
	}
	h.m.fail = nil
	h.dispatch(t)
	if len(h.m.sent) != 1 || len(h.s.approvals) != 1 || len(h.writes) != 0 || h.s.items[0].Status != ItemDrafted {
		t.Fatal("retry duplicated approval or changed draft")
	}
}

// P3-09 (REQ-15): revalidation must happen before EACH write, not once per item.
func TestB23HumanEditBetweenTwoActionsStopsSecondWrite(t *testing.T) {
	d := b23Doc()
	d.Actions["auto"] = []ActionDoc{{Type: ActionClickUpComment}, {Type: ActionDescriptionBlock}}
	h := b23NewEngine(t, d, Item{ID: 1, Key: "q", Text: "Question", Proposal: "Answer", Class: ClassAuto, Status: ItemDrafted})
	original := h.e.d.Callers
	h.e.d.Callers = func(run Run, scope string, p tenant.ToolPolicyConfig, ref string) ToolCaller {
		caller := original(run, scope, p, ref)
		return b23Caller(func(ctx context.Context, step int, name string, input json.RawMessage) tools.CallResult {
			out := caller.Call(ctx, step, name, input)
			if name == ToolPostComment {
				h.tasks.state.Markdown = "Human changed the task between actions"
			}
			return out
		})
	}
	h.dispatch(t)
	if _, err := h.press(h.approval(t, ApprovalPostDraft), CallbackApprove); err != nil {
		t.Fatal(err)
	}
	if len(h.writes) != 1 || h.s.items[0].Status != ItemConflict {
		t.Fatalf("human edit between actions was not revalidated: writes=%d status=%s", len(h.writes), h.s.items[0].Status)
	}
}

// P3-07 (REQ-12, REQ-13): live autos write once and send one FYI per run, not one per item.
func TestB23LiveAutoItemsWriteOnceAndShareOneFYI(t *testing.T) {
	d := b23Doc()
	d.DryRun = b23Ptr(false)
	h := b23NewEngine(t, d,
		Item{ID: 1, Key: "q1", Proposal: "First answer", Class: ClassAuto, Status: ItemDrafted},
		Item{ID: 2, Key: "q2", Proposal: "Second answer", Class: ClassAuto, Status: ItemDrafted})
	h.dispatch(t)
	h.dispatch(t)
	if len(h.writes) != 2 || len(h.m.sent) != 1 || !strings.Contains(h.m.sent[0].Text, "First answer") || !strings.Contains(h.m.sent[0].Text, "Second answer") {
		t.Fatal("live dispatch duplicated writes or omitted the single complete FYI")
	}
	if h.writes[0].scope == h.writes[1].scope {
		t.Fatal("different items share an idempotency key")
	}
}

type b23DigestQueue struct {
	DigestStore
	keys []string
	due  []time.Time
}

func (q *b23DigestQueue) QueueDigest(_ context.Context, _ string, _ int64, key string, _ ChatRef, due time.Time) (bool, error) {
	q.keys = append(q.keys, key)
	q.due = append(q.due, due)
	return true, nil
}

// P3-11 (REQ-17, REQ-22): quiet hours defer notify and FYI effects, never an ask.
func TestB23QuietHoursQueueDigestAndFYIButSendAskImmediately(t *testing.T) {
	d := b23Doc()
	d.DryRun = b23Ptr(false)
	h := b23NewEngine(t, d,
		Item{ID: 1, Key: "auto", Proposal: "Posted answer", Class: ClassAuto, Status: ItemDrafted},
		Item{ID: 2, Key: "notify", Text: "Quiet notice", Class: ClassNotify, Status: ItemDrafted},
		Item{ID: 3, Key: "ask", Text: "Urgent question", Proposal: "Suggested", Class: ClassAsk, Status: ItemDrafted})
	h.now = time.Date(2026, 11, 13, 23, 0, 0, 0, time.UTC)
	h.rc.Profile.Timezone = "UTC"
	h.rc.Profile.QuietStart = b23Ptr(22 * 60)
	h.rc.Profile.QuietEnd = b23Ptr(7 * 60)
	q := &b23DigestQueue{}
	h.e.d.Digests = q
	h.dispatch(t)
	if len(h.m.sent) != 1 || !strings.Contains(h.m.sent[0].Text, "Urgent question") || len(q.keys) != 2 || !slices.Contains(q.keys, DigestKeyNotify) || !slices.Contains(q.keys, DigestKeyFYI) {
		t.Fatal("quiet hours suppressed ask or immediately sent informing messages")
	}
	want := time.Date(2026, 11, 14, 7, 0, 0, 0, time.UTC)
	for _, due := range q.due {
		if !due.Equal(want) {
			t.Fatalf("queued until %s, want next morning", due)
		}
	}
}
