package relay

import (
	"context"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// Phase 10 Batch A, R1 (REQ-1, REQ-4): the notice worker routes a ClickUp notice to its task topic, recovers
// once to General, adds the button to General notices and reports where the message went.

type p10Store struct {
	deliveryStoreStub
	target   tenant.TelegramTargetConfig
	receipts []DeliveryReceipt
	retries  int
}

func (s *p10Store) ResolveTarget(context.Context, NoticeJob) (tenant.TelegramTargetConfig, error) {
	return s.target, nil
}
func (s *p10Store) MarkSent(_ context.Context, _ NoticeJob, r DeliveryReceipt) error {
	s.receipts = append(s.receipts, r)
	s.sent++
	return nil
}
func (s *p10Store) MarkRetry(_ context.Context, _ NoticeJob, _ time.Time, _ string) error {
	s.retries++
	return nil
}

type p10Router struct {
	thread    int64
	next      int64
	resend    bool
	asked     []string
	rejected  int
	buttonFor []string
	noButton  bool
}

func (r *p10Router) NoticeThread(_ context.Context, _, _ string, _ int64, task string) int64 {
	r.asked = append(r.asked, task)
	return r.thread
}
func (r *p10Router) NoticeThreadRejected(context.Context, string, string, int64, int64, error) (int64, bool) {
	r.rejected++
	return r.next, r.resend
}
func (r *p10Router) NoticeButton(task string) (NoticeButton, bool) {
	r.buttonFor = append(r.buttonFor, task)
	if r.noButton {
		return NoticeButton{}, false
	}
	return NoticeButton{Text: "📌 Mở topic task", Data: "tt:" + task}, true
}

type p10Sent struct {
	thread int64
	button *NoticeButton
	via    string
}

// p10Sender records every send; failThread makes sends to that thread fail.
type p10Sender struct {
	sends      []p10Sent
	failThread int64
	failErr    error
	plainOnly  bool
}

func (s *p10Sender) Send(_ context.Context, t tenant.TelegramTargetConfig, _ domain.NormalizedEvent) (DeliveryReceipt, error) {
	return s.record(t, nil, "send")
}

func (s *p10Sender) record(t tenant.TelegramTargetConfig, b *NoticeButton, via string) (DeliveryReceipt, error) {
	var th int64
	if t.MessageThreadID != nil {
		th = *t.MessageThreadID
	}
	s.sends = append(s.sends, p10Sent{th, b, via})
	if s.failThread != 0 && th == s.failThread {
		return DeliveryReceipt{}, s.failErr
	}
	return DeliveryReceipt{MessageID: int64(100 + len(s.sends))}, nil
}

type p10ButtonSender struct{ *p10Sender }

func (s p10ButtonSender) SendNotice(_ context.Context, t tenant.TelegramTargetConfig, _ domain.NormalizedEvent, b *NoticeButton) (DeliveryReceipt, error) {
	return s.record(t, b, "notice")
}

func p10Worker(store *p10Store, sender NoticeSender, router NoticeRouter) *NoticeWorker {
	w := NewNoticeWorker(store, sender, WorkerSettings{})
	if router != nil {
		w.SetNoticeRouter(router)
	}
	return w
}

func p10Job(task string) *NoticeJob {
	return &NoticeJob{ID: 1, Attempts: 1, WorkspaceID: "ws", TargetIntegrationID: "tg", Event: domain.NormalizedEvent{ExternalID: task}}
}

func p10Store1(task string) *p10Store {
	return &p10Store{deliveryStoreStub: deliveryStoreStub{job: p10Job(task)}, target: tenant.TelegramTargetConfig{ChatID: "-1001234567890"}}
}

// TC-R1a (REQ-1): a notice about a task with a topic goes to that topic, without the button; the receipt names chat and thread.
func TestP10Worker_NoticeGoesToTaskTopic(t *testing.T) {
	store := p10Store1("86abc")
	sender := p10ButtonSender{&p10Sender{}}
	router := &p10Router{thread: 41}
	if err := p10Worker(store, sender, router).processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.sends) != 1 || sender.sends[0].thread != 41 || sender.sends[0].button != nil {
		t.Fatalf("sends %+v", sender.sends)
	}
	if len(store.receipts) != 1 || store.receipts[0].ChatID != -1001234567890 || store.receipts[0].ThreadID != 41 {
		t.Fatalf("receipt %+v", store.receipts)
	}
	if len(router.asked) != 1 || router.asked[0] != "86abc" {
		t.Fatalf("router asked %v", router.asked)
	}
}

