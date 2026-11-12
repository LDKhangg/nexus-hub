package relay

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/tenant"
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
}

func (t *textSender) SendText(_ context.Context, ws, ref, chat string, _ *int64, text string) (int64, error) {
	t.ws, t.ref, t.chat, t.text = ws, ref, chat, text
	return 77, t.err
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
