package storage

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// TC-A14.1 (R2): the relay's agent report goes to the same owner chat as
// inbound updates and behavior messages: owner_chat_id, else chat_id.
func TestPhase6bRelayTargetUsesOwnerChat(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		chat   string
		thread *int64
		bad    bool
	}{
		{"owner_chat_id differs from chat_id", `{"chat_id":"-10042","owner_chat_id":-100777,"bot_token_ref":"telegram"}`, "-100777", nil, false},
		{"only owner_chat_id", `{"owner_chat_id":"-100888","bot_token_ref":"telegram"}`, "-100888", nil, false},
		{"only chat_id", `{"chat_id":"-10042","bot_token_ref":"telegram"}`, "-10042", nil, false},
		// Ruling R3: the thread is dropped when the owner chat is not chat_id.
		{"thread dropped, owner chat without chat_id", `{"owner_chat_id":-100777,"message_thread_id":5,"bot_token_ref":"telegram"}`, "-100777", nil, false},
		{"thread kept, owner chat equals chat_id", `{"chat_id":"-100777","owner_chat_id":-100777,"message_thread_id":5,"bot_token_ref":"telegram"}`, "-100777", ptrInt64(5), false},
		{"no chat at all is invalid", `{"bot_token_ref":"telegram"}`, "", nil, true},
		{"invalid owner falls to chat_id", `{"chat_id":"-10042","owner_chat_id":"abc","bot_token_ref":"telegram"}`, "-10042", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newAgentEnv(t, enabledCfg)
			now := time.Now()
			e.job(t, now)
			j, _ := e.store.Claim(e.ctx, now)
			if err := e.store.CommitOutcome(e.ctx, *j, agent.JobResult{Outcome: agent.OutcomeNotify, Text: "🧠 hi"}); err != nil {
				t.Fatal(err)
			}
			mustExec(t, e.ctx, e.pool, `UPDATE integrations SET config=$1::jsonb WHERE id=$2`, tc.config, integrationB)
			a, err := e.actions.Claim(e.ctx, time.Now().Add(time.Second))
			if err != nil || a == nil {
				t.Fatalf("claim: %+v %v", a, err)
			}
			target, err := e.actions.ResolveTarget(e.ctx, *a)
			if tc.bad {
				var de *relay.DeliveryError
				if !errors.As(err, &de) || de.Code != "invalid_target" {
					t.Fatalf("err=%v, want invalid_target", err)
				}
				return
			}
			if err != nil || target.ChatID != tc.chat || target.BotTokenRef != "telegram" {
				t.Fatalf("target=%+v err=%v, want chat %s", target, err, tc.chat)
			}
			if (tc.thread == nil) != (target.MessageThreadID == nil) || (tc.thread != nil && *tc.thread != *target.MessageThreadID) {
				t.Fatalf("thread=%v, want %v", target.MessageThreadID, tc.thread)
			}
		})
	}
}

