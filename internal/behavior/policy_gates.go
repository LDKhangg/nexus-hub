package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/policy"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// This file is the owner's side of a Phase 9 policy gate (REQ-10): the
// approval message with "Duyệt" / "Từ chối" in the execution's topic, the
// owner's press, and the edit of the message after the decision. The gate
// itself (an approvals row of kind policy_gate) binds one exact action:
// workspace, action, resource key, input hash and execution. Messages and
// payloads carry ids, a short commit SHA and the rule key only.

// ApprovalPolicyGate is an owner approval of one exact action asked by the
// policy engine (approvals.kind policy_gate).
const ApprovalPolicyGate ApprovalKind = "policy_gate"

// PolicyGateRecord is a stored policy gate as the behavior side reads it.
type PolicyGateRecord struct {
	// WorkspaceID is set by ExpireEndedExecutionGates only.
	WorkspaceID string
	ID          int64
	Nonce       string
	Decision    Decision // "" while open
	ExpiresAt   time.Time
	Consumed    bool
	Action      string
	ResourceKey string
	InputSHA256 string
	ExecutionID int64
	MessageID   int64
	Payload     json.RawMessage
	DecidedBy   string
}

// PolicyGateStore is the storage of policy gates (storage.PolicyStore).
type PolicyGateStore interface {
	policy.GateStore
	// PolicyGate returns one gate of the workspace; ErrNotFound when none.
	PolicyGate(ctx context.Context, workspaceID string, id int64) (PolicyGateRecord, error)
	// SetPolicyGateMessage stores the gate's message id and payload.
	SetPolicyGateMessage(ctx context.Context, workspaceID string, id, messageID int64, payload json.RawMessage) error
	// DecidePolicyGate decides an open, unexpired gate once, recording the
	// Telegram actor and the internal user; ErrDecided, ErrExpired or
	// ErrNotFound otherwise.
	DecidePolicyGate(ctx context.Context, workspaceID, nonce string, d Decision, actor, userID string, now time.Time) (PolicyGateRecord, error)
	// WorkspaceOwner returns the users.id of the workspace member with role
	// owner; ErrNotFound when there is none.
	WorkspaceOwner(ctx context.Context, workspaceID string) (string, error)
}

// PolicyGateEndStore closes the policy gates of ended executions (review B
// R-B-1); a PolicyGateStore without it leaves them to expire on their own.
type PolicyGateEndStore interface {
	// ExpireEndedExecutionGates expires up to limit open policy gates whose
	// execution completed, failed, was cancelled or is past its deadline,
	// and returns them.
	ExpireEndedExecutionGates(ctx context.Context, now time.Time, limit int) ([]PolicyGateRecord, error)
}

// policyGatePayload is where a gate's message went and what it shows
// (ids, a short SHA and the rule key only).
type policyGatePayload struct {
	IntegrationID string `json:"integration_id"`
	ChatID        int64  `json:"chat_id"`
	ThreadID      int64  `json:"thread_id"`
	StepKey       string `json:"step_key"`
	Repo          string `json:"repo"`
	Number        int64  `json:"number"`
	Head          string `json:"head"`
	RuleKey       string `json:"rule_key"`
}

func (p policyGatePayload) chat(workspaceID string) ChatRef {
	return ChatRef{WorkspaceID: workspaceID, IntegrationID: p.IntegrationID, ChatID: p.ChatID, ThreadID: p.ThreadID}
}

// PolicyGates sends policy gate messages and handles the owner's presses.
type PolicyGates struct {
	Store     PolicyGateStore
	Messenger Messenger
	// Waker wakes the step waiting on a decided gate; nil leaves it to the
	// worker task sweeper (ListOrchestratorStepsToWake).
	Waker LocalTaskWaker
	// OwnerTelegramUserID is the owner's Telegram user id (the only sender
	// the inbound handler accepts in a group chat); 0 means the owner chat
	// is a private chat, whose id is the owner's user id.
	OwnerTelegramUserID int64
	RequestTimeout      time.Duration
	// SweepInterval is how often Run closes the gates of ended executions;
	// 0 means one minute.
	SweepInterval time.Duration
	Now           func() time.Time
}

func (g *PolicyGates) now() time.Time {
	if g != nil && g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *PolicyGates) timeout() time.Duration {
	if g == nil || g.RequestTimeout <= 0 {
		return 10 * time.Second
	}
	return g.RequestTimeout
}

// shortSHA is the first 7 hex digits of a commit SHA.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func policyGateSubject(p policyGatePayload) string {
	return fmt.Sprintf("merge PR %s#%d (head %s)", p.Repo, p.Number, p.Head)
}

