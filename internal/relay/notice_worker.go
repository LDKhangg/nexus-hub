package relay

import (
	"context"
	"errors"
	"log"
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
}

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
	sendCtx, cancel := context.WithTimeout(ctx, w.settings.RequestTimeout)
	receipt, err := w.sender.Send(sendCtx, target, job.Event)
	cancel()
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
