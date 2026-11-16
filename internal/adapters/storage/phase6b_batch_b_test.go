package storage

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// Phase 6b batch B: ClickUp notice target (R5) and the status observer
// (REQ-9). DB tests use only TEST_DATABASE_URL via testDB.

func b1StatusEvent(key, task, to, typ string) domain.NormalizedEvent {
	e := noticeEvent(key, task)
	e.EventType = domain.EventStatusChanged
	e.Content = "open → " + to
	e.Metadata = map[string]string{"webhook_id": "w1", "status_to": to}
	if typ != "" {
		e.Metadata["status_type"] = typ
	}
	return e
}

type b1Seen struct{ ws, task, status, typ string }

type b1Observer struct {
	mu   sync.Mutex
	seen []b1Seen
	// at is called inside the observer, to look at the database.
	at func()
}

func (o *b1Observer) observe(ws, task, status, typ string) {
	if o.at != nil {
		o.at()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, b1Seen{ws, task, status, typ})
}

func (o *b1Observer) got() []b1Seen {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]b1Seen(nil), o.seen...)
}

// TC-B1-80 (R5): the notice for a ClickUp event goes to owner_chat_id when the
// Telegram integration has one, else to chat_id; a static thread is kept.
func TestPhase6bB1_NoticeTargetUsesOwnerChat(t *testing.T) {
	for _, tc := range []struct {
		name, config, chat string
		thread             *int64
		bad                bool
	}{
		{"chat_id only", `{"chat_id":"-10042","bot_token_env":"TOKEN"}`, "-10042", nil, false},
		{"owner_chat_id wins", `{"chat_id":"-10042","owner_chat_id":-1001234567890,"bot_token_env":"TOKEN"}`, "-1001234567890", nil, false},
		{"owner_chat_id as string", `{"chat_id":"-10042","owner_chat_id":"-100777","bot_token_env":"TOKEN"}`, "-100777", nil, false},
		{"owner_chat_id alone", `{"owner_chat_id":-100888,"bot_token_env":"TOKEN"}`, "-100888", nil, false},
		{"owner_chat_id null falls back", `{"chat_id":"-10042","owner_chat_id":null,"bot_token_env":"TOKEN"}`, "-10042", nil, false},
		{"invalid owner falls back", `{"chat_id":"-10042","owner_chat_id":"abc","bot_token_env":"TOKEN"}`, "-10042", nil, false},
		{"zero owner falls back", `{"chat_id":"-10042","owner_chat_id":0,"bot_token_env":"TOKEN"}`, "-10042", nil, false},
		{"static thread kept", `{"owner_chat_id":-100777,"message_thread_id":5,"bot_token_env":"TOKEN"}`, "-100777", ptrInt64(5), false},
		{"no chat at all", `{"bot_token_env":"TOKEN"}`, "", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, pool, r := newNoticeDB(t)
			mustExec(t, ctx, pool, `UPDATE integrations SET config=$1::jsonb WHERE id=$2`, tc.config, integrationB)
			if tc.bad {
				// A target without any chat is refused at ingest or at resolve.
				if _, err := r.Accept(ctx, []domain.NormalizedEvent{noticeEvent("h1", "task-1")}); err != nil {
					return
				}
			} else if _, err := r.Accept(ctx, []domain.NormalizedEvent{noticeEvent("h1", "task-1")}); err != nil {
				t.Fatal(err)
			}
			job, err := r.Claim(ctx, time.Now().Add(time.Second))
			if tc.bad {
				if err == nil && job != nil {
					if _, err := r.ResolveTarget(ctx, *job); err == nil {
						t.Fatal("a target without a chat resolved")
					}
				}
				return
			}
			if err != nil || job == nil {
				t.Fatalf("claim: %+v %v", job, err)
			}
			target, err := r.ResolveTarget(ctx, *job)
			if err != nil || target.ChatID != tc.chat || target.BotTokenRef == "" {
				t.Fatalf("target=%+v err=%v want chat %s", target, err, tc.chat)
			}
			if (tc.thread == nil) != (target.MessageThreadID == nil) || (tc.thread != nil && *tc.thread != *target.MessageThreadID) {
				t.Fatalf("thread=%v want %v", target.MessageThreadID, tc.thread)
			}
		})
	}
}

