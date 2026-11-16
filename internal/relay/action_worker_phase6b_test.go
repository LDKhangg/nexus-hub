package relay

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

type p6bActionStore struct {
	actionStore
	static *int64
}

func (s *p6bActionStore) ResolveTarget(context.Context, OutboundAction) (tenant.TelegramTargetConfig, error) {
	return tenant.TelegramTargetConfig{ChatID: "-100500", BotTokenRef: "telegram", MessageThreadID: s.static}, nil
}

type p6bSend struct {
	thread int64 // 0: no thread
	err    error
}

type p6bSender struct {
	sends []p6bSend
	fail  func(thread int64) error
}

func (s *p6bSender) SendText(_ context.Context, _, _, _ string, thread *int64, _ string) (int64, error) {
	var th int64
	if thread != nil {
		th = *thread
	}
	var err error
	if s.fail != nil {
		err = s.fail(th)
	}
	s.sends = append(s.sends, p6bSend{th, err})
	if err != nil {
		return 0, err
	}
	return 90, nil
}

type p6bRouter struct {
	thread     int64
	calls      int
	rejected   int
	nextThread int64
	resend     bool
}

func (r *p6bRouter) ActionThread(_ context.Context, _, _ string, chat, exec int64) int64 {
	r.calls++
	if chat != -100500 || exec != 9 {
		return 0
	}
	return r.thread
}

func (r *p6bRouter) ActionThreadRejected(_ context.Context, _, _ string, _, thread int64, _ error) (int64, bool) {
	r.rejected++
	if r.resend {
		return r.nextThread, true
	}
	return thread, false
}

type p6bConvRecorder struct {
	workflow.ConversationStore
	threads []int64
}

func (c *p6bConvRecorder) EnsureConversation(_ context.Context, cv workflow.Conversation) (workflow.Conversation, error) {
	c.threads = append(c.threads, cv.ThreadID)
	cv.ID = 3
	return cv, nil
}
func (*p6bConvRecorder) BindWorkflow(context.Context, string, int64, int64, string) error { return nil }
func (*p6bConvRecorder) BindResource(context.Context, workflow.ResourceBinding) error     { return nil }

func p6bWorker(store *p6bActionStore, sender *p6bSender, router ThreadRouter, conv workflow.ConversationStore) *ActionWorker {
	w := NewActionWorker(store, sender, WorkerSettings{RequestTimeout: time.Second}, conv)
	w.now = func() time.Time { return time.Unix(100, 0) }
	if router != nil {
		w.SetThreadRouter(router)
	}
	return w
}

func p6bAction(exec int64) *OutboundAction {
	return &OutboundAction{ID: 1, Attempts: 1, WorkspaceID: "ws", IntegrationID: "bot", ExecutionID: exec, Text: "agent report"}
}

// TC-A9.4 (REQ-5): an agent report of an execution goes to the routed topic
// and its binding records that thread.
func TestPhase6bActionWorkerRoutesAgentReportToTopic(t *testing.T) {
	store := &p6bActionStore{actionStore: actionStore{a: p6bAction(9)}}
	sender, router, conv := &p6bSender{}, &p6bRouter{thread: 31}, &p6bConvRecorder{}
	if err := p6bWorker(store, sender, router, conv).processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.sends) != 1 || sender.sends[0].thread != 31 || store.sent != 90 {
		t.Fatalf("sends=%+v sent=%d", sender.sends, store.sent)
	}
	if len(conv.threads) != 1 || conv.threads[0] != 31 {
		t.Fatalf("binding threads=%v", conv.threads)
	}
}

// TC-A9.5 (REQ-5): an integration with a static message_thread_id keeps it;
// the router is not asked. No router, no execution, or a router answering 0
// change nothing.
func TestPhase6bActionWorkerStaticThreadAndNoRouting(t *testing.T) {
	static := int64(5)
	store := &p6bActionStore{actionStore: actionStore{a: p6bAction(9)}, static: &static}
	sender, router := &p6bSender{}, &p6bRouter{thread: 31}
	if err := p6bWorker(store, sender, router, nil).processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.sends) != 1 || sender.sends[0].thread != 5 || router.calls != 0 {
		t.Fatalf("static thread lost: sends=%+v router calls=%d", sender.sends, router.calls)
	}
	for name, tc := range map[string]struct {
		exec   int64
		router ThreadRouter
	}{
		"no router":     {9, nil},
		"no execution":  {0, &p6bRouter{thread: 31}},
		"router says 0": {9, &p6bRouter{thread: 0}},
	} {
		store := &p6bActionStore{actionStore: actionStore{a: p6bAction(tc.exec)}}
		sender := &p6bSender{}
		if err := p6bWorker(store, sender, tc.router, nil).processNext(context.Background()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(sender.sends) != 1 || sender.sends[0].thread != 0 {
			t.Fatalf("%s: sends=%+v", name, sender.sends)
		}
	}
}

