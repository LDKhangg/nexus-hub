package relay

import (
	"context"
	"errors"
	"log"
	"strconv"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

const MaxNoticeAttempts = 8

var ErrStaleNoticeClaim = errors.New("stale notice claim")

type NoticeJob struct {
	ID                               int64
	ClaimToken                       string
	Attempts                         int
	Event                            domain.NormalizedEvent
	TargetIntegrationID, WorkspaceID string
}
type NoticeDeliveryStore interface {
	Claim(context.Context, time.Time) (*NoticeJob, error)
	ResolveTarget(context.Context, NoticeJob) (tenant.TelegramTargetConfig, error)
	MarkSent(context.Context, NoticeJob, DeliveryReceipt) error
	MarkRetry(context.Context, NoticeJob, time.Time, string) error
	MarkFailed(context.Context, NoticeJob, string) error
}
type WorkerSettings struct{ PollInterval, RequestTimeout time.Duration }
type NoticeWorker struct {
	store    NoticeDeliveryStore
	sender   NoticeSender
	settings WorkerSettings
	now      func() time.Time
	wait     func(context.Context, time.Duration) error
	router   NoticeRouter
}

// SetNoticeRouter routes ClickUp notices to their task's existing topic.
// Call it before Run.
func (w *NoticeWorker) SetNoticeRouter(r NoticeRouter) { w.router = r }

func NewNoticeWorker(store NoticeDeliveryStore, sender NoticeSender, settings WorkerSettings) *NoticeWorker {
	if settings.PollInterval <= 0 {
		settings.PollInterval = time.Second
	}
	if settings.RequestTimeout <= 0 {
		settings.RequestTimeout = 10 * time.Second
	}
	return &NoticeWorker{store: store, sender: sender, settings: settings, now: time.Now, wait: waitNotice}
}
func waitNotice(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (w *NoticeWorker) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		if err := w.processNext(ctx); err != nil && ctx.Err() == nil {
			log.Print("notice worker: storage operation failed; pending work retained")
		}
		if err := w.wait(ctx, w.settings.PollInterval); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
	return nil
}
func (w *NoticeWorker) processNext(ctx context.Context) error {
	job, err := w.store.Claim(ctx, w.now())
	if err != nil || job == nil {
		return err
	}
	target, err := w.store.ResolveTarget(ctx, *job)
	if err != nil {
		var delivery *DeliveryError
		if errors.As(err, &delivery) && !delivery.Retryable {
			return w.store.MarkFailed(ctx, *job, delivery.Code)
		}
		return err // Transient database failure: retain lease for recovery.
	}
	receipt, err := w.deliver(ctx, *job, target)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return w.store.MarkSent(ctx, *job, receipt)
	}
	code := "delivery_unknown"
	retry := true
	var after time.Duration
	var delivery *DeliveryError
	if errors.As(err, &delivery) {
		code, retry, after = delivery.Code, delivery.Retryable, delivery.RetryAfter
	}
	if !retry || job.Attempts >= MaxNoticeAttempts {
		return w.store.MarkFailed(ctx, *job, code)
	}
	backoff := []time.Duration{2, 4, 8, 16, 32, 60, 60}
	index := job.Attempts - 1
	if index < 0 {
		index = 0
	}
	if index >= len(backoff) {
		index = len(backoff) - 1
	}
	delay := backoff[index] * time.Second
	if after > delay {
		delay = after
	}
	return w.store.MarkRetry(ctx, *job, w.now().Add(delay), code)
}

// deliver sends the job's notice. With a router and no configured thread,
// a notice about a ClickUp task that has a topic goes to that topic; a
// send the topic made fail (rejected, so nothing was delivered) is resent
// once, in the reopened topic or in General. A notice that goes to
// General carries the router's button when the sender supports buttons.
// The receipt names the chat and thread the notice went to.
func (w *NoticeWorker) deliver(ctx context.Context, job NoticeJob, target tenant.TelegramTargetConfig) (DeliveryReceipt, error) {
	chat, perr := strconv.ParseInt(target.ChatID, 10, 64)
	routable := w.router != nil && target.MessageThreadID == nil && perr == nil && chat != 0 && job.Event.ExternalID != ""
	var routed int64
	if routable {
		rctx, rcancel := context.WithTimeout(ctx, w.settings.RequestTimeout)
		routed = w.router.NoticeThread(rctx, job.WorkspaceID, job.TargetIntegrationID, chat, job.Event.ExternalID)
		rcancel()
	}
	send := func(thread int64) (DeliveryReceipt, error) {
		to := target
		if thread > 0 {
			to.MessageThreadID = &thread
		}
		sendCtx, cancel := context.WithTimeout(ctx, w.settings.RequestTimeout)
		defer cancel()
		var receipt DeliveryReceipt
		var err error
		if bs, ok := w.sender.(ButtonNoticeSender); ok && routable && thread == 0 {
			var button *NoticeButton
			if b, ok := w.router.NoticeButton(job.Event.ExternalID); ok {
				button = &b
			}
			receipt, err = bs.SendNotice(sendCtx, to, job.Event, button)
		} else {
			receipt, err = w.sender.Send(sendCtx, to, job.Event)
		}
		if err == nil {
			receipt.ChatID = chat
			if to.MessageThreadID != nil {
				receipt.ThreadID = *to.MessageThreadID
			}
		}
		return receipt, err
	}
	receipt, err := send(routed)
	if err != nil && routed > 0 && ctx.Err() == nil {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), w.settings.RequestTimeout)
		next, ok := w.router.NoticeThreadRejected(rctx, job.WorkspaceID, job.TargetIntegrationID, chat, routed, err)
		rcancel()
		if ok {
			receipt, err = send(next)
		}
	}
	return receipt, err
}