// TC-B1-81 (R5): resolving a target does not rewrite the stored config.
func TestPhase6bB1_NoticeTargetDoesNotMutateConfig(t *testing.T) {
	ctx, pool, r := newNoticeDB(t)
	const cfg = `{"chat_id":"-10042","owner_chat_id":-100777,"bot_token_env":"TOKEN"}`
	mustExec(t, ctx, pool, `UPDATE integrations SET config=$1::jsonb WHERE id=$2`, cfg, integrationB)
	if _, err := r.Accept(ctx, []domain.NormalizedEvent{noticeEvent("h1", "task-1")}); err != nil {
		t.Fatal(err)
	}
	job, _ := r.Claim(ctx, time.Now().Add(time.Second))
	if _, err := r.ResolveTarget(ctx, *job); err != nil {
		t.Fatal(err)
	}
	var chat, owner string
	if err := pool.QueryRow(ctx, `SELECT config->>'chat_id', config->>'owner_chat_id' FROM integrations WHERE id=$1`, integrationB).Scan(&chat, &owner); err != nil || chat != "-10042" || owner != "-100777" {
		t.Fatalf("config chat=%q owner=%q err=%v", chat, owner, err)
	}
}

// TC-B1-82 (REQ-9): the status observer runs after commit, once per status
// event with a target status, with identifiers and status words only.
func TestPhase6bB1_StatusObserverAfterCommit(t *testing.T) {
	ctx, pool, r := newNoticeDB(t)
	obs := &b1Observer{}
	var rowsAtCall int
	obs.at = func() { rowsAtCall = countRows(t, context.Background(), pool, "notification_outbox") }
	r.SetStatusObserver(obs.observe)
	events := []domain.NormalizedEvent{
		b1StatusEvent("h1", "task-1", "Complete", "done"),
		b1StatusEvent("h2", "task-2", "in review", ""),
		noticeEvent("h3", "task-3"), // a comment: never observed
	}
	noStatus := b1StatusEvent("h4", "task-4", "x", "")
	delete(noStatus.Metadata, "status_to")
	events = append(events, noStatus)
	if _, err := r.Accept(ctx, events); err != nil {
		t.Fatal(err)
	}
	got := obs.got()
	if len(got) != 2 {
		t.Fatalf("observer calls=%+v", got)
	}
	want := map[string]b1Seen{
		"task-1": {workspaceA, "task-1", "Complete", "done"},
		"task-2": {workspaceA, "task-2", "in review", ""},
	}
	for _, g := range got {
		if g != want[g.task] {
			t.Fatalf("call=%+v want %+v", g, want[g.task])
		}
	}
	if rowsAtCall != 4 {
		t.Fatalf("observer ran before the commit: %d outbox rows visible", rowsAtCall)
	}
	// The observer gets no message text or sender.
	for _, g := range got {
		if strings.Contains(g.ws+g.task+g.status+g.typ, "original Việt") || strings.Contains(g.ws+g.task, "Author") {
			t.Fatalf("observer got text: %+v", g)
		}
	}
}

// TC-B1-83 (REQ-9, abuse): a rolled-back callback never reaches the observer;
// a repository without an observer works; the setter can be cleared.
func TestPhase6bB1_StatusObserverNotOnRollback(t *testing.T) {
	for _, mode := range []string{"source disabled", "target disabled", "db failure", "mixed source", "bad webhook"} {
		t.Run(mode, func(t *testing.T) {
			ctx, pool, r := newNoticeDB(t)
			obs := &b1Observer{}
			r.SetStatusObserver(obs.observe)
			events := []domain.NormalizedEvent{b1StatusEvent("h1", "task-1", "Complete", "done")}
			switch mode {
			case "source disabled":
				mustExec(t, ctx, pool, `UPDATE integrations SET is_enabled=false WHERE id=$1`, integrationA)
			case "target disabled":
				mustExec(t, ctx, pool, `UPDATE integrations SET is_enabled=false WHERE id=$1`, integrationB)
			case "db failure":
				mustExec(t, ctx, pool, `ALTER TABLE notification_outbox ADD CONSTRAINT reject_job CHECK (event_key='never')`)
			case "mixed source":
				e := b1StatusEvent("h2", "task-1", "Complete", "done")
				e.IntegrationID = missingID
				events = append(events, e)
			case "bad webhook":
				events[0].Metadata["webhook_id"] = "other"
			}
			if _, err := r.Accept(ctx, events); err == nil {
				t.Fatal("invalid acceptance succeeded")
			}
			if got := obs.got(); len(got) != 0 {
				t.Fatalf("observer ran for a failed callback: %+v", got)
			}
		})
	}
	t.Run("no observer", func(t *testing.T) {
		ctx, _, r := newNoticeDB(t)
		if _, err := r.Accept(ctx, []domain.NormalizedEvent{b1StatusEvent("h1", "task-1", "Complete", "done")}); err != nil {
			t.Fatal(err)
		}
		r.SetStatusObserver(nil)
		if _, err := r.Accept(ctx, []domain.NormalizedEvent{b1StatusEvent("h2", "task-1", "Complete", "done")}); err != nil {
			t.Fatal(err)
		}
	})
}