func policyGateAskText(p policyGatePayload, expires time.Time) string {
	rule := p.RuleKey
	if rule == "" {
		rule = "?"
	}
	return "🔐 Cần duyệt: " + policyGateSubject(p) + "\n" +
		"Quy tắc yêu cầu duyệt: " + rule + "\n" +
		"Hết hạn: " + expires.UTC().Format("15:04 02/01 UTC") + "\n" +
		"Chỉ merge đúng head này, sau khi kiểm tra lại."
}

func policyGateDecidedText(p policyGatePayload, d Decision) string {
	switch d {
	case DecisionApproved:
		return "✅ Đã duyệt: " + policyGateSubject(p) + ". Đang kiểm tra lại rồi merge."
	case DecisionRejected:
		return "❌ Đã từ chối: " + policyGateSubject(p) + ". Không merge."
	}
	return "⌛ Hết hạn: " + policyGateSubject(p) + ". Không merge."
}

// Ask sends the gate's message with "Duyệt" / "Từ chối" to chat and stores
// where it went. It returns false when the message was not sent.
func (g *PolicyGates) Ask(ctx context.Context, chat ChatRef, gate policy.Gate, p policyGatePayload) bool {
	if g == nil || g.Messenger == nil || g.Store == nil {
		return false
	}
	approve, err1 := (CallbackData{Kind: CallbackApprove, Nonce: gate.Nonce, Option: -1}).Encode()
	reject, err2 := (CallbackData{Kind: CallbackReject, Nonce: gate.Nonce, Option: -1}).Encode()
	if err1 != nil || err2 != nil {
		log.Print("policy gate: buttons not encoded")
		return false
	}
	p.IntegrationID, p.ChatID, p.ThreadID = chat.IntegrationID, chat.ChatID, chat.ThreadID
	sctx, cancel := context.WithTimeout(ctx, g.timeout())
	id, err := g.Messenger.SendMessage(sctx, chat, OutMessage{
		Text:    capRunes(policyGateAskText(p, gate.ExpiresAt), maxTelegramRunes),
		Buttons: [][]Button{{{Text: "Duyệt", Data: approve}, {Text: "Từ chối", Data: reject}}},
	})
	cancel()
	if err != nil {
		log.Printf("policy gate %d: approval message not sent", gate.ID)
		return false
	}
	raw, _ := json.Marshal(p)
	if err := g.Store.SetPolicyGateMessage(ctx, chat.WorkspaceID, gate.ID, id, raw); err != nil {
		log.Printf("policy gate %d: message id not saved", gate.ID)
	}
	return true
}

// finalEdit replaces the gate message's text with its outcome and removes
// the buttons. Best effort; it reports whether the buttons are gone.
func (g *PolicyGates) finalEdit(ctx context.Context, workspaceID string, rec PolicyGateRecord, d Decision) bool {
	if g == nil || g.Messenger == nil || rec.MessageID <= 0 {
		return false
	}
	var p policyGatePayload
	if json.Unmarshal(rec.Payload, &p) != nil || p.ChatID == 0 {
		return false
	}
	chat := p.chat(workspaceID)
	ectx, cancel := context.WithTimeout(ctx, g.timeout())
	defer cancel()
	if ed, ok := g.Messenger.(MessageEditor); ok {
		if err := ed.EditMessageText(ectx, chat, rec.MessageID, policyGateDecidedText(p, d)); err == nil {
			return true
		}
	}
	return g.Messenger.RemoveButtons(ectx, chat, rec.MessageID) == nil
}

