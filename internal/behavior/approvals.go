package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/descblock"
)

// This file is the owner's side of the action library: button presses
// (post a draft, answer an ask, confirm a change, skip, rate), free-text
// answers, the expiry sweep and /undo.

// --- CallbackHandler ---

// HandleCallback acts on one button press. Deciding presses go through
// ApprovalStore.Decide, which takes a decision once and only while the
// approval is open and unexpired; a second press or a late one returns
// ErrDecided or ErrExpired and changes nothing.
func (e *Engine) HandleCallback(ctx context.Context, a CallbackAction) (CallbackResult, error) {
	ap := a.Approval
	switch a.Data.Kind {
	case CallbackRateUp, CallbackRateDown:
		return e.rate(ctx, a)
	case CallbackFreeText:
		if ap.Kind != ApprovalItemAnswer {
			return CallbackResult{}, ErrNotAvailable
		}
		return e.askFreeText(ctx, a)
	case CallbackReject:
		return e.reject(ctx, a)
	case CallbackOption:
		if ap.Kind != ApprovalItemAnswer {
			return CallbackResult{}, ErrNotAvailable
		}
		var options []string
		if json.Unmarshal(ap.Options, &options) != nil || a.Data.Option < 0 || a.Data.Option >= len(options) {
			return CallbackResult{}, ErrNotAvailable
		}
		reply, ok, err := e.answer(ctx, ap, chatOf(a.WorkspaceID, a.IntegrationID, a.ChatID), options[a.Data.Option], a.Actor, false)
		if err != nil {
			return CallbackResult{}, err
		}
		return result(reply, ok), nil
	case CallbackApprove:
		chat := chatOf(a.WorkspaceID, a.IntegrationID, a.ChatID)
		switch ap.Kind {
		case ApprovalPostDraft:
			reply, ok, err := e.post(ctx, ap, chat, a.Actor)
			if err != nil {
				return CallbackResult{}, err
			}
			return result(reply, ok), nil
		case ApprovalConfirmAction:
			reply, err := e.confirm(ctx, ap, a.Actor)
			if err != nil {
				return CallbackResult{}, err
			}
			return CallbackResult{Toast: capRunes(reply, 180), RemoveButtons: true, Reply: reply}, nil
		}
	}
	return CallbackResult{}, ErrNotAvailable
}

// result is the press result: a toast, and the same text as a message
// when the outcome is not the plain success (a conflict or a failure).
func result(reply string, ok bool) CallbackResult {
	r := CallbackResult{Toast: capRunes(reply, 180), RemoveButtons: true}
	if !ok {
		r.Reply = reply
	}
	return r
}

func chatOf(ws, integration string, chat int64) ChatRef {
	return ChatRef{WorkspaceID: ws, IntegrationID: integration, ChatID: chat}
}

// HandleFreeText takes the owner's reply to a message in free-text mode:
// an ask answer, or the reason for a 👎.
func (e *Engine) HandleFreeText(ctx context.Context, a FreeTextAnswer) (string, error) {
	ap := a.Approval
	text := strings.TrimSpace(a.Answer)
	if text == "" {
		return "", ErrNotAvailable
	}
	chat := chatOf(a.WorkspaceID, a.IntegrationID, a.ChatID)
	switch ap.Kind {
	case ApprovalItemAnswer:
		reply, _, err := e.answer(ctx, ap, chat, text, a.Actor, true)
		return reply, err
	case ApprovalFeedbackReason:
		rc, err := e.loadRun(ctx, a.WorkspaceID, ap.RunID)
		if err != nil {
			return "", err
		}
		reason := capRunes(e.d.Redactor.RedactSecrets(text), 999)
		if _, err := e.d.Store.Decide(ctx, a.WorkspaceID, ap.Nonce, DecisionAnswered, reason, a.Actor, e.now()); err != nil {
			return "", err
		}
		if err := e.d.Store.RateItem(ctx, a.WorkspaceID, ap.ItemID, RatingDown, reason); err != nil {
			return "", err
		}
		log.Printf("behavior run %d item %d corr=%s: rated down with reason", ap.RunID, ap.ItemID, rc.Run.CorrelationID)
		return textsFor(rc.Doc.Language).reasonSaved(), nil
	}
	return "", ErrNotAvailable
}

