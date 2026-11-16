package behavior

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/relay"
)

// Fix Brief #A1 regressions: unclear creates, stale holders, slow task-name
// reads and the non-forum memory.

func p6bUnclear() error { return &relay.DeliveryError{Code: "transport", Retryable: true} }

// TC-A11.1 (R3a): an unclear create keeps the reservation; the next caller
// is pending (General, no create); after the lease exactly one caller
// reclaims and creates.
func TestPhase6bUnclearCreateKeepsReservationUntilLease(t *testing.T) {
	for name, cerr := range map[string]error{"transport": p6bUnclear(), "timeout": context.DeadlineExceeded, "server": &relay.DeliveryError{Code: "server", Retryable: true}} {
		t.Run(name, func(t *testing.T) {
			e := newP6bEnv(t)
			e.client.createErr = cerr
			ctx := withWorkflowExecution(context.Background(), 12)
			rc := p6bRC("task", "UNC-1")
			if _, err := e.send(ctx, "k1", "one", rc); err != nil {
				t.Fatal(err)
			}
			if e.store.count() != 1 || e.client.createCount() != 1 || e.store.released != 0 {
				t.Fatalf("rows=%d creates=%d released=%d, want reservation kept", e.store.count(), e.client.createCount(), e.store.released)
			}
			e.client.createErr = nil
			for i := 0; i < 3; i++ { // pending: General, no second create
				if _, err := e.send(ctx, "kp"+string(rune('a'+i)), "x", p6bRC("task", "UNC-1")); err != nil {
					t.Fatal(err)
				}
				if lastThread(t, e.msg) != 0 || e.client.createCount() != 1 {
					t.Fatalf("pending caller: thread=%d creates=%d", lastThread(t, e.msg), e.client.createCount())
				}
			}
			e.store.mu.Lock()
			e.store.clock = e.store.clock.Add(TopicReservationLease + time.Second)
			e.store.mu.Unlock()
			if _, err := e.send(ctx, "k2", "two", rc); err != nil {
				t.Fatal(err)
			}
			if lastThread(t, e.msg) == 0 || e.client.createCount() != 2 {
				t.Fatalf("after lease thread=%d creates=%d", lastThread(t, e.msg), e.client.createCount())
			}
			if _, err := e.send(ctx, "k3", "three", rc); err != nil || e.client.createCount() != 2 {
				t.Fatalf("topic not reused, creates=%d err=%v", e.client.createCount(), err)
			}
		})
	}
}

// TC-A11.2 (R3b): a definite rejection (rate limit, rejected, not forum)
// releases the reservation, so the next caller may retry at once (not-forum
// is held back by the memory instead).
func TestPhase6bDefiniteRejectionReleases(t *testing.T) {
	for name, cerr := range map[string]error{
		"rate limited": &relay.DeliveryError{Code: "rate_limited", Retryable: true},
		"rejected":     p6bErr("rejected"),
	} {
		t.Run(name, func(t *testing.T) {
			e := newP6bEnv(t)
			e.client.createErr = cerr
			ctx := withWorkflowExecution(context.Background(), 12)
			rc := p6bRC("task", "DEF-1")
			if _, err := e.send(ctx, "k1", "one", rc); err != nil || e.store.count() != 0 || e.store.released != 1 {
				t.Fatalf("rows=%d released=%d err=%v", e.store.count(), e.store.released, err)
			}
			e.client.createErr = nil
			if _, err := e.send(ctx, "k2", "two", rc); err != nil || lastThread(t, e.msg) == 0 {
				t.Fatalf("retry thread=%d err=%v", lastThread(t, e.msg), err)
			}
		})
	}
}

