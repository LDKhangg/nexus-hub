package behavior

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// Regression cases for Phase 4 F3 (R5, R7). External services are never
// called; ordering is proved with channels, and timeouts are only a safety
// net.

const p4Safety = 10 * time.Second

// p4ChanStore feeds updates to InboxWorker.Run and reports every finish.
type p4ChanStore struct {
	updates  chan *InboundUpdate
	finished chan p4Finish
}

type p4Finish struct {
	id   int64
	code string
}

func newP4ChanStore() *p4ChanStore {
	return &p4ChanStore{updates: make(chan *InboundUpdate, 16), finished: make(chan p4Finish, 16)}
}

func (s *p4ChanStore) RecordUpdate(context.Context, string, InboundUpdate) (bool, error) {
	return true, nil
}

func (s *p4ChanStore) ClaimUpdate(context.Context, time.Time, time.Duration) (*InboundUpdate, error) {
	select {
	case u := <-s.updates:
		return u, nil
	default:
		return nil, nil
	}
}

func (s *p4ChanStore) FinishUpdate(_ context.Context, id int64, _ string, code string) error {
	s.finished <- p4Finish{id: id, code: code}
	return nil
}

func (s *p4ChanStore) waitFinish(t *testing.T) p4Finish {
	t.Helper()
	select {
	case f := <-s.finished:
		return f
	case <-time.After(p4Safety):
		t.Fatal("no update finished")
		return p4Finish{}
	}
}

// pollWait replaces the poll interval so Run looks for new updates often.
func pollWait(ctx context.Context, _ time.Duration) error {
	t := time.NewTimer(2 * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(p4Safety):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// waitSent reads sent messages until one contains want.
func waitSent(t *testing.T, ch <-chan OutMessage, want string) OutMessage {
	t.Helper()
	deadline := time.After(p4Safety)
	for {
		select {
		case m := <-ch:
			if strings.Contains(m.Text, want) {
				return m
			}
		case <-deadline:
			t.Fatalf("no message containing %q", want)
			return OutMessage{}
		}
	}
}

// startRun runs the worker with a channel store and returns a stop function
// that cancels it and fails the test unless Run returns.
func (e *p4Env) startRun(t *testing.T) (*p4ChanStore, func()) {
	t.Helper()
	store := newP4ChanStore()
	e.w.d.Inbox = store
	e.w.wait = pollWait
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.w.Run(ctx) }()
	stopped := false
	return store, func() {
		t.Helper()
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run returned %v", err)
			}
		case <-time.After(p4Safety):
			t.Fatal("Run did not return after cancel")
		}
	}
}

func (e *p4Env) feedText(t *testing.T, store *p4ChanStore, id, chat int64, text string) string {
	t.Helper()
	key, err := e.inbox.StageAssistantText(p4WS, text, e.clock)
	if err != nil {
		t.Fatal(err)
	}
	u := e.update(chat, UpdateText, "", AssistantTextPrefix+key)
	u.ID = id
	store.updates <- &u
	return key
}

// queueJob stages text, lets the inbox step queue it and takes the job.
func (e *p4Env) queueJob(t *testing.T, chat int64, text string) assistantJob {
	t.Helper()
	key, err := e.inbox.StageAssistantText(p4WS, text, e.clock)
	if err != nil {
		t.Fatal(err)
	}
	if out := e.w.dispatch(context.Background(), e.update(chat, UpdateText, "", AssistantTextPrefix+key), "corr"); out.code != "assistant_queued" {
		t.Fatalf("not queued: %+v", out)
	}
	select {
	case job := <-e.w.assistantJobs:
		return job
	default:
		t.Fatal("no job queued")
		return assistantJob{}
	}
}