// rate stores 👍 or 👎 on the item; 👎 then waits for an optional reason.
func (e *Engine) rate(ctx context.Context, a CallbackAction) (CallbackResult, error) {
	ap := a.Approval
	if ap.ItemID == 0 || ap.RunID == 0 {
		return CallbackResult{Toast: textsFor("").notApplicable()}, nil
	}
	rc, err := e.loadRun(ctx, a.WorkspaceID, ap.RunID)
	if err != nil {
		return CallbackResult{}, err
	}
	t := textsFor(rc.Doc.Language)
	if a.Data.Kind == CallbackRateUp {
		if err := e.d.Store.RateItem(ctx, a.WorkspaceID, ap.ItemID, RatingUp, ""); err != nil {
			return CallbackResult{}, err
		}
		log.Printf("behavior run %d item %d corr=%s: rated up", ap.RunID, ap.ItemID, rc.Run.CorrelationID)
		return CallbackResult{Toast: t.thanks()}, nil
	}
	if err := e.d.Store.RateItem(ctx, a.WorkspaceID, ap.ItemID, RatingDown, ""); err != nil {
		return CallbackResult{}, err
	}
	log.Printf("behavior run %d item %d corr=%s: rated down", ap.RunID, ap.ItemID, rc.Run.CorrelationID)
	fb, err := e.ensureApproval(ctx, a.WorkspaceID, NewApproval{
		Kind: ApprovalFeedbackReason, RunID: ap.RunID, ItemID: ap.ItemID, Action: "feedback", Resource: rc.Run.Subject,
		ExpiresAt: e.now().Add(e.s.FeedbackExpiry),
	}, true)
	if err != nil {
		return CallbackResult{Toast: t.thanks()}, nil
	}
	chat := chatOf(a.WorkspaceID, a.IntegrationID, a.ChatID)
	if id, err := e.prompt(ctx, chat, a.MessageID, t.reasonPrompt()); err == nil {
		_ = e.d.Store.SetReplyMessage(ctx, a.WorkspaceID, fb.ID, id)
	}
	return CallbackResult{Toast: t.thanks()}, nil
}

// prompt sends a message that opens a reply box and returns its ID.
func (e *Engine) prompt(ctx context.Context, chat ChatRef, replyTo int64, text string) (int64, error) {
	sctx, cancel := context.WithTimeout(ctx, e.s.RequestTimeout)
	defer cancel()
	return e.d.Messenger.SendMessage(sctx, chat, OutMessage{Text: text, ReplyTo: replyTo, ForceReply: true})
}

// askFreeText puts an ask in free-text mode: a reply to the prompt it
// sends becomes the answer. The approval stays open.
func (e *Engine) askFreeText(ctx context.Context, a CallbackAction) (CallbackResult, error) {
	ap := a.Approval
	rc, err := e.loadRun(ctx, a.WorkspaceID, ap.RunID)
	if err != nil {
		return CallbackResult{}, err
	}
	t := textsFor(rc.Doc.Language)
	id, err := e.prompt(ctx, chatOf(a.WorkspaceID, a.IntegrationID, a.ChatID), a.MessageID, t.replyPrompt())
	if err != nil {
		return CallbackResult{}, err
	}
	if err := e.d.Store.SetReplyMessage(ctx, a.WorkspaceID, ap.ID, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			return CallbackResult{}, ErrDecided
		}
		return CallbackResult{}, err
	}
	return CallbackResult{Toast: t.replyPromptToast()}, nil
}