// TC-R1b (REQ-1): without a topic the notice goes to General with the button; a sender without buttons still sends.
func TestP10Worker_GeneralNoticeCarriesButton(t *testing.T) {
	store := p10Store1("86abc")
	sender := p10ButtonSender{&p10Sender{}}
	router := &p10Router{}
	if err := p10Worker(store, sender, router).processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.sends) != 1 || sender.sends[0].thread != 0 || sender.sends[0].via != "notice" ||
		sender.sends[0].button == nil || sender.sends[0].button.Data != "tt:86abc" {
		t.Fatalf("sends %+v", sender.sends)
	}
	if store.receipts[0].ThreadID != 0 || store.receipts[0].ChatID == 0 {
		t.Fatalf("receipt %+v", store.receipts[0])
	}
	// No button offered: still one plain notice.
	store2 := p10Store1("86abc")
	sender2 := p10ButtonSender{&p10Sender{}}
	if err := p10Worker(store2, sender2, &p10Router{noButton: true}).processNext(context.Background()); err != nil || len(sender2.sends) != 1 || sender2.sends[0].button != nil {
		t.Fatalf("sends %+v err %v", sender2.sends, err)
	}
	// A sender that cannot show buttons falls back to Send.
	store3 := p10Store1("86abc")
	plain := &p10Sender{}
	if err := p10Worker(store3, plain, &p10Router{}).processNext(context.Background()); err != nil || len(plain.sends) != 1 || plain.sends[0].via != "send" {
		t.Fatalf("sends %+v err %v", plain.sends, err)
	}
}

// TC-R4b (REQ-4): a send the topic rejects is resent once, in General, never in another topic; a second failure is a retry.
func TestP10Worker_RejectedTopicResendsOnceToGeneral(t *testing.T) {
	store := p10Store1("86abc")
	sender := p10ButtonSender{&p10Sender{failThread: 41, failErr: &DeliveryError{Code: "thread_not_found"}}}
	router := &p10Router{thread: 41, next: 0, resend: true}
	if err := p10Worker(store, sender, router).processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender.sends) != 2 || sender.sends[0].thread != 41 || sender.sends[1].thread != 0 {
		t.Fatalf("sends %+v", sender.sends)
	}
	if sender.sends[1].button == nil || sender.sends[1].button.Data != "tt:86abc" {
		t.Fatalf("the General resend must carry the button: %+v", sender.sends[1])
	}
	if router.rejected != 1 || store.sent != 1 || store.receipts[0].ThreadID != 0 {
		t.Fatalf("rejected %d sent %d receipt %+v", router.rejected, store.sent, store.receipts)
	}
	// The router says do not resend (a transport error): one send, one retry, nothing else.
	store2 := p10Store1("86abc")
	sender2 := p10ButtonSender{&p10Sender{failThread: 41, failErr: &DeliveryError{Code: "network", Retryable: true}}}
	router2 := &p10Router{thread: 41, resend: false}
	if err := p10Worker(store2, sender2, router2).processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sender2.sends) != 1 || store2.retries != 1 || store2.sent != 0 {
		t.Fatalf("sends %d retries %d sent %d", len(sender2.sends), store2.retries, store2.sent)
	}
	// The resend itself fails: marked for retry, no third send.
	store3 := p10Store1("86abc")
	always := &p10Sender{failThread: 41, failErr: &DeliveryError{Code: "thread_not_found"}}
	failGeneral := &alwaysFail{p10Sender: always}
	router3 := &p10Router{thread: 41, next: 0, resend: true}
	if err := p10Worker(store3, failGeneral, router3).processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(always.sends) != 2 || store3.sent != 0 {
		t.Fatalf("sends %d sent %d", len(always.sends), store3.sent)
	}
}

type alwaysFail struct{ *p10Sender }

func (a *alwaysFail) Send(_ context.Context, t tenant.TelegramTargetConfig, _ domain.NormalizedEvent) (DeliveryReceipt, error) {
	_, _ = a.record(t, nil, "send")
	return DeliveryReceipt{}, &DeliveryError{Code: "thread_not_found"}
}

// TC-R1d (REQ-1): a fixed message_thread_id, a chat id that is not numeric, or an event without a task disables routing.
func TestP10Worker_FixedThreadAndBadChatDisableRouting(t *testing.T) {
	fixed := int64(32)
	for name, tc := range map[string]struct {
		target tenant.TelegramTargetConfig
		task   string
		want   int64
	}{
		"fixed thread":    {tenant.TelegramTargetConfig{ChatID: "-1001234567890", MessageThreadID: &fixed}, "86abc", 32},
		"chat not number": {tenant.TelegramTargetConfig{ChatID: "@channel"}, "86abc", 0},
		"no task":         {tenant.TelegramTargetConfig{ChatID: "-1001234567890"}, "", 0},
	} {
		t.Run(name, func(t *testing.T) {
			store := p10Store1(tc.task)
			store.target = tc.target
			sender := p10ButtonSender{&p10Sender{}}
			router := &p10Router{thread: 41}
			if err := p10Worker(store, sender, router).processNext(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(router.asked) != 0 || len(sender.sends) != 1 || sender.sends[0].thread != tc.want || sender.sends[0].button != nil {
				t.Fatalf("asked %v sends %+v", router.asked, sender.sends)
			}
		})
	}
	// Without a router every notice goes as configured.
	store := p10Store1("86abc")
	sender := &p10Sender{}
	if err := p10Worker(store, sender, nil).processNext(context.Background()); err != nil || len(sender.sends) != 1 || sender.sends[0].thread != 0 {
		t.Fatalf("%+v %v", sender.sends, err)
	}
}
