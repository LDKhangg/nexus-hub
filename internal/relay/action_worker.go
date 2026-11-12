package relay

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/tenant"
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

// ActionWorker delivers outbound actions with the notice retry policy.
type ActionWorker struct {
	store    ActionStore
	sender   TextSender
	settings WorkerSettings
	now      func() time.Time
}

func NewActionWorker(store ActionStore, sender TextSender, settings WorkerSettings) *ActionWorker {
	if settings.PollInterval <= 0 {
		settings.PollInterval = time.Second
	}
	if settings.RequestTimeout <= 0 {
		settings.RequestTimeout = 10 * time.Second
	}
	return &ActionWorker{store: store, sender: sender, settings: settings, now: time.Now}
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
	sendCtx, cancel := context.WithTimeout(ctx, w.settings.RequestTimeout)
	id, err := w.sender.SendText(sendCtx, a.WorkspaceID, target.BotTokenRef, target.ChatID, target.MessageThreadID, a.Text)
	cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return w.store.MarkSent(ctx, *a, id)
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
