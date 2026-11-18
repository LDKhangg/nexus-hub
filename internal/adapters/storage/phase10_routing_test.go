package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Phase 10 Batch A, R1 (REQ-1, REQ-2, REQ-5): ingest routing and message
// links against PostgreSQL.

const p10NoticeChat = int64(-10042) // the chat newNoticeDB's target uses

func p10TaskTopic(t *testing.T, ctx context.Context, pool *pgxpool.Pool, task string, thread int64) *TelegramTopicStore {
	t.Helper()
	s, err := NewTelegramTopicStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	topic, _, err := s.EnsureTopic(ctx, behavior.TopicKey{WorkspaceID: workspaceA, IntegrationID: integrationB, ChatID: p10NoticeChat,
		SubjectKind: behavior.TopicSubjectClickUpTask, SubjectKey: task})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTopicThread(ctx, workspaceA, topic.ID, thread, topic.ReservedAt); err != nil {
		t.Fatal(err)
	}
	return s
}

// TC-R1a, TC-R2a (REQ-1, REQ-2): with mentions-only on, a task with a topic gets
// every comment, status and assignee event; a task without a topic keeps the filter.
func TestP10Ingest_TopicTaskBypassesMentionsOnly(t *testing.T) {
	ctx, pool, r := newNoticeDB(t)
	mustExec(t, ctx, pool, `UPDATE integrations SET config = config || '{"notice_mentions_only":true}'::jsonb WHERE id=$1`, integrationA)
	mustExec(t, ctx, pool, `INSERT INTO app_settings(key,value) VALUES ('owner_clickup_user_id','"77"')`)
	p10TaskTopic(t, ctx, pool, "task-in", 41)
	rows := func() int { return countRows(t, ctx, pool, "notification_outbox") }
	accept := func(e domain.NormalizedEvent, task string) domain.NormalizedEvent {
		e.ExternalID = task
		return e
	}
	cmt := string(domain.EventCommandPosted)
	cases := []struct {
		name string
		e    domain.NormalizedEvent
		want int // rows added
	}{
		{"untagged comment in topic", accept(nmEvent("c1", cmt, "5", nil), "task-in"), 1},
		{"untagged comment, no topic", accept(nmEvent("c2", cmt, "5", nil), "task-out"), 0},
		{"status change in topic", accept(nmEvent("s1", string(domain.EventStatusChanged), "5", map[string]string{"status_to": "done"}), "task-in"), 1},
		{"status change, no topic", accept(nmEvent("s2", string(domain.EventStatusChanged), "5", map[string]string{"status_to": "done"}), "task-out"), 0},
		{"assignee change in topic", accept(nmEvent("a1", string(domain.EventAssigneeChanged), "5", map[string]string{"assignee_added": "9"}), "task-in"), 1},
		{"assignee change, no topic", accept(nmEvent("a2", string(domain.EventAssigneeChanged), "5", map[string]string{"assignee_added": "9"}), "task-out"), 0},
		{"owner's own comment in topic", accept(nmEvent("o1", cmt, "77", nil), "task-in"), 1},
		{"comment from the hub in topic", func() domain.NormalizedEvent {
			e := accept(nmEvent("b1", cmt, "5", nil), "task-in")
			e.FromOwnBot = true
			return e
		}(), 0},
		{"comment from the hub, no topic", func() domain.NormalizedEvent {
			e := accept(nmEvent("b2", cmt, "5", nil), "task-out")
			e.FromOwnBot = true
			return e
		}(), 0},
	}
	for _, c := range cases {
		before := rows()
		if _, err := r.Accept(ctx, []domain.NormalizedEvent{c.e}); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := rows() - before; got != c.want {
			t.Errorf("%s: %d notices, want %d", c.name, got, c.want)
		}
	}
}

// TC-R2c (REQ-2, human): a webhook delivered again (even in a burst) is one notice.
func TestP10Ingest_RedeliveryIsOneNotice(t *testing.T) {
	ctx, pool, r := newNoticeDB(t)
	p10TaskTopic(t, ctx, pool, "task-in", 41)
	e := nmEvent("dup", string(domain.EventCommandPosted), "5", nil)
	e.ExternalID = "task-in"
	for i := 0; i < 4; i++ {
		if _, err := r.Accept(ctx, []domain.NormalizedEvent{e}); err != nil {
			t.Fatal(err)
		}
	}
	if n := countRows(t, ctx, pool, "notification_outbox"); n != 1 {
		t.Fatalf("%d notices for one event", n)
	}
}