// TC-A14.2 (R3c): a stale holder cannot set the thread id; the new holder
// can; a topic with a thread is not taken over.
func TestPhase6bStaleHolderSetTopicThreadConflicts(t *testing.T) {
	e, store := topicEnv(t)
	k := topicKey(behavior.TopicSubjectClickUpTask, "STALE")
	a, claimedA, err := store.EnsureTopic(e.ctx, k)
	if err != nil || !claimedA {
		t.Fatalf("A: claimed=%v err=%v", claimedA, err)
	}
	mustExec(t, e.ctx, e.pool, `UPDATE telegram_topics SET reserved_at = now() - interval '61 seconds'`)
	b, claimedB, err := store.EnsureTopic(e.ctx, k)
	if err != nil || !claimedB || b.ID != a.ID || b.ReservedAt.Equal(a.ReservedAt) {
		t.Fatalf("B: %+v claimed=%v err=%v", b, claimedB, err)
	}
	if _, err := store.SetTopicThread(e.ctx, workspaceA, a.ID, 11, a.ReservedAt); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("stale holder err=%v, want ErrConflict", err)
	}
	if got, _ := store.TopicBySubject(e.ctx, k); got.ThreadID != 0 {
		t.Fatalf("stale holder wrote thread %d", got.ThreadID)
	}
	if set, err := store.SetTopicThread(e.ctx, workspaceA, a.ID, 12, b.ReservedAt); err != nil || set.ThreadID != 12 {
		t.Fatalf("current holder: %+v err=%v", set, err)
	}
	if _, err := store.SetTopicThread(e.ctx, workspaceA, a.ID, 13, b.ReservedAt); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("second set err=%v", err)
	}
	// Concurrent: after expiry one reclaimer wins, and only its thread sticks.
	k2 := topicKey(behavior.TopicSubjectWorkflow, "99")
	if _, c, _ := store.EnsureTopic(e.ctx, k2); !c {
		t.Fatal("setup")
	}
	mustExec(t, e.ctx, e.pool, `UPDATE telegram_topics SET reserved_at = now() - interval '61 seconds' WHERE subject_key='99'`)
	var mu sync.Mutex
	var winners []behavior.Topic
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if top, c, err := store.EnsureTopic(e.ctx, k2); err == nil && c {
				mu.Lock()
				winners = append(winners, top)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(winners) != 1 {
		t.Fatalf("winners=%d", len(winners))
	}
	if _, err := store.SetTopicThread(e.ctx, workspaceA, winners[0].ID, 21, winners[0].ReservedAt); err != nil {
		t.Fatal(err)
	}
}