// reject skips a draft or an ask, or cancels a confirmation.
func (e *Engine) reject(ctx context.Context, a CallbackAction) (CallbackResult, error) {
	ap := a.Approval
	switch ap.Kind {
	case ApprovalPostDraft, ApprovalItemAnswer, ApprovalConfirmAction:
	default:
		return CallbackResult{}, ErrNotAvailable
	}
	rc, err := e.loadRun(ctx, a.WorkspaceID, ap.RunID)
	if err != nil {
		return CallbackResult{}, err
	}
	t := textsFor(rc.Doc.Language)
	if _, err := e.d.Store.Decide(ctx, a.WorkspaceID, ap.Nonce, DecisionRejected, "", a.Actor, e.now()); err != nil {
		return CallbackResult{}, err
	}
	switch ap.Kind {
	case ApprovalPostDraft:
		_, err = e.d.Store.TransitionItem(ctx, a.WorkspaceID, ap.ItemID, []ItemStatus{ItemDrafted}, ItemSkipped, nil)
	case ApprovalItemAnswer:
		_, err = e.d.Store.TransitionItem(ctx, a.WorkspaceID, ap.ItemID, []ItemStatus{ItemAsked, ItemDrafted}, ItemSkipped, nil)
	case ApprovalConfirmAction:
		err = e.updateChanges(ctx, rc, ap, func(c change) actionOutcome {
			return actionOutcome{Index: c.Index, Type: c.Type, Status: OutcomeCanceled}
		})
		log.Printf("behavior run %d item %d corr=%s: confirmation canceled", ap.RunID, ap.ItemID, rc.Run.CorrelationID)
		if err != nil && !errors.Is(err, ErrConflict) {
			return CallbackResult{}, err
		}
		return CallbackResult{Toast: t.canceled(), RemoveButtons: true}, nil
	}
	if err != nil && !errors.Is(err, ErrConflict) {
		return CallbackResult{}, err
	}
	log.Printf("behavior run %d item %d corr=%s: skipped", ap.RunID, ap.ItemID, rc.Run.CorrelationID)
	return CallbackResult{Toast: t.skipped(), RemoveButtons: true}, nil
}

// decisionScope is where a remembered decision of the run applies: the
// behavior, the trigger's ClickUp list (when known) and the GitHub repos
// the run read.
func decisionScope(rc RunContext) DecisionScope {
	return DecisionScope{Behavior: rc.Doc.Name, List: rc.Trigger.ListID, Repo: capRunes(strings.Join(contextRepos(rc.Context), ","), 300)}
}

func (e *Engine) remember(ctx context.Context, rc RunContext, it Item, answer string, src DecisionSource) {
	question := strings.TrimSpace(it.Text)
	answer = strings.TrimSpace(answer)
	if question == "" || answer == "" {
		return
	}
	err := e.d.Decisions.RecordDecision(ctx, rc.Run.WorkspaceID, NewDecision{
		Topic: capRunes(it.Topic, 199), Question: capRunes(question, 3999), Answer: capRunes(answer, 3999),
		Scope: decisionScope(rc), Source: src, RunID: rc.Run.ID, ItemID: it.ID,
	})
	if err != nil {
		log.Printf("behavior run %d item %d corr=%s: decision not remembered", rc.Run.ID, it.ID, rc.Run.CorrelationID)
	}
}

// outcomeReply is the owner's reply after an item's actions ran.
func outcomeReply(t texts, rc RunContext, status ItemStatus, result json.RawMessage, ok string) string {
	switch status {
	case ItemConflict:
		return t.conflict(rc.Run.ID)
	case ItemFailed:
		code := "failed"
		for _, o := range decodeResult(result).Actions {
			if o.Code != "" {
				code = o.Code
			}
		}
		return t.writeFailed(code, rc.Run.ID)
	}
	return ok
}

