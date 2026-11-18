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

// DeliveryReceipt is a delivered notice: the Telegram message id and, set
// by the worker, the chat and thread (0: General) it went to.
type DeliveryReceipt struct {
	MessageID int64
	ChatID    int64
	ThreadID  int64
}
type NoticeSender interface {
	Send(context.Context, tenant.TelegramTargetConfig, domain.NormalizedEvent) (DeliveryReceipt, error)
}

// NoticeButton is one inline callback button under a notice.
type NoticeButton struct{ Text, Data string }

// ButtonNoticeSender sends a notice with an optional callback button. A
// NoticeSender that implements it gets the "open task topic" button on
// notices sent to General.
type ButtonNoticeSender interface {
	SendNotice(ctx context.Context, target tenant.TelegramTargetConfig, e domain.NormalizedEvent, button *NoticeButton) (DeliveryReceipt, error)
}

// NoticeRouter picks the Telegram topic of a ClickUp notice: the task's
// existing topic, or General. Nil: every notice goes to the target as
// configured.
type NoticeRouter interface {
	// NoticeThread returns the thread of the task's existing topic in the
	// chat (reopened when closed), or 0 for General. It never creates a
	// topic.
	NoticeThread(ctx context.Context, workspaceID, integrationID string, chatID int64, taskID string) int64
	// NoticeThreadRejected handles a send the topic made fail: it returns
	// the thread to resend to (0: General) and whether to resend once.
	NoticeThreadRejected(ctx context.Context, workspaceID, integrationID string, chatID, threadID int64, err error) (int64, bool)
	// NoticeButton returns the button of a notice about the task that goes
	// to General; ok is false when there is none.
	NoticeButton(taskID string) (NoticeButton, bool)
}

// DeliveryError contains only safe diagnostics, never a token-bearing URL or remote body.
type DeliveryError struct {
	Code       string
	Retryable  bool
	RetryAfter time.Duration
}

func (e *DeliveryError) Error() string { return "notice delivery: " + e.Code }