// TC-A11.3 (R3c): a stale holder (reservation taken over after the lease)
// never overwrites the new holder's thread; its own message still goes to
// the topic it created.
func TestPhase6bStaleHolderDoesNotOverwriteBinding(t *testing.T) {
	e := newP6bEnv(t)
	ctx := withWorkflowExecution(context.Background(), 12)
	first := true
	e.client.hook = func() {
		if !first {
			return
		}
		first = false
		// Holder A is slow: its lease runs out and holder B takes over (B creates thread 101 first, A's create returns 102).
		e.store.mu.Lock()
		e.store.clock = e.store.clock.Add(TopicReservationLease + time.Second)
		e.store.mu.Unlock()
		got := e.router.Destination(ctx, p6bBase(), TopicIntent{Mode: "task", TaskID: "STALE-1", ExecutionID: 12})
		if got.ThreadID == 0 {
			t.Error("holder B did not create the topic")
		}
	}
	a := e.router.Destination(ctx, p6bBase(), TopicIntent{Mode: "task", TaskID: "STALE-1", ExecutionID: 12})
	row := e.store.only(t)
	if e.client.createCount() != 2 || row.ThreadID != 101 {
		t.Fatalf("creates=%d bound thread=%d, want 2 and B's 101", e.client.createCount(), row.ThreadID)
	}
	if a.ThreadID != 102 {
		t.Fatalf("stale holder's own message thread=%d, want its own 102", a.ThreadID)
	}
	// Later sends use B's binding.
	if again := e.router.Destination(ctx, p6bBase(), TopicIntent{Mode: "task", TaskID: "STALE-1", ExecutionID: 12}); again.ThreadID != 101 {
		t.Fatalf("later send thread=%d", again.ThreadID)
	}
}