// TC-R1d (REQ-1): a fixed message_thread_id target disables task routing, so the
// mentions-only filter applies as before.
func TestP10Ingest_FixedThreadTargetDisablesRouting(t *testing.T) {
	ctx, pool, r := newNoticeDB(t)
	mustExec(t, ctx, pool, `UPDATE integrations SET config = config || '{"notice_mentions_only":true}'::jsonb WHERE id=$1`, integrationA)
	mustExec(t, ctx, pool, `INSERT INTO app_settings(key,value) VALUES ('owner_clickup_user_id','"77"')`)
	mustExec(t, ctx, pool, `UPDATE integrations SET config = config || '{"message_thread_id":32}'::jsonb WHERE id=$1`, integrationB)
	p10TaskTopic(t, ctx, pool, "task-in", 41)
	e := nmEvent("fx", string(domain.EventCommandPosted), "5", nil)
	e.ExternalID = "task-in"
	res, err := r.Accept(ctx, []domain.NormalizedEvent{e})
	if err != nil || res.Accepted != 0 || countRows(t, ctx, pool, "notification_outbox") != 0 {
		t.Fatalf("fixed-thread target routed: %+v %v", res, err)
	}
}

// TC-R1a (REQ-1, abuse): a topic of another chat or workspace never counts as the task's topic.
func TestP10Ingest_TopicOfAnotherChatDoesNotCount(t *testing.T) {
	ctx, pool, r := newNoticeDB(t)
	mustExec(t, ctx, pool, `UPDATE integrations SET config = config || '{"notice_mentions_only":true}'::jsonb WHERE id=$1`, integrationA)
	mustExec(t, ctx, pool, `INSERT INTO app_settings(key,value) VALUES ('owner_clickup_user_id','"77"')`)
	s, err := NewTelegramTopicStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	topic, _, err := s.EnsureTopic(ctx, behavior.TopicKey{WorkspaceID: workspaceA, IntegrationID: integrationB, ChatID: -99999,
		SubjectKind: behavior.TopicSubjectClickUpTask, SubjectKey: "task-in"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTopicThread(ctx, workspaceA, topic.ID, 5, topic.ReservedAt); err != nil {
		t.Fatal(err)
	}
	e := nmEvent("oc", string(domain.EventCommandPosted), "5", nil)
	e.ExternalID = "task-in"
	if res, err := r.Accept(ctx, []domain.NormalizedEvent{e}); err != nil || res.Accepted != 0 {
		t.Fatalf("a topic of another chat routed the notice: %+v %v", res, err)
	}
}

// TC-R5a (REQ-5): a delivered comment notice records its message link in the same
// transaction as the sent state; a stale claim records nothing.
func TestP10Notice_MarkSentRecordsCommentLink(t *testing.T) {
	ctx, pool, r := newNoticeDB(t)
	if _, err := r.Accept(ctx, []domain.NormalizedEvent{noticeEvent("k1", "task-1")}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Second)
	first, err := r.Claim(ctx, now)
	if err != nil || first == nil {
		t.Fatal(first, err)
	}
	second, err := r.Claim(ctx, now.Add(61*time.Second))
	if err != nil || second == nil {
		t.Fatal(second, err)
	}
	if err := r.MarkSent(ctx, *first, relay.DeliveryReceipt{MessageID: 7, ChatID: p10NoticeChat, ThreadID: 41}); !errors.Is(err, relay.ErrStaleNoticeClaim) {
		t.Fatal("stale claim accepted", err)
	}
	if n := countRows(t, ctx, pool, "telegram_message_links"); n != 0 {
		t.Fatalf("stale claim left %d links", n)
	}
	if err := r.MarkSent(ctx, *second, relay.DeliveryReceipt{MessageID: 8, ChatID: p10NoticeChat, ThreadID: 41}); err != nil {
		t.Fatal(err)
	}
	s, _ := NewTelegramTopicStore(pool)
	l, err := s.LookupCommentLink(ctx, workspaceA, integrationB, p10NoticeChat, 8)
	if err != nil || l.TaskID != "task-1" || l.CommentID != "k1" {
		t.Fatalf("link %+v err %v (comment id must be the bare id)", l, err)
	}
	if n := countRows(t, ctx, pool, "telegram_message_links"); n != 1 {
		t.Fatalf("%d links", n)
	}
}

// TC-R5a (REQ-5): notices that show no comment, or arrive without a chat, store no link.
func TestP10Notice_NoLinkForNonCommentOrNoChat(t *testing.T) {
	ctx, pool, r := newNoticeDB(t)
	status := noticeEvent("st1", "task-1")
	status.EventType = domain.EventStatusChanged
	if _, err := r.Accept(ctx, []domain.NormalizedEvent{status, noticeEvent("nc1", "task-2")}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Second)
	for i := 0; i < 2; i++ {
		job, err := r.Claim(ctx, now)
		if err != nil || job == nil {
			t.Fatalf("claim %d: %v %v", i, job, err)
		}
		receipt := relay.DeliveryReceipt{MessageID: int64(20 + i), ChatID: p10NoticeChat}
		if job.Event.EventType == domain.EventCommandPosted {
			receipt.ChatID = 0 // no chat: no link
		}
		if err := r.MarkSent(ctx, *job, receipt); err != nil {
			t.Fatal(err)
		}
	}
	if n := countRows(t, ctx, pool, "telegram_message_links"); n != 0 {
		t.Fatalf("%d links, want none", n)
	}
	if n := scalar[int](t, ctx, pool, `SELECT count(*) FROM notification_outbox WHERE status='sent'`); n != 2 {
		t.Fatalf("%d sent", n)
	}
}

// TC-R5c (REQ-5, abuse): a link is found only with its workspace, integration and chat.
func TestP10Links_LookupIsScoped(t *testing.T) {
	e, s := topicEnv(t)
	l := behavior.CommentLink{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: -1001, MessageID: 55, TaskID: "86abc", CommentID: "9001"}
	if err := s.RecordCommentLink(e.ctx, l); err != nil {
		t.Fatal(err)
	}
	// A second record of the same message keeps the first.
	l2 := l
	l2.CommentID = "9002"
	if err := s.RecordCommentLink(e.ctx, l2); err != nil {
		t.Fatal(err)
	}
	got, err := s.LookupCommentLink(e.ctx, workspaceA, integrationA, -1001, 55)
	if err != nil || got.CommentID != "9001" || got.TaskID != "86abc" {
		t.Fatalf("got %+v %v", got, err)
	}
	for name, f := range map[string]func() (behavior.CommentLink, error){
		"other chat": func() (behavior.CommentLink, error) {
			return s.LookupCommentLink(e.ctx, workspaceA, integrationA, -1002, 55)
		},
		"other message": func() (behavior.CommentLink, error) {
			return s.LookupCommentLink(e.ctx, workspaceA, integrationA, -1001, 56)
		},
		"other integration": func() (behavior.CommentLink, error) {
			return s.LookupCommentLink(e.ctx, workspaceA, integrationA2, -1001, 55)
		},
		"other workspace": func() (behavior.CommentLink, error) {
			return s.LookupCommentLink(e.ctx, workspaceB, integrationB1, -1001, 55)
		},
		"zero message": func() (behavior.CommentLink, error) {
			return s.LookupCommentLink(e.ctx, workspaceA, integrationA, -1001, 0)
		},
	} {
		if _, err := f(); !errors.Is(err, behavior.ErrNotFound) {
			t.Errorf("%s: err %v, want ErrNotFound", name, err)
		}
	}
	// Invalid ids are refused and store nothing.
	bad := l
	bad.CommentID = "a b"
	bad.MessageID = 56
	if err := s.RecordCommentLink(e.ctx, bad); err == nil {
		t.Fatal("invalid comment id stored")
	}
	if e.count(`SELECT count(*) FROM telegram_message_links`) != 1 {
		t.Fatal("rows changed")
	}
}

// TC-R5b (REQ-5): prune deletes only links older than the cutoff.
func TestP10Links_PruneByRetention(t *testing.T) {
	e, s := topicEnv(t)
	for i, age := range []string{"40 days", "31 days", "29 days", "1 day"} {
		l := behavior.CommentLink{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: -1001, MessageID: int64(10 + i), TaskID: "86abc", CommentID: "1"}
		if err := s.RecordCommentLink(e.ctx, l); err != nil {
			t.Fatal(err)
		}
		mustExec(t, e.ctx, e.pool, `UPDATE telegram_message_links SET created_at = now() - $1::interval WHERE message_id=$2`, age, l.MessageID)
	}
	n, err := s.PruneMessageLinks(e.ctx, time.Now().Add(-behavior.DefaultMessageLinkRetention))
	if err != nil || n != 2 {
		t.Fatalf("pruned %d err %v", n, err)
	}
	if left := e.count(`SELECT count(*) FROM telegram_message_links WHERE message_id IN (12,13)`); left != 2 {
		t.Fatalf("recent links removed: %d", left)
	}
	// A shorter retention prunes more; a zero time prunes nothing; running again is safe.
	if n, err := s.PruneMessageLinks(e.ctx, time.Time{}); err != nil || n != 0 {
		t.Fatalf("zero time pruned %d %v", n, err)
	}
	if n, err := s.PruneMessageLinks(e.ctx, time.Now().Add(-48*time.Hour)); err != nil || n != 1 {
		t.Fatalf("7-day-style prune removed %d %v", n, err)
	}
}

// TC-R5d (REQ-5): migration 00031 Down drops the table; Up restores it empty with its constraints.
func TestP10Migration00031DownUp(t *testing.T) {
	e, s := topicEnv(t)
	if err := s.RecordCommentLink(e.ctx, behavior.CommentLink{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: -1001, MessageID: 5, TaskID: "t", CommentID: "1"}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.ctx, e.pool, downSection(t, "00031_task_topic_outbound.sql"))
	if n := e.count(`SELECT count(*) FROM information_schema.tables WHERE table_name='telegram_message_links'`); n != 0 {
		t.Fatalf("Down left the table: %d", n)
	}
	mustExec(t, e.ctx, e.pool, upSection(t, "00031_task_topic_outbound.sql"))
	if n := e.count(`SELECT count(*) FROM telegram_message_links`); n != 0 {
		t.Fatalf("restored table has %d rows", n)
	}
	_, err := e.pool.Exec(e.ctx, `INSERT INTO telegram_message_links(workspace_id,integration_id,chat_id,message_id,subject_kind,task_id,comment_id) VALUES($1,$2,0,1,'clickup_comment','t','1')`, workspaceA, integrationA)
	requirePGCode(t, err, "23514")
}
