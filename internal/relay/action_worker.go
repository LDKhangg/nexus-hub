package relay

import (
	"context"
	"errors"
	"log"
	"strconv"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// MaxActionAttempts matches the notice outbox.
const MaxActionAttempts = 8

var ErrStaleActionClaim = errors.New("stale action claim")

// OutboundAction is a claimed one-way Telegram message.
type OutboundAction struct {
	ID                         int64
	ClaimToken                 string
	Attempts                   int
	WorkspaceID, IntegrationID string
	Text                       string
	ExecutionID                int64
}

// ActionStore is the outbound_actions queue (storage.ActionStore).
type ActionStore interface {
	Claim(context.Context, time.Time) (*OutboundAction, error)
	ResolveTarget(context.Context, OutboundAction) (tenant.TelegramTargetConfig, error)
	MarkSent(context.Context, OutboundAction, int64) error
	MarkRetry(context.Context, OutboundAction, time.Time, string) error
	MarkFailed(context.Context, OutboundAction, string) error
}

// TextSender sends plain text (telegram.Client).
type TextSender interface {
	SendText(ctx context.Context, workspaceID, tokenRef, chatID string, threadID *int64, text string) (int64, error)
}

// ThreadRouter picks the Telegram topic of an execution's report. Nil
// keeps the integration's static message_thread_id behavior.
type ThreadRouter interface {
	// ActionThread returns the topic thread for the action, or 0 (General).
	ActionThread(ctx context.Context, workspaceID, integrationID string, chatID int64, executionID int64) int64
	// ActionThreadRejected handles a send the topic made fail: it returns
	// the thread to resend to once (0: General) and whether to resend.
	ActionThreadRejected(ctx context.Context, workspaceID, integrationID string, chatID, threadID int64, err error) (int64, bool)
}

// ActionWorker delivers outbound actions with the notice retry policy.
type ActionWorker struct {
	store    ActionStore
	sender   TextSender
	settings WorkerSettings
	now      func() time.Time

	conversations workflow.ConversationStore
	threads       ThreadRouter
}

// SetThreadRouter routes reports of executions to topics. An integration
// with a fixed message_thread_id keeps it.
func (w *ActionWorker) SetThreadRouter(t ThreadRouter) { w.threads = t }

func NewActionWorker(store ActionStore, sender TextSender, settings WorkerSettings, conversations ...workflow.ConversationStore) *ActionWorker {
	if settings.PollInterval <= 0 {
		settings.PollInterval = time.Second
	}
	if settings.RequestTimeout <= 0 {
		settings.RequestTimeout = 10 * time.Second
	}
	w := &ActionWorker{store: store, sender: sender, settings: settings, now: time.Now}
	if len(conversations) > 0 {
		w.conversations = conversations[0]
	}
	return w
}

func (w *ActionWorker) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		if err := w.processNext(ctx); err != nil && ctx.Err() == nil && !errors.Is(err, ErrStaleActionClaim) {
			log.Print("action worker: storage operation failed; pending work retained")
		}
		if err := waitNotice(ctx, w.settings.PollInterval); err != nil {
			return nil
		}
	}
	return nil
}

func (w *ActionWorker) processNext(ctx context.Context) error {
	a, err := w.store.Claim(ctx, w.now())
	if err != nil || a == nil {
		return err
	}
	target, err := w.store.ResolveTarget(ctx, *a)
	if err != nil {
		var de *DeliveryError
		if errors.As(err, &de) && !de.Retryable {
			return w.store.MarkFailed(ctx, *a, de.Code)
		}
		return err
	}
	thread := target.MessageThreadID
	var routed int64
	if chat, perr := strconv.ParseInt(target.ChatID, 10, 64); w.threads != nil && thread == nil && a.ExecutionID > 0 && perr == nil && chat != 0 {
		rctx, rcancel := context.WithTimeout(ctx, w.settings.RequestTimeout)
		if routed = w.threads.ActionThread(rctx, a.WorkspaceID, a.IntegrationID, chat, a.ExecutionID); routed > 0 {
			thread = &routed
		}
		rcancel()
	}
	send := func(thread *int64) (int64, error) {
		sendCtx, cancel := context.WithTimeout(ctx, w.settings.RequestTimeout)
		defer cancel()
		return w.sender.SendText(sendCtx, a.WorkspaceID, target.BotTokenRef, target.ChatID, thread, a.Text)
	}
	id, err := send(thread)
	if err != nil && routed > 0 && ctx.Err() == nil {
		// The topic made the send fail (rejected, so nothing was delivered):
		// resend once, in the reopened topic or in General.
		chat, _ := strconv.ParseInt(target.ChatID, 10, 64)
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), w.settings.RequestTimeout)
		next, ok := w.threads.ActionThreadRejected(rctx, a.WorkspaceID, a.IntegrationID, chat, routed, err)
		rcancel()
		if ok {
			routed, thread = next, nil
			if next > 0 {
				thread = &next
			}
			id, err = send(thread)
		}
	}
	if err == nil {
		if err := w.store.MarkSent(ctx, *a, id); err != nil {
			return err
		}
		bindCtx, bindCancel := context.WithTimeout(context.WithoutCancel(ctx), w.settings.RequestTimeout)
		w.bindWorkflowMessage(bindCtx, *a, target.ChatID, routed, id)
		bindCancel()
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	code, retry := "delivery_unknown", true
	var after time.Duration
	var de *DeliveryError
	if errors.As(err, &de) {
		code, retry, after = de.Code, de.Retryable, de.RetryAfter
	}
	if !retry || a.Attempts >= MaxActionAttempts {
		return w.store.MarkFailed(ctx, *a, code)
	}
	backoff := []time.Duration{2, 4, 8, 16, 32, 60, 60}
	delay := backoff[min(max(a.Attempts-1, 0), len(backoff)-1)] * time.Second
	if after > delay {
		delay = after
	}
	return w.store.MarkRetry(ctx, *a, w.now().Add(delay), code)
}

func (w *ActionWorker) bindWorkflowMessage(ctx context.Context, a OutboundAction, chatID string, threadID, messageID int64) {
	if w.conversations == nil || a.ExecutionID <= 0 || messageID <= 0 {
		return
	}
	chat, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil || chat == 0 {
		log.Print("action worker workflow binding failed (invalid_chat)")
		return
	}
	conversation, err := w.conversations.EnsureConversation(ctx, workflow.Conversation{
		WorkspaceID: a.WorkspaceID, IntegrationID: a.IntegrationID, ChatID: chat, ThreadID: threadID,
	})
	if err != nil {
		log.Print("action worker workflow binding failed (ensure_conversation)")
	} else if err := w.conversations.BindWorkflow(ctx, a.WorkspaceID, conversation.ID, a.ExecutionID, "report"); err != nil {
		log.Print("action worker workflow binding failed (bind_conversation)")
	}
	if err := w.conversations.BindResource(ctx, workflow.ResourceBinding{
		WorkspaceID: a.WorkspaceID, ExecutionID: a.ExecutionID, Kind: "telegram.message",
		ExternalID: workflow.TelegramMessageRef(a.IntegrationID, chat, messageID),
	}); err != nil {
		log.Print("action worker workflow binding failed (bind_resource)")
	}
}