// HandlePress decides a policy gate from the owner's press. Callbacks reach
// the inbox only from the owner (the inbound handler's owner rule); the
// decision also needs the workspace's owner member, recorded as the
// deciding user. A press in another chat, a second press, a press on an
// old message or after expiry changes nothing.
func (g *PolicyGates) HandlePress(ctx context.Context, u InboundUpdate, msgID int64, data CallbackData, a Approval) (string, string, bool) {
	if g == nil || g.Store == nil {
		return msgNotAvailable, "not_available", false
	}
	if a.Kind != ApprovalPolicyGate || a.WorkspaceID != u.WorkspaceID || a.Decision != "" || !g.now().Before(a.ExpiresAt) {
		return msgStale, "stale", true
	}
	if data.Kind != CallbackApprove && data.Kind != CallbackReject {
		return msgNotAvailable, "not_available", false
	}
	rec, err := g.Store.PolicyGate(ctx, u.WorkspaceID, a.ID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return msgStale, "stale", true
		}
		return "", "retry", false
	}
	var p policyGatePayload
	if json.Unmarshal(rec.Payload, &p) != nil || p.ChatID != u.ChatID || (rec.MessageID != 0 && rec.MessageID != msgID) || rec.ExecutionID <= 0 {
		return msgStale, "stale", true
	}
	owner, err := g.Store.WorkspaceOwner(ctx, u.WorkspaceID)
	if errors.Is(err, ErrNotFound) {
		return "Không có chủ workspace để duyệt.", "no_owner_member", false
	}
	if err != nil {
		return "", "retry", false
	}
	tgID := g.OwnerTelegramUserID
	if tgID <= 0 {
		tgID = u.ChatID
	}
	decision, toast := DecisionApproved, "Đã duyệt."
	if data.Kind == CallbackReject {
		decision, toast = DecisionRejected, "Đã từ chối."
	}
	decided, err := g.Store.DecidePolicyGate(ctx, u.WorkspaceID, a.Nonce, decision, "telegram:"+strconv.FormatInt(tgID, 10), owner, g.now())
	switch {
	case errors.Is(err, ErrExpired):
		// Past its expiry, or its execution has ended: the press is stale
		// and the message is closed (⌛, no buttons); the sweep records it.
		return msgStale, "stale", !g.finalEdit(ctx, u.WorkspaceID, rec, DecisionExpired)
	case errors.Is(err, ErrDecided), errors.Is(err, ErrNotFound):
		return msgStale, "stale", true
	case err != nil:
		return "", "retry", false
	}
	log.Printf("policy gate %d: %s for %s", decided.ID, decision, workflow.FormatRef(rec.ExecutionID))
	removed := g.finalEdit(ctx, u.WorkspaceID, rec, decision)
	if g.Waker != nil && p.StepKey != "" {
		if err := g.Waker.WakeWorkerStep(ctx, u.WorkspaceID, rec.ExecutionID, p.StepKey); err != nil && !errors.Is(err, workflow.ErrNotEligible) {
			log.Print("policy gate: waiting step not woken; the sweeper retries")
		}
	}
	return toast, "", !removed
}

// closeExpired edits the message of a gate expired by the store (⌛, no
// buttons). Best effort.
func (g *PolicyGates) closeExpired(ctx context.Context, workspaceID string, id int64) {
	if g == nil || g.Store == nil || id <= 0 {
		return
	}
	rec, err := g.Store.PolicyGate(ctx, workspaceID, id)
	if err != nil {
		return
	}
	g.finalEdit(ctx, workspaceID, rec, DecisionExpired)
}

// Run closes the gates of ended executions every SweepInterval until ctx
// ends.
func (g *PolicyGates) Run(ctx context.Context) error {
	interval := time.Minute
	if g != nil && g.SweepInterval > 0 {
		interval = g.SweepInterval
	}
	for ctx.Err() == nil {
		if _, err := g.Sweep(ctx); err != nil && ctx.Err() == nil {
			log.Print("policy gates: sweep failed; retried next period")
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
	return nil
}

// Sweep expires the open gates of executions that completed, failed, were
// cancelled or are past their deadline, and closes their messages (⌛
// Hết hạn, buttons removed; review B R-B-1). Their waiting step never runs
// again, so nothing else would close them. It returns how many expired.
func (g *PolicyGates) Sweep(ctx context.Context) (int, error) {
	if g == nil || g.Store == nil {
		return 0, nil
	}
	es, ok := g.Store.(PolicyGateEndStore)
	if !ok {
		return 0, nil
	}
	const batch = 50
	total := 0
	for {
		list, err := es.ExpireEndedExecutionGates(ctx, g.now(), batch)
		if err != nil {
			return total, err
		}
		for _, rec := range list {
			log.Printf("policy gate %d: expired, %s ended", rec.ID, workflow.FormatRef(rec.ExecutionID))
			g.finalEdit(ctx, rec.WorkspaceID, rec, DecisionExpired)
		}
		total += len(list)
		if len(list) < batch {
			return total, nil
		}
	}
}

// gateWaitRef is the executor's wait reference of a policy gate.
func gateWaitRef(id int64) string { return gateWaitPrefix + strconv.FormatInt(id, 10) }

// parseGateWaitRef returns the gate id of a "gate:<id>" reference.
func parseGateWaitRef(ref string) (int64, bool) {
	if !strings.HasPrefix(ref, gateWaitPrefix) {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(ref, gateWaitPrefix), 10, 64)
	return id, err == nil && id > 0
}

// gateWaitPrefix is the executor's wait reference prefix of a policy gate.
const gateWaitPrefix = "gate:"