// answer takes an ask's answer once, runs the behavior's ask actions with
// it and remembers the decision.
func (e *Engine) answer(ctx context.Context, ap Approval, chat ChatRef, text, actor string, removeButtons bool) (string, bool, error) {
	rc, err := e.loadRun(ctx, ap.WorkspaceID, ap.RunID)
	if err != nil {
		return "", false, err
	}
	text = capRunes(strings.TrimSpace(text), 3999)
	if _, err := e.d.Store.Decide(ctx, ap.WorkspaceID, ap.Nonce, DecisionAnswered, text, actor, e.now()); err != nil {
		return "", false, err
	}
	if removeButtons && ap.TelegramMessageID != 0 {
		e.removeButtons(ctx, rc.Run, chat, ap.TelegramMessageID)
	}
	it, err := e.d.Store.GetItem(ctx, ap.WorkspaceID, ap.ItemID)
	if err != nil {
		return "", false, err
	}
	t := textsFor(rc.Doc.Language)
	status, result := e.runDecided(ctx, rc, chat, it, ClassAsk, text)
	if _, err := e.d.Store.TransitionItem(ctx, ap.WorkspaceID, it.ID, []ItemStatus{ItemAsked, ItemDrafted}, status, result); err != nil && !errors.Is(err, ErrConflict) {
		return "", false, err
	}
	log.Printf("behavior run %d item %d corr=%s: answered (%s)", rc.Run.ID, it.ID, rc.Run.CorrelationID, status)
	e.remember(ctx, rc, it, text, SourceAskAnswer)
	return outcomeReply(t, rc, status, result, t.answered()), status == ItemAnswered, nil
}

// runDecided runs a class's actions after the owner decided: a failure is
// final (the decision is already taken) and ends the item as failed.
func (e *Engine) runDecided(ctx context.Context, rc RunContext, chat ChatRef, it Item, class Class, text string) (ItemStatus, json.RawMessage) {
	var env writeEnv
	if needsClickUp(rc.Doc, []Item{it}) {
		var err error
		if env, err = e.env(ctx, rc); err != nil {
			r := itemResult{Actions: []actionOutcome{{Index: 0, Type: "policy", Status: OutcomeError, Code: CodePolicyUnavailable}}}
			return ItemFailed, r.encode()
		}
	}
	status, result, err := e.execute(ctx, rc, env, chat, it, class, text, false)
	if err != nil {
		r := itemResult{Actions: []actionOutcome{{Index: 0, Type: "engine", Status: OutcomeError, Code: "storage"}}}
		return ItemFailed, r.encode()
	}
	if status == ItemAnswered && class != ClassAsk {
		status = ItemDone
	}
	return status, result
}

// post publishes a draft once: revalidated (open, unexpired; the target
// is checked before every write), then the auto actions run.
func (e *Engine) post(ctx context.Context, ap Approval, chat ChatRef, actor string) (string, bool, error) {
	rc, err := e.loadRun(ctx, ap.WorkspaceID, ap.RunID)
	if err != nil {
		return "", false, err
	}
	if _, err := e.d.Store.Decide(ctx, ap.WorkspaceID, ap.Nonce, DecisionApproved, "", actor, e.now()); err != nil {
		return "", false, err
	}
	it, err := e.d.Store.GetItem(ctx, ap.WorkspaceID, ap.ItemID)
	if err != nil {
		return "", false, err
	}
	t := textsFor(rc.Doc.Language)
	answer := answerOf(it)
	status, result := e.runDecided(ctx, rc, chat, it, ClassAuto, answer)
	if _, err := e.d.Store.TransitionItem(ctx, ap.WorkspaceID, it.ID, []ItemStatus{ItemDrafted}, status, result); err != nil && !errors.Is(err, ErrConflict) {
		return "", false, err
	}
	log.Printf("behavior run %d item %d corr=%s: draft posted (%s)", rc.Run.ID, it.ID, rc.Run.CorrelationID, status)
	if status == ItemDone {
		e.remember(ctx, rc, it, answer, SourceAutoPosted)
	}
	return outcomeReply(t, rc, status, result, t.posted()), status == ItemDone, nil
}

