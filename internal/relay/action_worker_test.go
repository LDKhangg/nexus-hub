package relay

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

type actionStore struct {
	a          *OutboundAction
	targetErr  error
	sent       int64
	retryAt    time.Time
	retryCode  string
	failedCode string
}

func (s *actionStore) Claim(context.Context, time.Time) (*OutboundAction, error) {
	a := s.a
	s.a = nil
	return a, nil
}
func (s *actionStore) ResolveTarget(context.Context, OutboundAction) (tenant.TelegramTargetConfig, error) {
	return tenant.TelegramTargetConfig{ChatID: "-100", BotTokenRef: "telegram"}, s.targetErr
}
func (s *actionStore) MarkSent(_ context.Context, _ OutboundAction, id int64) error {
	s.sent = id
	return nil
}
func (s *actionStore) MarkRetry(_ context.Context, _ OutboundAction, at time.Time, code string) error {
	s.retryAt, s.retryCode = at, code
	return nil
}
func (s *actionStore) MarkFailed(_ context.Context, _ OutboundAction, code string) error {
	s.failedCode = code
	return nil
}

type textSender struct {
	err           error
	ws, ref, chat string
	text          string
	count         int
}

func (t *textSender) SendText(_ context.Context, ws, ref, chat string, _ *int64, text string) (int64, error) {
	t.ws, t.ref, t.chat, t.text = ws, ref, chat, text
	t.count++
	return 77, t.err
}

type relayBindingTimeout struct {
	workflow.ConversationStore
	receipt    func() bool
	sawReceipt bool
	sawTimeout bool
}

type relayBindingRecorder struct {
	workflow.ConversationStore
	resource workflow.ResourceBinding
}

func (*relayBindingRecorder) EnsureConversation(_ context.Context, c workflow.Conversation) (workflow.Conversation, error) {
	c.ID = 19
	return c, nil
}

func (*relayBindingRecorder) BindWorkflow(context.Context, string, int64, int64, string) error {
	return nil
}

func (s *relayBindingRecorder) BindResource(_ context.Context, r workflow.ResourceBinding) error {
	s.resource = r
	return nil
}

func (s *relayBindingTimeout) EnsureConversation(ctx context.Context, _ workflow.Conversation) (workflow.Conversation, error) {
	s.sawReceipt = s.receipt()
	_, s.sawTimeout = ctx.Deadline()
	<-ctx.Done()
	return workflow.Conversation{}, ctx.Err()
}

func (*relayBindingTimeout) BindResource(ctx context.Context, _ workflow.ResourceBinding) error {
	return ctx.Err()
}

func newActionWorker(s *actionStore, t *textSender) *ActionWorker {
	w := NewActionWorker(s, t, WorkerSettings{})
	w.now = func() time.Time { return time.Unix(100, 0) }
	return w
}

func TestActionWorker_Sent(t *testing.T) {
	s := &actionStore{a: &OutboundAction{ID: 1, Attempts: 1, WorkspaceID: "ws", Text: "🔎 report"}}
	tx := &textSender{}
	if err := newActionWorker(s, tx).processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.sent != 77 || tx.ws != "ws" || tx.ref != "telegram" || tx.chat != "-100" || tx.text != "🔎 report" {
		t.Fatalf("%+v %+v", s, tx)
	}
}

// TC-A5.5 (REQ-5): relay delivery is finalised before best-effort workflow
// binding, and a binding timeout neither fails nor repeats the Telegram send.
func TestActionWorker_BindingTimeoutAfterReceiptDoesNotResend(t *testing.T) {
	s := &actionStore{a: &OutboundAction{ID: 1, Attempts: 1, WorkspaceID: "ws", IntegrationID: "bot", ExecutionID: 9, Text: "report"}}
	tx := &textSender{}
	binding := &relayBindingTimeout{receipt: func() bool { return s.sent == 77 }}
	w := NewActionWorker(s, tx, WorkerSettings{RequestTimeout: 5 * time.Millisecond}, binding)
	if err := w.processNext(context.Background()); err != nil {
		t.Fatalf("delivered action failed on binding timeout: %v", err)
	}
	if err := w.processNext(context.Background()); err != nil {
		t.Fatalf("empty retry pass: %v", err)
	}
	if s.sent != 77 || tx.count != 1 || !binding.sawReceipt || !binding.sawTimeout {
		t.Fatalf("receipt=%d sends=%d binding receipt=%v timeout=%v", s.sent, tx.count, binding.sawReceipt, binding.sawTimeout)
	}
}

// TC-A5.6 (REQ-5, REQ-6): relay reports bind Telegram messages under the bot
// integration as well as chat/message IDs, preventing cross-bot collisions.
func TestActionWorker_WorkflowBindingIncludesIntegration(t *testing.T) {
	s := &actionStore{a: &OutboundAction{ID: 1, Attempts: 1, WorkspaceID: "ws", IntegrationID: "bot-b", ExecutionID: 9, Text: "report"}}
	tx := &textSender{}
	binding := &relayBindingRecorder{}
	w := NewActionWorker(s, tx, WorkerSettings{}, binding)
	if err := w.processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := workflow.ResourceBinding{WorkspaceID: "ws", ExecutionID: 9, Kind: "telegram.message", ExternalID: "bot-b:-100:77"}
	if binding.resource != want {
		t.Fatalf("binding=%+v, want %+v", binding.resource, want)
	}
}

func TestActionWorker_RetryAndFail(t *testing.T) {
	s := &actionStore{a: &OutboundAction{ID: 1, Attempts: 1}}
	if err := newActionWorker(s, &textSender{err: &DeliveryError{Code: "rate_limited", Retryable: true, RetryAfter: 9 * time.Second}}).processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.retryCode != "rate_limited" || !s.retryAt.Equal(time.Unix(109, 0)) {
		t.Fatalf("%v %s", s.retryAt, s.retryCode)
	}
	s = &actionStore{a: &OutboundAction{ID: 1, Attempts: MaxActionAttempts}}
	_ = newActionWorker(s, &textSender{err: &DeliveryError{Code: "server", Retryable: true}}).processNext(context.Background())
	if s.failedCode != "server" {
		t.Fatalf("exhausted: %+v", s)
	}
	s = &actionStore{a: &OutboundAction{ID: 1, Attempts: 1}}
	_ = newActionWorker(s, &textSender{err: &DeliveryError{Code: "rejected"}}).processNext(context.Background())
	if s.failedCode != "rejected" {
		t.Fatalf("final: %+v", s)
	}
	s = &actionStore{a: &OutboundAction{ID: 1, Attempts: 1}}
	_ = newActionWorker(s, &textSender{err: errors.New("boom")}).processNext(context.Background())
	if s.retryCode != "delivery_unknown" {
		t.Fatalf("unknown: %+v", s)
	}
}

func TestActionWorker_InvalidTarget(t *testing.T) {
	s := &actionStore{a: &OutboundAction{ID: 1, Attempts: 1}, targetErr: &DeliveryError{Code: "invalid_target"}}
	tx := &textSender{}
	_ = newActionWorker(s, tx).processNext(context.Background())
	if s.failedCode != "invalid_target" || tx.text != "" {
		t.Fatalf("%+v", s)
	}
}
