package storage

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// Phase 6b batch A: topic storage, conversation threads, inbound thread id.

const (
	integrationA2 = "019a6690-abcd-7000-8000-000000000013"
	integrationB1 = "019a6690-abcd-7000-8000-000000000014"
)

func topicEnv(t *testing.T) (*wfEnv, *TelegramTopicStore) {
	t.Helper()
	e := newWFEnv(t)
	mustExec(t, e.ctx, e.pool, `INSERT INTO integrations(id,workspace_id,platform) VALUES($1,$2,'telegram'),($3,$2,'telegram'),($4,$5,'telegram')`,
		integrationA, workspaceA, integrationA2, integrationB1, workspaceB)
	store, err := NewTelegramTopicStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	return e, store
}

func topicKey(kind, key string) behavior.TopicKey {
	return behavior.TopicKey{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: -1001, SubjectKind: kind, SubjectKey: key}
}

func downSection(t *testing.T, file string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(here), "../../../db/migrations", file))
	if err != nil {
		t.Fatal(err)
	}
	_, down, ok := strings.Cut(string(raw), "-- +goose Down")
	if !ok {
		t.Fatal("no Down section")
	}
	return down
}

func upSection(t *testing.T, file string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(here), "../../../db/migrations", file))
	if err != nil {
		t.Fatal(err)
	}
	up, _, _ := strings.Cut(string(raw), "-- +goose Down")
	return up
}

// TC-A1.1 (REQ-1): migration 00025 constraints and RLS.
func TestPhase6bTopicSchemaConstraints(t *testing.T) {
	e, _ := topicEnv(t)
	ins := func(sql string, args ...any) error {
		_, err := e.pool.Exec(e.ctx, sql, args...)
		return err
	}
	const base = `INSERT INTO telegram_topics(workspace_id,integration_id,chat_id,thread_id,subject_kind,subject_key,state,closed_at) VALUES`
	if err := ins(base+`($1,$2,-1001,5,'clickup_task','T1','open',NULL)`, workspaceA, integrationA); err != nil {
		t.Fatal(err)
	}
	requirePGCode(t, ins(base+`($1,$2,-1001,6,'clickup_task','T1','open',NULL)`, workspaceA, integrationA), "23505") // subject key unique
	requirePGCode(t, ins(base+`($1,$2,-1001,5,'clickup_task','T2','open',NULL)`, workspaceA, integrationA), "23505") // thread unique
	requirePGCode(t, ins(base+`($1,$2,-1001,0,'clickup_task','T3','open',NULL)`, workspaceA, integrationA), "23514") // thread > 0
	requirePGCode(t, ins(base+`($1,$2,-1001,-4,'clickup_task','T3','open',NULL)`, workspaceA, integrationA), "23514")
	requirePGCode(t, ins(base+`($1,$2,-1001,7,'bogus','T3','open',NULL)`, workspaceA, integrationA), "23514")
	requirePGCode(t, ins(base+`($1,$2,-1001,7,'fixed','   ','open',NULL)`, workspaceA, integrationA), "23514")
	requirePGCode(t, ins(base+`($1,$2,-1001,7,'fixed',repeat('x',129),'open',NULL)`, workspaceA, integrationA), "23514")
	requirePGCode(t, ins(base+`($1,$2,-1001,7,'fixed','F','closed',NULL)`, workspaceA, integrationA), "23514") // closed needs closed_at
	requirePGCode(t, ins(base+`($1,$2,-1001,7,'fixed','F','open',now())`, workspaceA, integrationA), "23514")
	requirePGCode(t, ins(base+`($1,$2,-1001,7,'fixed','F','weird',NULL)`, workspaceA, integrationA), "23514")
	// integration of another workspace: composite FK.
	requirePGCode(t, ins(base+`($1,$2,-1001,7,'fixed','F','open',NULL)`, workspaceA, integrationB1), "23503")
	// Two reservations (NULL thread) of different subjects coexist.
	for _, k := range []string{"R1", "R2"} {
		if err := ins(base+`($1,$2,-1001,NULL,'fixed',$3,'open',NULL)`, workspaceA, integrationA, k); err != nil {
			t.Fatal(err)
		}
	}
	var rls bool
	if err := e.pool.QueryRow(e.ctx, `SELECT relrowsecurity FROM pg_class WHERE oid='public.telegram_topics'::regclass`).Scan(&rls); err != nil || !rls {
		t.Fatalf("telegram_topics RLS enabled=%v err=%v", rls, err)
	}
	requirePGCode(t, ins(`INSERT INTO conversations(workspace_id,integration_id,chat_id,thread_id) VALUES($1,$2,9,-1)`, workspaceA, integrationA), "23514")
}

