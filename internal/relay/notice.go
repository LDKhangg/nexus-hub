package relay

import (
	"context"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"time"
)

type AcceptResult struct{ Accepted, Duplicates, Ignored int }
type NoticeAcceptor interface {
	Accept(context.Context, []domain.NormalizedEvent) (AcceptResult, error)
}
type DeliveryReceipt struct{ MessageID int64 }
type NoticeSender interface {
	Send(context.Context, tenant.TelegramTargetConfig, domain.NormalizedEvent) (DeliveryReceipt, error)
}

// DeliveryError contains only safe diagnostics, never a token-bearing URL or remote body.
type DeliveryError struct {
	Code       string
	Retryable  bool
	RetryAfter time.Duration
}

func (e *DeliveryError) Error() string { return "notice delivery: " + e.Code }
