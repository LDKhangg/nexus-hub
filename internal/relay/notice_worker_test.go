package relay

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

type deliveryStoreStub struct {
	job                     *NoticeJob
	next                    time.Time
	sent, failed            int
	resolveErr, errorOnSent error
}

func (s *deliveryStoreStub) Claim(context.Context, time.Time) (*NoticeJob, error) {
	j := s.job
	s.job = nil
	return j, nil
}
func (s *deliveryStoreStub) ResolveTarget(context.Context, NoticeJob) (tenant.TelegramTargetConfig, error) {
	return tenant.TelegramTargetConfig{}, s.resolveErr
}
func (s *deliveryStoreStub) MarkSent(context.Context, NoticeJob, DeliveryReceipt) error {
	s.sent++
	return s.errorOnSent
}
func (s *deliveryStoreStub) MarkRetry(_ context.Context, _ NoticeJob, next time.Time, _ string) error {
	s.next = next
	return nil
}
func (s *deliveryStoreStub) MarkFailed(context.Context, NoticeJob, string) error {
	s.failed++
	return nil
}

type senderFunc func(context.Context, tenant.TelegramTargetConfig, domain.NormalizedEvent) (DeliveryReceipt, error)

func (f senderFunc) Send(c context.Context, t tenant.TelegramTargetConfig, e domain.NormalizedEvent) (DeliveryReceipt, error) {
	return f(c, t, e)
}

func TestNoticeWorkerTransitions(t *testing.T) {
	for _, mode := range []string{"success", "transient", "rate limit", "permanent", "exhausted", "disabled", "ack failure", "idle", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Unix(1700000000, 0)
			s := &deliveryStoreStub{job: &NoticeJob{ID: 1, Attempts: 1}}
			calls := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			send := senderFunc(func(c context.Context, _ tenant.TelegramTargetConfig, _ domain.NormalizedEvent) (DeliveryReceipt, error) {
				calls++
				switch mode {
				case "transient", "exhausted":
					return DeliveryReceipt{}, &DeliveryError{Code: "network", Retryable: true}
				case "rate limit":
					return DeliveryReceipt{}, &DeliveryError{Code: "rate_limited", Retryable: true, RetryAfter: 90 * time.Second}
				case "permanent":
					return DeliveryReceipt{}, &DeliveryError{Code: "rejected"}
				case "canceled":
					cancel()
					return DeliveryReceipt{}, c.Err()
				}
				return DeliveryReceipt{MessageID: 42}, nil
			})
			if mode == "idle" {
				s.job = nil
			}
			if mode == "exhausted" {
				s.job.Attempts = 8
			}
			if mode == "disabled" {
				s.resolveErr = &DeliveryError{Code: "invalid_target"}
			}
			if mode == "ack failure" {
				s.errorOnSent = errors.New("database unavailable")
			}
			w := NewNoticeWorker(s, send, WorkerSettings{})
			w.now = func() time.Time { return now }
			err := w.processNext(ctx)
			switch mode {
			case "success":
				if err != nil || s.sent != 1 {
					t.Fatal(err, s)
				}
			case "transient":
				if !s.next.Equal(now.Add(2 * time.Second)) {
					t.Fatal(s.next)
				}
			case "rate limit":
				if !s.next.Equal(now.Add(90 * time.Second)) {
					t.Fatal(s.next)
				}
			case "permanent", "exhausted", "disabled":
				if s.failed != 1 {
					t.Fatal(s)
				}
			case "ack failure":
				if err == nil || !s.next.IsZero() {
					t.Fatal("ambiguous send must retain recoverable lease")
				}
			case "idle":
				if calls != 0 {
					t.Fatal("sent idle job")
				}
			case "canceled":
				if !errors.Is(err, context.Canceled) || s.failed != 0 || !s.next.IsZero() {
					t.Fatal("cancellation lost pending delivery", err)
				}
			}
		})
	}
}

func TestNoticeWorkerRetryThenSuccess(t *testing.T) {
	s := &deliveryStoreStub{job: &NoticeJob{ID: 1, Attempts: 1}}
	calls := 0
	w := NewNoticeWorker(s, senderFunc(func(context.Context, tenant.TelegramTargetConfig, domain.NormalizedEvent) (DeliveryReceipt, error) {
		calls++
		if calls == 1 {
			return DeliveryReceipt{}, &DeliveryError{Code: "network", Retryable: true}
		}
		return DeliveryReceipt{MessageID: 2}, nil
	}), WorkerSettings{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.wait = func(context.Context, time.Duration) error {
		if calls == 1 {
			s.job = &NoticeJob{ID: 1, Attempts: 2}
			return nil
		}
		cancel()
		return context.Canceled
	}
	if err := w.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if s.sent != 1 || calls != 2 {
		t.Fatal("retry failed", s, calls)
	}
}