func TestPhase4F3AssistantQueue(t *testing.T) {
	// TC-R5a: while a job waits on a blocked model, a slash command and a
	// button are handled at once; the answer arrives after the model returns.
	t.Run("TC-R5a inbox worker is not blocked by a slow model", func(t *testing.T) {
		e := newP4Env(decisionJSON(t, "answer", nil, "Trả lời chậm", 0.9))
		e.llm.entered = make(chan struct{}, 4)
		e.llm.release = make(chan struct{})
		e.msg.sentCh = make(chan OutMessage, 64)
		released := false
		defer func() {
			if !released {
				close(e.llm.release)
			}
		}()
		store, stop := e.startRun(t)
		defer stop()

		e.feedText(t, store, 1, 77, "trả lời giúp mình")
		if f := store.waitFinish(t); f.id != 1 || f.code != "assistant_queued" {
			t.Fatalf("text update finished %+v", f)
		}
		waitSignal(t, e.llm.entered, "the model call")

		help := e.update(77, UpdateCommand, "/help", "")
		help.ID = 2
		store.updates <- &help
		if f := store.waitFinish(t); f.id != 2 || f.code != "" {
			t.Fatalf("/help finished %+v while the model was busy", f)
		}
		waitSent(t, e.msg.sentCh, "/alias <platform:id> <tên>")

		button := e.update(77, UpdateCallback, "a:unknownNonce_0123456789abcdef", "cbq9 1009")
		button.ID = 3
		store.updates <- &button
		if f := store.waitFinish(t); f.id != 3 {
			t.Fatalf("button finished %+v", f)
		}
		if e.msg.lastToast(t) == "" {
			t.Fatal("button was not answered while the model was busy")
		}
		for _, text := range e.msg.texts() {
			if strings.Contains(text, "Trả lời chậm") {
				t.Fatal("answer sent before the model returned")
			}
		}

		close(e.llm.release)
		released = true
		waitSent(t, e.msg.sentCh, "Trả lời chậm")
		stop()
		if e.llm.calls() != 1 {
			t.Fatalf("model calls=%d", e.llm.calls())
		}
	})

	// TC-R5b: a full queue (8 jobs) replies "busy" at once, the text is
	// dropped, the model is not called, and the queue accepts work again once
	// a job is taken.
	t.Run("TC-R5b full queue replies busy", func(t *testing.T) {
		e := newP4Env(decisionJSON(t, "answer", nil, "ok", 0.9))
		for i := int64(0); i < assistantQueueSize; i++ {
			key, _ := e.inbox.StageAssistantText(p4WS, "tin nhắn", e.clock)
			if out := e.w.dispatch(context.Background(), e.update(100+i, UpdateText, "", AssistantTextPrefix+key), "corr"); out.code != "assistant_queued" {
				t.Fatalf("job %d: %+v", i+1, out)
			}
		}
		if e.msg.count() != 0 {
			t.Fatalf("queueing sent %v", e.msg.texts())
		}
		key, _ := e.inbox.StageAssistantText(p4WS, "tin nhắn thứ chín", e.clock)
		out := e.w.dispatch(context.Background(), e.update(200, UpdateText, "", AssistantTextPrefix+key), "corr")
		if out.code != "assistant_queue_full" || e.msg.last(t).Text != msgAssistantOverloaded || msgAssistantOverloaded != "Mình đang bận, thử lại sau nhé." {
			t.Fatalf("ninth: %+v text=%q", out, e.msg.last(t).Text)
		}
		if e.llm.calls() != 0 || len(e.w.assistantJobs) != assistantQueueSize {
			t.Fatalf("calls=%d queued=%d", e.llm.calls(), len(e.w.assistantJobs))
		}
		if _, ok := e.inbox.take(p4WS, key, e.clock); ok {
			t.Fatal("rejected text was kept in the inbox")
		}
		if out := e.runQueued(t); out.code != "assistant_answer" {
			t.Fatalf("drained job: %+v", out)
		}
		key, _ = e.inbox.StageAssistantText(p4WS, "lại", e.clock)
		if out := e.w.dispatch(context.Background(), e.update(201, UpdateText, "", AssistantTextPrefix+key), "corr"); out.code != "assistant_queued" {
			t.Fatalf("after a free slot: %+v", out)
		}
	})

	// TC-R5c: timeouts and overload say "busy", other provider failures say
	// "failed"; neither says "chưa hiểu ý", and neither retries.
	t.Run("TC-R5c busy mapping", func(t *testing.T) {
		cases := []struct {
			name string
			err  error
			code string
			text string
		}{
			{"deadline", context.DeadlineExceeded, "assistant_busy", msgAssistantOverloaded},
			{"wrapped deadline", fmt.Errorf("wait for slot: %w", context.DeadlineExceeded), "assistant_busy", msgAssistantOverloaded},
			{"llm_timeout", &domain.LLMError{Code: domain.LLMCodeTimeout}, "assistant_busy", msgAssistantOverloaded},
			{"llm_unavailable", fmt.Errorf("call: %w", &domain.LLMError{Code: domain.LLMCodeUnavailable, Retryable: true}), "assistant_busy", msgAssistantOverloaded},
			{"llm_error", &domain.LLMError{Code: domain.LLMCodeError}, "assistant_model_error", msgFailed},
			{"llm_unconfigured", &domain.LLMError{Code: domain.LLMCodeUnconfigured}, "assistant_model_error", msgFailed},
			{"plain error", errors.New("boom token=abc"), "assistant_model_error", msgFailed},
		}
		for _, c := range cases {
			e := newP4Env()
			e.llm.err = c.err
			out := e.say(t, "chào")
			got := e.msg.last(t).Text
			if out.code != c.code || got != c.text || got == msgAssistantNotUnderstood || e.llm.calls() != 1 {
				t.Fatalf("%s: out=%+v text=%q calls=%d", c.name, out, got, e.llm.calls())
			}
		}

		// The call context expires while the model is still working; the
		// provider's own error text does not matter.
		e := newP4Env()
		e.llm.release = make(chan struct{})
		defer close(e.llm.release)
		e.llm.ctxErr = errors.New("transport closed")
		job := e.queueJob(t, 77, "chào")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		out := e.w.assistantJob(ctx, job)
		if out.code != "assistant_busy" || e.msg.last(t).Text != msgAssistantOverloaded || e.llm.calls() != 1 {
			t.Fatalf("expired call: out=%+v text=%q calls=%d", out, e.msg.last(t).Text, e.llm.calls())
		}
	})

	// TC-R5d: "Đang xử lý…" is sent only once the answer has taken longer
	// than the delay, counted from queueing, and before the answer.
	t.Run("TC-R5d working notice only after the delay", func(t *testing.T) {
		// Quick answer: no notice.
		e := newP4Env(decisionJSON(t, "answer", nil, "nhanh", 0.9))
		e.w.assistantNoticeAfter = assistantNoticeAfter
		if assistantNoticeAfter != 3*time.Second {
			t.Fatalf("notice delay=%v, want 3s", assistantNoticeAfter)
		}
		if out := e.say(t, "chào"); out.code != "assistant_answer" {
			t.Fatalf("quick: %+v", out)
		}
		if got := e.msg.texts(); len(got) != 1 || got[0] != "nhanh" {
			t.Fatalf("quick answer sent %q", got)
		}

		// Slow answer: the notice comes after the delay, then the answer.
		const delay = 80 * time.Millisecond
		e = newP4Env(decisionJSON(t, "answer", nil, "chậm", 0.9))
		e.w.assistantNoticeAfter = delay
		e.llm.entered = make(chan struct{}, 4)
		e.llm.release = make(chan struct{})
		e.msg.sentCh = make(chan OutMessage, 16)
		start := time.Now()
		job := e.queueJob(t, 77, "chào")
		result := make(chan outcome, 1)
		go func() { result <- e.w.assistantJob(context.Background(), job) }()
		waitSignal(t, e.llm.entered, "the model call")
		select {
		case m := <-e.msg.sentCh:
			if m.Text != msgAssistantWorking || msgAssistantWorking != "Đang xử lý…" {
				t.Fatalf("first message=%q", m.Text)
			}
			if el := time.Since(start); el < delay {
				t.Fatalf("notice after %v, before the %v delay", el, delay)
			}
		case <-time.After(p4Safety):
			t.Fatal("no working notice for a slow answer")
		}
		close(e.llm.release)
		select {
		case out := <-result:
			if out.code != "assistant_answer" {
				t.Fatalf("slow: %+v", out)
			}
		case <-time.After(p4Safety):
			t.Fatal("job did not finish")
		}
		if got := e.msg.texts(); len(got) != 2 || got[0] != msgAssistantWorking || got[1] != "chậm" {
			t.Fatalf("slow answer sent %q", got)
		}

		// A job that waited in the queue longer than the delay gets the
		// notice as soon as its model call is slow at all.
		e = newP4Env(decisionJSON(t, "answer", nil, "muộn", 0.9))
		e.w.assistantNoticeAfter = assistantNoticeAfter
		e.llm.release = make(chan struct{})
		e.msg.sentCh = make(chan OutMessage, 16)
		job = e.queueJob(t, 77, "chào")
		job.queued = time.Now().Add(-time.Minute)
		result = make(chan outcome, 1)
		go func() { result <- e.w.assistantJob(context.Background(), job) }()
		if m := waitSent(t, e.msg.sentCh, ""); m.Text != msgAssistantWorking {
			t.Fatalf("queued job first message=%q", m.Text)
		}
		close(e.llm.release)
		if out := <-result; out.code != "assistant_answer" {
			t.Fatalf("queued job: %+v", out)
		}
	})

	// TC-R5e: cancelling Run stops the assistant goroutine, even while a job
	// waits on the model, and drops jobs still in the queue.
	t.Run("TC-R5e shutdown stops the assistant goroutine", func(t *testing.T) {
		e := newP4Env(decisionJSON(t, "answer", nil, "không bao giờ", 0.9))
		e.llm.entered = make(chan struct{}, 4)
		e.llm.release = make(chan struct{}) // never closed: only cancel frees it
		store, stop := e.startRun(t)
		e.feedText(t, store, 1, 77, "một")
		if f := store.waitFinish(t); f.code != "assistant_queued" {
			t.Fatalf("first: %+v", f)
		}
		waitSignal(t, e.llm.entered, "the model call")
		e.feedText(t, store, 2, 78, "hai")
		if f := store.waitFinish(t); f.code != "assistant_queued" {
			t.Fatalf("second: %+v", f)
		}
		stop() // fails unless Run returns, which waits for runAssistant
		if e.llm.calls() != 1 {
			t.Fatalf("model calls=%d after shutdown; the queued job must be dropped", e.llm.calls())
		}
		for _, text := range e.msg.texts() {
			if strings.Contains(text, "không bao giờ") {
				t.Fatal("answer sent after shutdown")
			}
		}

		// runAssistant alone returns on cancel with an empty queue.
		e = newP4Env()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); e.w.runAssistant(ctx) }()
		cancel()
		waitSignal(t, done, "runAssistant to return")
	})
}