// TC-A9.6 (REQ-6): a topic-rejected report is resent once (General or the
// reopened topic); any other failure is retried by the queue, not resent.
func TestPhase6bActionWorkerFallback(t *testing.T) {
	t.Run("thread not found resends to General once", func(t *testing.T) {
		store := &p6bActionStore{actionStore: actionStore{a: p6bAction(9)}}
		sender := &p6bSender{fail: func(th int64) error {
			if th != 0 {
				return &DeliveryError{Code: "thread_not_found"}
			}
			return nil
		}}
		router, conv := &p6bRouter{thread: 31, resend: true, nextThread: 0}, &p6bConvRecorder{}
		if err := p6bWorker(store, sender, router, conv).processNext(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(sender.sends) != 2 || sender.sends[1].thread != 0 || sender.sends[1].err != nil || store.sent != 90 || router.rejected != 1 {
			t.Fatalf("sends=%+v sent=%d rejected=%d", sender.sends, store.sent, router.rejected)
		}
		if len(conv.threads) != 1 || conv.threads[0] != 0 {
			t.Fatalf("binding must follow the delivered thread: %v", conv.threads)
		}
	})
	t.Run("closed then reopened resends in the topic", func(t *testing.T) {
		store := &p6bActionStore{actionStore: actionStore{a: p6bAction(9)}}
		n := 0
		sender := &p6bSender{fail: func(int64) error {
			n++
			if n == 1 {
				return &DeliveryError{Code: "topic_closed"}
			}
			return nil
		}}
		router := &p6bRouter{thread: 31, resend: true, nextThread: 31}
		if err := p6bWorker(store, sender, router, nil).processNext(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(sender.sends) != 2 || sender.sends[1].thread != 31 || store.sent != 90 {
			t.Fatalf("sends=%+v", sender.sends)
		}
	})
	t.Run("transport error is not resent, queue retries", func(t *testing.T) {
		store := &p6bActionStore{actionStore: actionStore{a: p6bAction(9)}}
		sender := &p6bSender{fail: func(int64) error { return &DeliveryError{Code: "transport", Retryable: true} }}
		router := &p6bRouter{thread: 31}
		if err := p6bWorker(store, sender, router, nil).processNext(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(sender.sends) != 1 || store.retryCode != "transport" || store.sent != 0 {
			t.Fatalf("sends=%+v retry=%q sent=%d", sender.sends, store.retryCode, store.sent)
		}
	})
	t.Run("second failure after the resend is marked, never a third send", func(t *testing.T) {
		store := &p6bActionStore{actionStore: actionStore{a: p6bAction(9)}}
		sender := &p6bSender{fail: func(int64) error { return &DeliveryError{Code: "thread_not_found"} }}
		router := &p6bRouter{thread: 31, resend: true, nextThread: 0}
		if err := p6bWorker(store, sender, router, nil).processNext(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(sender.sends) != 2 || store.failedCode != "thread_not_found" {
			t.Fatalf("sends=%+v failed=%q", sender.sends, store.failedCode)
		}
	})
	t.Run("cancelled context returns, no resend", func(t *testing.T) {
		store := &p6bActionStore{actionStore: actionStore{a: p6bAction(9)}}
		ctx, cancel := context.WithCancel(context.Background())
		sender := &p6bSender{fail: func(int64) error { cancel(); return &DeliveryError{Code: "thread_not_found"} }}
		router := &p6bRouter{thread: 31, resend: true}
		err := p6bWorker(store, sender, router, nil).processNext(ctx)
		if !errors.Is(err, context.Canceled) || len(sender.sends) != 1 {
			t.Fatalf("err=%v sends=%+v", err, sender.sends)
		}
	})
}