// TC-A11.4 (R3d): a slow task-name read does not hold the first send: the
// topic is named by the bare id within the budget.
func TestPhase6bSlowTaskNameFallsBackToBareID(t *testing.T) {
	e := newP6bEnv(t)
	e.router.d.TaskName = func(ctx context.Context, _, _, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	start := time.Now()
	got := e.router.Destination(context.Background(), p6bBase(), TopicIntent{Mode: "task", TaskID: "SLOW-1", ExecutionID: 1})
	if took := time.Since(start); took > taskNameTimeout+2*time.Second {
		t.Fatalf("send waited %v for the task name", took)
	}
	if got.ThreadID == 0 || e.client.names[0] != "SLOW-1" {
		t.Fatalf("thread=%d name=%q", got.ThreadID, e.client.names[0])
	}
}

// TC-A12.1 (extra): not-forum and no-rights chats skip reserve/create for
// 10 minutes, still deliver once to General, and count one fallback per send
// with the same code.
func TestPhase6bNonForumMemory(t *testing.T) {
	for _, code := range []string{"topic_not_forum", "topic_no_rights"} {
		t.Run(code, func(t *testing.T) {
			e := newP6bEnv(t)
			logs := p6aCaptureLog(t)
			now := time.Unix(5000, 0)
			e.router.now = func() time.Time { return now }
			e.client.createErr = p6bErr(code)
			ctx := withWorkflowExecution(context.Background(), 12)
			if _, err := e.send(ctx, "k1", "one", p6bRC("task", "NF-1")); err != nil {
				t.Fatal(err)
			}
			now = now.Add(9 * time.Minute)
			rows := e.store.count()
			for i, task := range []string{"NF-1", "NF-2"} { // same chat, any subject
				if _, err := e.send(ctx, "k"+string(rune('2'+i)), "x", p6bRC("task", task)); err != nil {
					t.Fatal(err)
				}
				if lastThread(t, e.msg) != 0 {
					t.Fatalf("send %d went to thread %d", i, lastThread(t, e.msg))
				}
			}
			if e.client.createCount() != 1 || e.store.count() != rows || rows != 0 {
				t.Fatalf("creates=%d rows=%d, want no extra create or reservation", e.client.createCount(), e.store.count())
			}
			if len(e.msg.delivered()) != 3 || e.router.Fallbacks() != 3 {
				t.Fatalf("delivered=%d fallbacks=%d, want 3 and 3", len(e.msg.delivered()), e.router.Fallbacks())
			}
			if strings.Count(logs.String(), "sent to General ("+code+")") != 3 {
				t.Fatalf("log must carry the code once per send:\n%s", logs.String())
			}
			// Another chat and another workspace are not blocked.
			e.client.createErr = nil
			other := p6bBase()
			other.ChatID = -999
			if got := e.router.Destination(ctx, other, TopicIntent{Mode: "task", TaskID: "NF-1", ExecutionID: 12}); got.ThreadID == 0 {
				t.Fatal("block leaked to another chat")
			}
			ws2 := p6bBase()
			ws2.WorkspaceID = "ws-other"
			if got := e.router.Destination(ctx, ws2, TopicIntent{Mode: "task", TaskID: "NF-1", ExecutionID: 12}); got.ThreadID == 0 {
				t.Fatal("block leaked to another workspace")
			}
			// Expiry: after 10 minutes the next send tries again and succeeds.
			now = now.Add(2 * time.Minute)
			if _, err := e.send(ctx, "k9", "later", p6bRC("task", "NF-1")); err != nil {
				t.Fatal(err)
			}
			if lastThread(t, e.msg) == 0 || e.client.createCount() != 4 {
				t.Fatalf("after expiry thread=%d creates=%d", lastThread(t, e.msg), e.client.createCount())
			}
		})
	}
}

// TC-A12.2 (extra): the memory is cleared by NoteSent after a threaded
// send, is not set by other failures, and a send-time not-forum error
// starts it.
func TestPhase6bNonForumMemoryClearAndScope(t *testing.T) {
	ctx := withWorkflowExecution(context.Background(), 12)
	t.Run("NoteSent clears", func(t *testing.T) {
		e := newP6bEnv(t)
		now := time.Unix(5000, 0)
		e.router.now = func() time.Time { return now }
		e.client.createErr = p6bErr("topic_not_forum")
		e.router.Destination(ctx, p6bBase(), TopicIntent{Mode: "task", TaskID: "C-1", ExecutionID: 1})
		e.client.createErr = nil
		if got := e.router.Destination(ctx, p6bBase(), TopicIntent{Mode: "task", TaskID: "C-1", ExecutionID: 1}); got.ThreadID != 0 {
			t.Fatal("memory not applied")
		}
		general := p6bBase()
		e.router.NoteSent(general) // a General send proves nothing
		if got := e.router.Destination(ctx, p6bBase(), TopicIntent{Mode: "task", TaskID: "C-1", ExecutionID: 1}); got.ThreadID != 0 {
			t.Fatal("a General send cleared the memory")
		}
		threaded := p6bBase()
		threaded.ThreadID = 5
		e.router.NoteSent(threaded)
		if got := e.router.Destination(ctx, p6bBase(), TopicIntent{Mode: "task", TaskID: "C-1", ExecutionID: 1}); got.ThreadID == 0 {
			t.Fatal("NoteSent after a threaded send did not clear the memory")
		}
		var nilRouter *TopicRouter
		nilRouter.NoteSent(threaded) // must not panic
	})
	t.Run("other failures do not set it", func(t *testing.T) {
		for _, cerr := range []error{p6bUnclear(), p6bErr("rejected"), &relay.DeliveryError{Code: "rate_limited", Retryable: true}, errors.New("boom")} {
			e := newP6bEnv(t)
			e.client.createErr = cerr
			e.router.Destination(ctx, p6bBase(), TopicIntent{Mode: "task", TaskID: "O-1", ExecutionID: 1})
			e.client.createErr = nil
			e.store.mu.Lock()
			e.store.clock = e.store.clock.Add(TopicReservationLease + time.Second)
			e.store.mu.Unlock()
			if got := e.router.Destination(ctx, p6bBase(), TopicIntent{Mode: "task", TaskID: "O-1", ExecutionID: 1}); got.ThreadID == 0 {
				t.Fatalf("%v wrongly blocked the chat", cerr)
			}
		}
	})
	t.Run("send-time not forum starts it", func(t *testing.T) {
		e := newP6bEnv(t)
		rc := p6bRC("task", "S-1")
		if _, err := e.send(ctx, "k1", "one", rc); err != nil { // topic created
			t.Fatal(err)
		}
		e.msg.fail = func(to ChatRef, n int) error {
			if to.ThreadID != 0 {
				return p6bErr("topic_not_forum")
			}
			return nil
		}
		if _, err := e.send(ctx, "k2", "two", rc); err != nil {
			t.Fatal(err)
		}
		before := e.msg.tried()
		if _, err := e.send(ctx, "k3", "three", rc); err != nil {
			t.Fatal(err)
		}
		if got := e.msg.tried() - before; got != 1 || lastThread(t, e.msg) != 0 {
			t.Fatalf("after the memory: attempts=%d thread=%d, want one General attempt", got, lastThread(t, e.msg))
		}
	})
	t.Run("successful reopen clears it", func(t *testing.T) {
		e := newP6bEnv(t)
		now := time.Unix(5000, 0)
		e.router.now = func() time.Time { return now }
		e.router.block(p6bBase(), "topic_no_rights")
		now = now.Add(NonForumMemory + time.Second)
		if _, ok := e.router.blockedCode(p6bBase()); ok {
			t.Fatal("expiry not applied")
		}
		e.router.block(p6bBase(), "topic_no_rights")
		e.router.unblock(p6bBase())
		if _, ok := e.router.blockedCode(p6bBase()); ok {
			t.Fatal("unblock failed")
		}
	})
}