// TC-A1.2 (REQ-1): Down works on a database with data; Up can run again.
// Topic conversations carry messages and workflow bindings, as in production.
func TestPhase6bMigrationDownWithDataAndUpAgain(t *testing.T) {
	migrationDownWithData(t, true)
}

// TC-A1.3 (REQ-1): the same Down check when the topic conversation has no
// dependent rows, so the rest of Down is proven independently.
func TestPhase6bMigrationDownTopicConversationWithoutDependents(t *testing.T) {
	migrationDownWithData(t, false)
}

func migrationDownWithData(t *testing.T, topicDeps bool) {
	e, _ := topicEnv(t)
	convs, err := NewConversationStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	version := e.enabledVersion(workspaceA, "p6b-mig")
	ex := e.start(workspaceA, version.ID, "p6b-mig", chainDef("one"))
	var general, topicConv workflow.Conversation
	if general, err = convs.EnsureConversation(e.ctx, workflow.Conversation{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: 7001}); err != nil {
		t.Fatal(err)
	}
	if topicConv, err = convs.EnsureConversation(e.ctx, workflow.Conversation{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: 7001, ThreadID: 12}); err != nil {
		t.Fatal(err)
	}
	withDeps := []workflow.Conversation{general}
	if topicDeps {
		withDeps = append(withDeps, topicConv)
	}
	for _, c := range withDeps {
		if _, err := convs.RecordMessage(e.ctx, workflow.Message{WorkspaceID: workspaceA, ConversationID: c.ID, Direction: "out", Kind: "report", TelegramMessageID: 50 + c.ID, ExecutionID: ex.ID}); err != nil {
			t.Fatal(err)
		}
		if err := convs.BindWorkflow(e.ctx, workspaceA, c.ID, ex.ID, "report"); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(t, e.ctx, e.pool, `INSERT INTO telegram_topics(workspace_id,integration_id,chat_id,thread_id,subject_kind,subject_key) VALUES($1,$2,7001,12,'clickup_task','T1')`, workspaceA, integrationA)
	mustExec(t, e.ctx, e.pool, `INSERT INTO telegram_updates(workspace_id,update_id,chat_id,kind,message_thread_id) VALUES($1,1,7001,'text',12)`, workspaceA)

	if _, err := e.pool.Exec(e.ctx, downSection(t, "00025_telegram_topics.sql")); err != nil {
		t.Fatalf("Down failed on a database with data: %v", err)
	}
	if e.count(`SELECT count(*) FROM information_schema.tables WHERE table_name='telegram_topics'`) != 0 {
		t.Fatal("telegram_topics still exists after Down")
	}
	if e.count(`SELECT count(*) FROM information_schema.columns WHERE column_name IN ('thread_id','message_thread_id') AND table_name IN ('conversations','telegram_updates')`) != 0 {
		t.Fatal("thread columns survive Down")
	}
	if got := e.count(`SELECT count(*) FROM conversations WHERE id=$1`, general.ID); got != 1 {
		t.Fatalf("General conversation lost by Down: %d", got)
	}
	if got := e.count(`SELECT count(*) FROM conversation_messages WHERE conversation_id=$1`, general.ID); got != 1 {
		t.Fatalf("General message lost by Down: %d", got)
	}
	if got := e.count(`SELECT count(*) FROM conversations WHERE id=$1`, topicConv.ID); got != 0 {
		t.Fatalf("topic conversation not removed: %d", got)
	}
	requirePGCode(t, func() error {
		_, err := e.pool.Exec(e.ctx, `INSERT INTO conversations(workspace_id,integration_id,chat_id) VALUES($1,$2,7001)`, workspaceA, integrationA)
		return err
	}(), "23505") // the old unique key is back

	if _, err := e.pool.Exec(e.ctx, upSection(t, "00025_telegram_topics.sql")); err != nil {
		t.Fatalf("Up after Down: %v", err)
	}
	if got := e.count(`SELECT thread_id FROM conversations WHERE id=$1`, general.ID); got != 0 {
		t.Fatalf("General thread_id after re-Up = %d, want 0", got)
	}
}

// TC-A2.1 (REQ-1, REQ-2): exactly one concurrent caller is claimed.
func TestPhase6bEnsureTopicOneClaimerUnderConcurrency(t *testing.T) {
	e, store := topicEnv(t)
	const n = 24
	var claimed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			top, c, err := store.EnsureTopic(e.ctx, topicKey(behavior.TopicSubjectClickUpTask, "TASK1"))
			if err != nil {
				errs <- err
				return
			}
			if c {
				claimed.Add(1)
				if top.ThreadID != 0 {
					errs <- errors.New("claimed topic already has a thread id")
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if claimed.Load() != 1 {
		t.Fatalf("claimers = %d, want exactly 1", claimed.Load())
	}
	if got := e.count(`SELECT count(*) FROM telegram_topics`); got != 1 {
		t.Fatalf("rows = %d, want 1", got)
	}
}

// TC-A2.2 (REQ-1, REQ-2): thread id is set once; release only drops a
// reservation; claimed callers after completion are not claimed.
func TestPhase6bSetThreadReleaseSemantics(t *testing.T) {
	e, store := topicEnv(t)
	k := topicKey(behavior.TopicSubjectFixed, "PR approve")
	top, claimed, err := store.EnsureTopic(e.ctx, k)
	if err != nil || !claimed {
		t.Fatalf("first ensure claimed=%v err=%v", claimed, err)
	}
	if _, err := store.SetTopicThread(e.ctx, workspaceB, top.ID, 31); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("foreign workspace SetTopicThread err=%v, want ErrConflict", err)
	}
	if _, err := store.SetTopicThread(e.ctx, workspaceA, top.ID, 0); !errors.Is(err, behavior.ErrInvalid) {
		t.Fatalf("thread 0 err=%v, want ErrInvalid", err)
	}
	set, err := store.SetTopicThread(e.ctx, workspaceA, top.ID, 31)
	if err != nil || set.ThreadID != 31 {
		t.Fatalf("set=%+v err=%v", set, err)
	}
	if _, err := store.SetTopicThread(e.ctx, workspaceA, top.ID, 32); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("second SetTopicThread err=%v, want ErrConflict", err)
	}
	if got, _ := store.TopicBySubject(e.ctx, k); got.ThreadID != 31 {
		t.Fatalf("thread overwritten: %+v", got)
	}
	// Release must not delete a topic that has a thread.
	if err := store.ReleaseTopic(e.ctx, workspaceA, top.ID); err != nil {
		t.Fatal(err)
	}
	again, claimed, err := store.EnsureTopic(e.ctx, k)
	if err != nil || claimed || again.ThreadID != 31 {
		t.Fatalf("after release of a bound topic: %+v claimed=%v err=%v", again, claimed, err)
	}
	// Release of a reservation lets the next caller claim.
	k2 := topicKey(behavior.TopicSubjectWorkflow, "42")
	r, _, _ := store.EnsureTopic(e.ctx, k2)
	if err := store.ReleaseTopic(e.ctx, workspaceB, r.ID); err != nil { // foreign workspace: no effect
		t.Fatal(err)
	}
	if _, c, _ := store.EnsureTopic(e.ctx, k2); c {
		t.Fatal("foreign workspace release freed the reservation")
	}
	if err := store.ReleaseTopic(e.ctx, workspaceA, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, c, err := store.EnsureTopic(e.ctx, k2); err != nil || !c {
		t.Fatalf("after release claimed=%v err=%v", c, err)
	}
}

// TC-A2.3 (REQ-1, REQ-2): a reservation is taken over only after the lease,
// and only by one caller.
func TestPhase6bLeaseTakeover(t *testing.T) {
	e, store := topicEnv(t)
	k := topicKey(behavior.TopicSubjectClickUpTask, "STALE")
	if _, c, _ := store.EnsureTopic(e.ctx, k); !c {
		t.Fatal("first call not claimed")
	}
	mustExec(t, e.ctx, e.pool, `UPDATE telegram_topics SET reserved_at = now() - interval '59 seconds'`)
	if _, c, err := store.EnsureTopic(e.ctx, k); err != nil || c {
		t.Fatalf("takeover before the lease: claimed=%v err=%v", c, err)
	}
	mustExec(t, e.ctx, e.pool, `UPDATE telegram_topics SET reserved_at = now() - interval '61 seconds'`)
	const n = 16
	var claimed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, c, err := store.EnsureTopic(e.ctx, k); err == nil && c {
				claimed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if claimed.Load() != 1 {
		t.Fatalf("reclaimers = %d, want exactly 1", claimed.Load())
	}
	// A completed topic is never taken over, however old.
	top, _ := store.TopicBySubject(e.ctx, k)
	if _, err := store.SetTopicThread(e.ctx, workspaceA, top.ID, 77); err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.ctx, e.pool, `UPDATE telegram_topics SET reserved_at = now() - interval '1 day'`)
	if got, c, err := store.EnsureTopic(e.ctx, k); err != nil || c || got.ThreadID != 77 {
		t.Fatalf("bound topic after lease: %+v claimed=%v err=%v", got, c, err)
	}
}

// TC-A2.4 (REQ-1, REQ-2): lookups and writes never cross workspace,
// integration or chat, and invalid keys are refused.
func TestPhase6bTopicLookupsAreScoped(t *testing.T) {
	e, store := topicEnv(t)
	k := topicKey(behavior.TopicSubjectClickUpTask, "SCOPE")
	top, _, _ := store.EnsureTopic(e.ctx, k)
	if _, err := store.SetTopicThread(e.ctx, workspaceA, top.ID, 9); err != nil {
		t.Fatal(err)
	}
	other := []behavior.TopicKey{k, k, k}
	other[0].WorkspaceID, other[0].IntegrationID = workspaceB, integrationB1
	other[1].IntegrationID = integrationA2
	other[2].ChatID = -2002
	for i, o := range other {
		if _, err := store.TopicBySubject(e.ctx, o); !errors.Is(err, behavior.ErrNotFound) {
			t.Errorf("case %d: TopicBySubject err=%v, want ErrNotFound", i, err)
		}
	}
	if _, err := store.TopicByThread(e.ctx, workspaceB, integrationA, -1001, 9); !errors.Is(err, behavior.ErrNotFound) {
		t.Errorf("other workspace by thread err=%v", err)
	}
	if _, err := store.TopicByThread(e.ctx, workspaceA, integrationA2, -1001, 9); !errors.Is(err, behavior.ErrNotFound) {
		t.Errorf("other integration by thread err=%v", err)
	}
	if _, err := store.TopicByThread(e.ctx, workspaceA, integrationA, -2002, 9); !errors.Is(err, behavior.ErrNotFound) {
		t.Errorf("other chat by thread err=%v", err)
	}
	if _, err := store.TopicByThread(e.ctx, workspaceA, integrationA, -1001, 0); !errors.Is(err, behavior.ErrNotFound) {
		t.Errorf("thread 0 must find nothing, err=%v", err)
	}
	if got, err := store.TopicByThread(e.ctx, workspaceA, integrationA, -1001, 9); err != nil || got.ID != top.ID {
		t.Fatalf("own lookup: %+v err=%v", got, err)
	}
	if _, err := store.SetTopicState(e.ctx, workspaceB, top.ID, behavior.TopicClosed); !errors.Is(err, behavior.ErrNotFound) {
		t.Errorf("foreign SetTopicState err=%v", err)
	}
	if list, _ := store.ListTopics(e.ctx, workspaceB); len(list) != 0 {
		t.Errorf("workspace B lists %d topics", len(list))
	}
	if err := store.DeleteTopic(e.ctx, workspaceB, top.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := store.TopicBySubject(e.ctx, k); err != nil || got.ID != top.ID {
		t.Fatalf("foreign DeleteTopic removed the row: %v", err)
	}
	// state round trip
	closed, err := store.SetTopicState(e.ctx, workspaceA, top.ID, behavior.TopicClosed)
	if err != nil || closed.State != behavior.TopicClosed || closed.ClosedAt == nil {
		t.Fatalf("close: %+v err=%v", closed, err)
	}
	reopened, err := store.SetTopicState(e.ctx, workspaceA, top.ID, behavior.TopicOpen)
	if err != nil || reopened.State != behavior.TopicOpen || reopened.ClosedAt != nil {
		t.Fatalf("reopen: %+v err=%v", reopened, err)
	}
	// invalid keys
	bad := []behavior.TopicKey{
		{WorkspaceID: "nope", IntegrationID: integrationA, ChatID: 1, SubjectKind: "fixed", SubjectKey: "x"},
		{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: 0, SubjectKind: "fixed", SubjectKey: "x"},
		{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: 1, SubjectKind: "other", SubjectKey: "x"},
		{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: 1, SubjectKind: "fixed", SubjectKey: ""},
		{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: 1, SubjectKind: "fixed", SubjectKey: strings.Repeat("x", 129)},
	}
	for i, b := range bad {
		if _, _, err := store.EnsureTopic(e.ctx, b); !errors.Is(err, behavior.ErrInvalid) {
			t.Errorf("bad key %d: err=%v, want ErrInvalid", i, err)
		}
	}
	// DeleteTopic forgets the binding, next ensure claims again.
	if err := store.DeleteTopic(e.ctx, workspaceA, top.ID); err != nil {
		t.Fatal(err)
	}
	if _, c, _ := store.EnsureTopic(e.ctx, k); !c {
		t.Fatal("after DeleteTopic the next ensure must claim")
	}
}

// TC-A4.1 (REQ-4): General and each topic have their own conversation and
// summary; the thread id round-trips.
func TestPhase6bConversationPerThread(t *testing.T) {
	e, _ := topicEnv(t)
	convs, err := NewConversationStore(e.pool)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(thread int64) workflow.Conversation {
		c, err := convs.EnsureConversation(e.ctx, workflow.Conversation{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: 55, ThreadID: thread})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	g, t1, t2 := mk(0), mk(11), mk(12)
	if g.ID == t1.ID || t1.ID == t2.ID || g.ID == t2.ID {
		t.Fatalf("conversations share ids: %d %d %d", g.ID, t1.ID, t2.ID)
	}
	if g.ThreadID != 0 || t1.ThreadID != 11 || t2.ThreadID != 12 {
		t.Fatalf("thread ids: %d %d %d", g.ThreadID, t1.ThreadID, t2.ThreadID)
	}
	if again := mk(11); again.ID != t1.ID {
		t.Fatalf("ensure not idempotent for a topic: %d vs %d", again.ID, t1.ID)
	}
	for c, s := range map[int64]string{g.ID: "general fact", t1.ID: "topic one fact", t2.ID: "topic two fact"} {
		if err := convs.SetSummary(e.ctx, workspaceA, c, s); err != nil {
			t.Fatal(err)
		}
	}
	for c, want := range map[int64]string{g.ID: "general fact", t1.ID: "topic one fact", t2.ID: "topic two fact"} {
		if got, err := convs.GetSummary(e.ctx, workspaceA, c); err != nil || got != want {
			t.Fatalf("summary of %d = %q err=%v, want %q", c, got, err, want)
		}
	}
	if _, err := convs.EnsureConversation(e.ctx, workflow.Conversation{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: 55, ThreadID: -3}); !errors.Is(err, workflow.ErrInvalidInput) {
		t.Fatalf("negative thread err=%v, want ErrInvalidInput", err)
	}
}

// TC-A3.3 (REQ-3): the inbound topic id is stored with the update and read
// back on claim; 0 is stored as NULL.
func TestPhase6bInboundThreadStoredAndClaimed(t *testing.T) {
	e, _ := topicEnv(t)
	now := time.Now().Add(time.Minute)
	rec := func(id, thread int64) {
		ok, err := e.behaviors.RecordUpdate(e.ctx, workspaceA, behavior.InboundUpdate{
			IntegrationID: integrationA, UpdateID: id, ChatID: -1001, ThreadID: thread, MessageID: 5, Kind: behavior.UpdateText,
		})
		if err != nil || !ok {
			t.Fatalf("record %d: ok=%v err=%v", id, ok, err)
		}
	}
	rec(1, 0)
	rec(2, 77)
	if got := e.count(`SELECT count(*) FROM telegram_updates WHERE update_id=1 AND message_thread_id IS NULL`); got != 1 {
		t.Fatal("thread 0 must be stored as NULL")
	}
	var seen = map[int64]int64{}
	for i := 0; i < 2; i++ {
		u, err := e.behaviors.ClaimUpdate(e.ctx, now, time.Minute)
		if err != nil || u == nil {
			t.Fatalf("claim %d: %v %v", i, u, err)
		}
		seen[u.UpdateID] = u.ThreadID
	}
	if seen[1] != 0 || seen[2] != 77 {
		t.Fatalf("claimed thread ids: %v", seen)
	}
}