// TC-A15.1 (R4): Down folds topic conversations into the thread-0 one:
// messages, instructions and bindings survive, an existing thread-0 binding
// does not conflict, a chat with only topics gets a thread-0 conversation,
// summaries of topics are dropped, and Up works again.
func TestPhase6bDownFoldsTopicConversations(t *testing.T) {
	e, _ := topicEnv(t)
	convs, err := NewConversationStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	version := e.enabledVersion(workspaceA, "p6b-fold")
	ex1 := e.start(workspaceA, version.ID, "p6b-fold-1", chainDef("one"))
	ex2 := e.start(workspaceA, version.ID, "p6b-fold-2", chainDef("one"))
	mk := func(chat, thread int64) workflow.Conversation {
		c, err := convs.EnsureConversation(e.ctx, workflow.Conversation{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: chat, ThreadID: thread})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	g, t1, t2, t3 := mk(7001, 0), mk(7001, 11), mk(7001, 12), mk(7002, 5)
	for c, s := range map[int64]string{g.ID: "keep general", t1.ID: "drop topic one"} {
		if err := convs.SetSummary(e.ctx, workspaceA, c, s); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range []workflow.Conversation{g, t1, t2, t3} {
		if _, err := convs.RecordMessage(e.ctx, workflow.Message{WorkspaceID: workspaceA, ConversationID: c.ID, Direction: "out", Kind: "report", TelegramMessageID: int64(60 + i)}); err != nil {
			t.Fatal(err)
		}
	}
	bind := func(c workflow.Conversation, ex workflow.Execution, src string) {
		if err := convs.BindWorkflow(e.ctx, workspaceA, c.ID, ex.ID, src); err != nil {
			t.Fatal(err)
		}
	}
	bind(g, ex1, "report")
	bind(t1, ex1, "report") // same execution already bound to General
	bind(t1, ex2, "approval")
	bind(t2, ex2, "report") // same execution bound by two topics
	instr := func(ex workflow.Execution, version int, c workflow.Conversation, update int64) {
		mustExec(t, e.ctx, e.pool, `INSERT INTO workflow_instructions(workspace_id,execution_id,version,text,conversation_id,telegram_update_id) VALUES($1,$2,$3,'instr',$4,$5)`, workspaceA, ex.ID, version, c.ID, update)
	}
	instr(ex1, 1, t1, 1)
	instr(ex1, 2, t2, 2)
	instr(ex2, 1, t3, 3)

	if _, err := e.pool.Exec(e.ctx, downSection(t, "00025_telegram_topics.sql")); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if got := e.count(`SELECT count(*) FROM conversations`); got != 2 {
		t.Fatalf("conversations=%d, want 2 (chat 7001 and chat 7002)", got)
	}
	var gen, only int64
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM conversations WHERE chat_id=7001`).Scan(&gen); err != nil || gen != g.ID {
		t.Fatalf("chat 7001 conversation=%d err=%v, want the existing %d", gen, err, g.ID)
	}
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM conversations WHERE chat_id=7002`).Scan(&only); err != nil {
		t.Fatal(err)
	}
	if got := e.count(`SELECT count(*) FROM conversation_messages WHERE conversation_id=$1`, gen); got != 3 {
		t.Fatalf("chat 7001 messages=%d, want 3", got)
	}
	if got := e.count(`SELECT count(*) FROM conversation_messages WHERE conversation_id=$1`, only); got != 1 {
		t.Fatalf("chat 7002 messages=%d, want 1", got)
	}
	if got := e.count(`SELECT count(*) FROM conversation_messages`); got != 4 {
		t.Fatalf("messages=%d, want 4", got)
	}
	// Instruction versions are intact and now point at thread-0 conversations.
	rows, err := e.pool.Query(e.ctx, `SELECT execution_id, version, conversation_id FROM workflow_instructions ORDER BY execution_id, version`)
	if err != nil {
		t.Fatal(err)
	}
	type ins struct{ ex, v, c int64 }
	var gotIns []ins
	for rows.Next() {
		var i ins
		if err := rows.Scan(&i.ex, &i.v, &i.c); err != nil {
			t.Fatal(err)
		}
		gotIns = append(gotIns, i)
	}
	rows.Close()
	wantIns := []ins{{ex1.ID, 1, gen}, {ex1.ID, 2, gen}, {ex2.ID, 1, only}}
	if len(gotIns) != 3 || gotIns[0] != wantIns[0] || gotIns[1] != wantIns[1] || gotIns[2] != wantIns[2] {
		t.Fatalf("instructions=%v, want %v", gotIns, wantIns)
	}
	// Bindings: each execution once, on the thread-0 conversation.
	if got := e.count(`SELECT count(*) FROM conversation_workflows WHERE conversation_id=$1 AND execution_id=$2`, gen, ex1.ID); got != 1 {
		t.Fatalf("ex1 bindings=%d", got)
	}
	if got := e.count(`SELECT count(*) FROM conversation_workflows WHERE conversation_id=$1 AND execution_id=$2`, gen, ex2.ID); got != 1 {
		t.Fatalf("ex2 bindings=%d", got)
	}
	if got := e.count(`SELECT count(*) FROM conversation_workflows`); got != 2 {
		t.Fatalf("bindings=%d, want 2", got)
	}
	// Summaries: General keeps its own; topic summaries are dropped.
	var s1, s2 string
	if err := e.pool.QueryRow(e.ctx, `SELECT summary FROM conversations WHERE id=$1`, gen).Scan(&s1); err != nil || s1 != "keep general" {
		t.Fatalf("general summary=%q err=%v", s1, err)
	}
	if err := e.pool.QueryRow(e.ctx, `SELECT summary FROM conversations WHERE id=$1`, only).Scan(&s2); err != nil || s2 != "" {
		t.Fatalf("new conversation summary=%q err=%v", s2, err)
	}
	if _, err := e.pool.Exec(e.ctx, upSection(t, "00025_telegram_topics.sql")); err != nil {
		t.Fatalf("Up after Down: %v", err)
	}
}

func ptrInt64(v int64) *int64 { return &v }