// TC-R7: after "Làm", the toast says "Đã làm." only when the write ran.
func TestPhase4F3ConfirmToast(t *testing.T) {
	propose := func(t *testing.T, e *p4Env) string {
		t.Helper()
		if out := e.say(t, "đặt tên clickup:12345 là chị Lan"); out.code != "assistant_alias" {
			t.Fatalf("propose: %+v", out)
		}
		doData, _ := buttons(t, e.msg.last(t))
		return doData
	}

	t.Run("TC-R7 Làm on a forgotten contact says not done", func(t *testing.T) {
		e := newP4Env(decisionJSON(t, "alias", map[string]any{"ref": "clickup:12345", "name": "chị Lan"}, "", 0.95))
		e.dir.aliasErr = fmt.Errorf("contact forgotten: %w", ErrNotFound)
		doData := propose(t, e)
		out := e.press(doData)
		if e.msg.lastToast(t) != msgAssistantNotDone || msgAssistantNotDone != "Chưa làm được." || out.code != "not_found" {
			t.Fatalf("toast=%q out=%+v", e.msg.lastToast(t), out)
		}
		if e.msg.last(t).Text != msgNotFound || len(e.dir.aliasCalls()) != 1 {
			t.Fatalf("handler reply=%q aliases=%v", e.msg.last(t).Text, e.dir.aliasCalls())
		}
		if out := e.press(doData); out.code != "stale" || len(e.dir.aliasCalls()) != 1 {
			t.Fatalf("second press: %+v", out)
		}
	})

	t.Run("TC-R7 Làm with a failing handler says not done", func(t *testing.T) {
		e := newP4Env(decisionJSON(t, "alias", map[string]any{"ref": "clickup:12345", "name": "chị Lan"}, "", 0.95))
		e.dir.aliasErr = errors.New("db down")
		out := e.press(propose(t, e))
		if e.msg.lastToast(t) != msgAssistantNotDone || out.code != "handler_failed" || e.msg.last(t).Text != msgFailed {
			t.Fatalf("toast=%q out=%+v reply=%q", e.msg.lastToast(t), out, e.msg.last(t).Text)
		}
	})

	t.Run("TC-R7 Làm that ran but whose reply was lost says done", func(t *testing.T) {
		e := newP4Env(decisionJSON(t, "alias", map[string]any{"ref": "clickup:12345", "name": "chị Lan"}, "", 0.95))
		doData := propose(t, e)
		e.msg.mu.Lock()
		e.msg.sendErr = errors.New("telegram down")
		e.msg.mu.Unlock()
		out := e.press(doData)
		if e.msg.lastToast(t) != msgAssistantDone || out.code != "reply_failed" || len(e.dir.aliasCalls()) != 1 {
			t.Fatalf("toast=%q out=%+v aliases=%v", e.msg.lastToast(t), out, e.dir.aliasCalls())
		}
	})

	t.Run("TC-R7 Làm on prefs says done", func(t *testing.T) {
		e := newP4Env(decisionJSON(t, "prefs_style", map[string]any{"style": "detailed"}, "", 0.9))
		e.say(t, "chi tiết hơn")
		doData, _ := buttons(t, e.msg.last(t))
		if out := e.press(doData); e.msg.lastToast(t) != msgAssistantDone || out.code != "" || e.rep.setPrefsCount() != 1 {
			t.Fatalf("toast=%q out=%+v writes=%d", e.msg.lastToast(t), out, e.rep.setPrefsCount())
		}
	})
}