// confirm applies a confirmed change once, after a fresh read: a change
// already made, or a target that moved on, is left alone.
func (e *Engine) confirm(ctx context.Context, ap Approval, actor string) (string, error) {
	rc, err := e.loadRun(ctx, ap.WorkspaceID, ap.RunID)
	if err != nil {
		return "", err
	}
	if _, err := e.d.Store.Decide(ctx, ap.WorkspaceID, ap.Nonce, DecisionApproved, "", actor, e.now()); err != nil {
		return "", err
	}
	t := textsFor(rc.Doc.Language)
	var p confirmPayload
	if json.Unmarshal(ap.Payload, &p) != nil || p.TaskID == "" {
		return t.writeFailed("invalid_payload", rc.Run.ID), nil
	}
	env, err := e.env(ctx, rc)
	if err != nil {
		return t.writeFailed(CodePolicyUnavailable, rc.Run.ID), nil
	}
	env.taskID = p.TaskID
	st, err := e.d.Tasks.ReadTask(ctx, ap.WorkspaceID, env.tokenRef, p.TaskID)
	if err != nil {
		code := toolCode(err)
		_ = e.updateChanges(ctx, rc, ap, func(c change) actionOutcome {
			return actionOutcome{Index: c.Index, Type: c.Type, Status: OutcomeError, Code: code}
		})
		return t.writeFailed(code, rc.Run.ID), nil
	}
	var replies []string
	err = e.updateChanges(ctx, rc, ap, func(c change) actionOutcome {
		o := actionOutcome{Index: c.Index, Type: c.Type, Status: OutcomeOK}
		ok := false
		switch c.Type {
		case ActionSetStatus:
			switch {
			case strings.EqualFold(st.Status, c.To):
				o.Status = OutcomeAlready
				replies = append(replies, t.statusAlready(c.To))
				return o
			case !strings.EqualFold(st.Status, c.From):
				o.Status = OutcomeChanged
				replies = append(replies, t.statusMoved(st.Status))
				return o
			}
			out := e.callWrite(ctx, rc, env, actionScope(ap.ItemID, c.Index), ToolSetStatus,
				map[string]any{"task_id": p.TaskID, "status": c.To, "if_status": c.From})
			o, _ = outcomeOf(out, c.Index, c.Type)
			ok = o.Status == OutcomeOK
			if ok {
				replies = append(replies, t.statusDone(c.From, c.To))
			}
		case ActionUnassign:
			if !slices.Contains(st.AssigneeIDs, c.User) {
				o.Status = OutcomeAlready
				replies = append(replies, t.unassignAlready(c.User))
				return o
			}
			out := e.callWrite(ctx, rc, env, actionScope(ap.ItemID, c.Index), ToolRemoveAssignee,
				map[string]any{"task_id": p.TaskID, "user_id": c.User})
			o, _ = outcomeOf(out, c.Index, c.Type)
			ok = o.Status == OutcomeOK
			if ok {
				replies = append(replies, t.unassignDone(c.User))
			}
		}
		if !ok {
			if o.Status == OutcomeConflict {
				replies = append(replies, t.conflict(rc.Run.ID))
			} else {
				replies = append(replies, t.writeFailed(firstNonEmpty(o.Code, o.Status), rc.Run.ID))
			}
		}
		return o
	})
	if err != nil && !errors.Is(err, ErrConflict) {
		log.Printf("behavior run %d item %d corr=%s: confirmation result not stored", rc.Run.ID, ap.ItemID, rc.Run.CorrelationID)
	}
	log.Printf("behavior run %d item %d corr=%s: confirmation applied (%d changes)", rc.Run.ID, ap.ItemID, rc.Run.CorrelationID, len(p.Changes))
	return strings.Join(replies, "\n"), nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// updateChanges records an outcome for every change of a confirmation in
// its item's action result (the item keeps its status).
func (e *Engine) updateChanges(ctx context.Context, rc RunContext, ap Approval, fn func(change) actionOutcome) error {
	var p confirmPayload
	_ = json.Unmarshal(ap.Payload, &p)
	outcomes := make([]actionOutcome, 0, len(p.Changes))
	for _, c := range p.Changes {
		outcomes = append(outcomes, fn(c))
	}
	it, err := e.d.Store.GetItem(ctx, rc.Run.WorkspaceID, ap.ItemID)
	if err != nil {
		return err
	}
	res := decodeResult(it.ActionResult)
	for _, o := range outcomes {
		res.set(o)
	}
	_, err = e.d.Store.TransitionItem(ctx, rc.Run.WorkspaceID, it.ID, []ItemStatus{it.Status}, it.Status, res.encode())
	return err
}

func (e *Engine) removeButtons(ctx context.Context, run Run, chat ChatRef, messageID int64) {
	rctx, cancel := context.WithTimeout(ctx, e.s.RequestTimeout)
	defer cancel()
	if err := e.d.Messenger.RemoveButtons(rctx, chat, messageID); err != nil {
		log.Printf("behavior run %d corr=%s: buttons of message %d not removed (%s)", run.ID, run.CorrelationID, messageID, sendCode(err))
	}
}

// --- expiry sweep ---

// Run sweeps expired approvals and sends due digest batches until ctx
// ends.
func (e *Engine) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		if _, err := e.Sweep(ctx); err != nil && ctx.Err() == nil {
			log.Print("behavior approvals: expiry sweep failed; retried next period")
		}
		if _, err := e.FlushDigests(ctx); err != nil && ctx.Err() == nil {
			log.Print("behavior digests: flush failed; retried next period")
		}
		t := time.NewTimer(e.s.SweepInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
	return nil
}

// Sweep marks overdue open approvals expired, moves their waiting items to
// expired, and removes their buttons (best effort). It returns how many
// approvals expired.
func (e *Engine) Sweep(ctx context.Context) (int, error) {
	const batch = 50
	total := 0
	for {
		list, err := e.d.Store.ExpireDue(ctx, e.now(), batch)
		if err != nil {
			return total, err
		}
		for _, a := range list {
			e.expired(ctx, a)
		}
		total += len(list)
		if len(list) < batch {
			return total, nil
		}
	}
}

func (e *Engine) expired(ctx context.Context, a Approval) {
	// The run gives the log lines their correlation ID; an approval without
	// a readable run still expires.
	run := Run{ID: a.RunID}
	if a.RunID != 0 {
		if r, gerr := e.d.Store.GetRun(ctx, a.WorkspaceID, a.RunID); gerr == nil {
			run = r
		}
	}
	var err error
	switch a.Kind {
	case ApprovalItemAnswer:
		_, err = e.d.Store.TransitionItem(ctx, a.WorkspaceID, a.ItemID, []ItemStatus{ItemAsked, ItemDrafted}, ItemExpired, nil)
	case ApprovalPostDraft:
		_, err = e.d.Store.TransitionItem(ctx, a.WorkspaceID, a.ItemID, []ItemStatus{ItemDrafted}, ItemExpired, nil)
	case ApprovalConfirmAction:
		if it, gerr := e.d.Store.GetItem(ctx, a.WorkspaceID, a.ItemID); gerr == nil {
			res := decodeResult(it.ActionResult)
			var p confirmPayload
			_ = json.Unmarshal(a.Payload, &p)
			for _, c := range p.Changes {
				res.set(actionOutcome{Index: c.Index, Type: c.Type, Status: OutcomeExpired})
			}
			_, err = e.d.Store.TransitionItem(ctx, a.WorkspaceID, it.ID, []ItemStatus{it.Status}, it.Status, res.encode())
		}
	}
	if err != nil && !errors.Is(err, ErrConflict) {
		log.Printf("behavior run %d item %d corr=%s: approval %d expired, item not updated", run.ID, a.ItemID, run.CorrelationID, a.ID)
	} else {
		log.Printf("behavior run %d item %d corr=%s: approval %d expired", run.ID, a.ItemID, run.CorrelationID, a.ID)
	}
	if a.TelegramMessageID == 0 || a.Kind == ApprovalFeedback || a.Kind == ApprovalFeedbackReason {
		return
	}
	chat, err := e.d.Chats.OwnerChat(ctx, a.WorkspaceID, TriggerData{})
	if err != nil {
		return
	}
	e.removeButtons(ctx, run, chat, a.TelegramMessageID)
}

// --- Undoer ---

// Undo puts back the description blocks a run wrote, but only when the
// task's description is still exactly what Nexus last wrote for the run;
// text outside the blocks is never touched. A run with run-level actions
// (actions.run) is undone by undoRunActions instead.
func (e *Engine) Undo(ctx context.Context, r UndoRequest) (string, error) {
	rc, err := e.loadRun(ctx, r.WorkspaceID, r.RunID)
	if err != nil {
		return "", err
	}
	t := textsFor(rc.Doc.Language)
	if rc.Run.UndoneAt != nil {
		return t.undoAlready(), nil
	}
	if len(rc.Run.RunActions) > 0 || len(rc.Doc.RunActions) > 0 {
		return e.undoRunActions(ctx, rc, r, t)
	}
	snap := decodeSnapshot(rc.Run.Snapshot)
	items, err := e.d.Store.ListItems(ctx, r.WorkspaceID, r.RunID)
	if err != nil {
		return "", err
	}
	var last actionOutcome
	var blocks []string
	for _, it := range items {
		for _, o := range decodeResult(it.ActionResult).Actions {
			if o.Type != ActionDescriptionBlock || o.Status != OutcomeOK || o.DescSHA == "" {
				continue
			}
			if o.At > last.At {
				last = o
			}
			if !slices.Contains(blocks, o.Block) {
				blocks = append(blocks, o.Block)
			}
		}
	}
	if snap == nil || last.DescSHA == "" || len(blocks) == 0 {
		return t.undoNothing(), nil
	}
	env, err := e.env(ctx, rc)
	if err != nil {
		return t.undoFailed(CodePolicyUnavailable), nil
	}
	env.taskID = snap.TaskID
	st, err := e.d.Tasks.ReadTask(ctx, r.WorkspaceID, env.tokenRef, snap.TaskID)
	if err != nil {
		return t.undoFailed(toolCode(err)), nil
	}
	if descblock.Hash(st.Markdown) != last.DescSHA {
		return t.undoChanged(), nil
	}
	slices.Sort(blocks)
	guard := last.DescSHA
	for i, block := range blocks {
		input := map[string]any{"task_id": snap.TaskID, "block": block, "if_current_sha256": guard}
		body, found, ferr := descblock.Find(snap.DescriptionBefore, block)
		if ferr == nil && found {
			input["body"] = body
		} else {
			input["remove"] = true
		}
		out := e.callWrite(ctx, rc, env, fmt.Sprintf("behavior_undo:%d:%d", r.RunID, i), ToolSetDescriptionBlock, input)
		o, _ := outcomeOf(out, i, ActionDescriptionBlock)
		if o.Status != OutcomeOK {
			if o.Status == OutcomeConflict {
				return t.undoChanged(), nil
			}
			return t.undoFailed(firstNonEmpty(o.Code, o.Status)), nil
		}
		m := descHashRe.FindStringSubmatch(out.Content)
		if m == nil {
			return t.undoFailed("invalid_response"), nil
		}
		guard = m[1]
	}
	if err := e.d.Store.MarkUndone(ctx, r.WorkspaceID, r.RunID); err != nil && !errors.Is(err, ErrConflict) {
		return "", err
	}
	log.Printf("behavior run %d corr=%s req=%s: undone by owner (%d blocks)", rc.Run.ID, rc.Run.CorrelationID, r.CorrelationID, len(blocks))
	for _, it := range items {
		if it.Class == ClassAuto && it.Status == ItemDone {
			e.remember(ctx, rc, it, answerOf(it), SourceAutoUndone)
		}
	}
	return t.undone(r.RunID), nil
}
