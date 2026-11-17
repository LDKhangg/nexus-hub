package storage

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// Test debt TD-TT-2 (link claim store), TD-TT-4 (remembered repo query) and
// TD-TT-6 (migration 00027).

func (e *wfEnv) linkTopic(t *testing.T, s *TelegramTopicStore, key string, thread int64) behavior.Topic {
	t.Helper()
	k := topicKey(behavior.TopicSubjectClickUpTask, key)
	topic, _, err := s.EnsureTopic(e.ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	if thread > 0 {
		topic, err = s.SetTopicThread(e.ctx, workspaceA, topic.ID, thread, topic.ReservedAt)
		if err != nil {
			t.Fatal(err)
		}
	}
	return topic
}

func (e *wfEnv) linkValue(t *testing.T, id int64) (v *int64) {
	t.Helper()
	if err := e.pool.QueryRow(e.ctx, `SELECT link_message_id FROM telegram_topics WHERE id=$1`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TC-TT-2f (TD-TT-2): the claim is NULL -> 0 -> message id, once per topic.
func TestTD_TT2_LinkClaimLifecycle(t *testing.T) {
	e, s := topicEnv(t)
	noThread := e.linkTopic(t, s, "86a0", 0)
	if ok, err := s.ClaimTopicLink(e.ctx, workspaceA, noThread.ID); err != nil || ok {
		t.Fatalf("claim before the thread exists: %v %v", ok, err)
	}
	topic := e.linkTopic(t, s, "86a1", 11)
	if v := e.linkValue(t, topic.ID); v != nil {
		t.Fatalf("new topic link=%v, want NULL", *v)
	}
	// Setting a link that was never claimed is a conflict.
	if err := s.SetTopicLink(e.ctx, workspaceA, topic.ID, 700); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("set without claim: %v", err)
	}
	if ok, err := s.ClaimTopicLink(e.ctx, workspaceA, topic.ID); err != nil || !ok {
		t.Fatalf("first claim: %v %v", ok, err)
	}
	if ok, err := s.ClaimTopicLink(e.ctx, workspaceA, topic.ID); err != nil || ok {
		t.Fatalf("second claim: %v %v", ok, err)
	}
	if v := e.linkValue(t, topic.ID); v == nil || *v != 0 {
		t.Fatalf("claimed link=%v, want 0", v)
	}
	if err := s.SetTopicLink(e.ctx, workspaceA, topic.ID, 700); err != nil {
		t.Fatal(err)
	}
	if v := e.linkValue(t, topic.ID); v == nil || *v != 700 {
		t.Fatalf("link=%v, want 700", v)
	}
	// Once recorded, nothing changes it: no second set, no claim, release is a no-op.
	if err := s.SetTopicLink(e.ctx, workspaceA, topic.ID, 701); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("second set: %v", err)
	}
	if ok, _ := s.ClaimTopicLink(e.ctx, workspaceA, topic.ID); ok {
		t.Fatal("claimed a posted link")
	}
	if err := s.ReleaseTopicLink(e.ctx, workspaceA, topic.ID); err != nil {
		t.Fatal(err)
	}
	if v := e.linkValue(t, topic.ID); v == nil || *v != 700 {
		t.Fatalf("release changed a posted link: %v", v)
	}
	// A released claim can be taken again.
	other := e.linkTopic(t, s, "86a2", 12)
	if ok, _ := s.ClaimTopicLink(e.ctx, workspaceA, other.ID); !ok {
		t.Fatal("claim")
	}
	if err := s.ReleaseTopicLink(e.ctx, workspaceA, other.ID); err != nil {
		t.Fatal(err)
	}
	if v := e.linkValue(t, other.ID); v != nil {
		t.Fatalf("released link=%v, want NULL", *v)
	}
	if ok, _ := s.ClaimTopicLink(e.ctx, workspaceA, other.ID); !ok {
		t.Fatal("claim after release")
	}
}

// TC-TT-2g (TD-TT-2): ids are validated and scoped to the workspace.
func TestTD_TT2_LinkClaimValidationAndScope(t *testing.T) {
	e, s := topicEnv(t)
	topic := e.linkTopic(t, s, "86a1", 11)
	if _, err := s.ClaimTopicLink(e.ctx, "not-a-uuid", topic.ID); !errors.Is(err, behavior.ErrInvalid) {
		t.Fatalf("bad workspace: %v", err)
	}
	for _, id := range []int64{0, -1} {
		if _, err := s.ClaimTopicLink(e.ctx, workspaceA, id); !errors.Is(err, behavior.ErrInvalid) {
			t.Fatalf("claim id %d: %v", id, err)
		}
		if err := s.SetTopicLink(e.ctx, workspaceA, id, 5); !errors.Is(err, behavior.ErrInvalid) {
			t.Fatalf("set id %d: %v", id, err)
		}
		if err := s.ReleaseTopicLink(e.ctx, workspaceA, id); !errors.Is(err, behavior.ErrInvalid) {
			t.Fatalf("release id %d: %v", id, err)
		}
	}
	if ok, _ := s.ClaimTopicLink(e.ctx, workspaceA, topic.ID); !ok {
		t.Fatal("claim")
	}
	for _, m := range []int64{0, -5} {
		if err := s.SetTopicLink(e.ctx, workspaceA, topic.ID, m); !errors.Is(err, behavior.ErrInvalid) {
			t.Fatalf("message id %d: %v", m, err)
		}
	}
	// Workspace B can neither claim, set nor release workspace A's topic.
	if ok, err := s.ClaimTopicLink(e.ctx, workspaceB, topic.ID); err != nil || ok {
		t.Fatalf("B claim: %v %v", ok, err)
	}
	if err := s.SetTopicLink(e.ctx, workspaceB, topic.ID, 9); !errors.Is(err, behavior.ErrConflict) {
		t.Fatalf("B set: %v", err)
	}
	if err := s.ReleaseTopicLink(e.ctx, workspaceB, topic.ID); err != nil {
		t.Fatal(err)
	}
	if v := e.linkValue(t, topic.ID); v == nil || *v != 0 {
		t.Fatalf("A's claim changed by B: %v", v)
	}
	// The column refuses negative ids.
	_, err := e.pool.Exec(e.ctx, `UPDATE telegram_topics SET link_message_id=-1 WHERE id=$1`, topic.ID)
	requirePGCode(t, err, "23514")
}

// TC-TT-2h (TD-TT-2): exactly one concurrent caller wins the claim.
func TestTD_TT2_LinkClaimConcurrent(t *testing.T) {
	e, s := topicEnv(t)
	topic := e.linkTopic(t, s, "86a1", 11)
	var wins int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := s.ClaimTopicLink(e.ctx, workspaceA, topic.ID); err == nil && ok {
				atomic.AddInt32(&wins, 1)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d callers won the claim", wins)
	}
}

// TC-TT-4f (TD-TT-4): LastTaskRepo returns the repo of the newest local task of one ClickUp task.
func TestTD_TT4_LastTaskRepoQuery(t *testing.T) {
	b := newB3Env(t)
	start := func(task, repo string) {
		t.Helper()
		b.update++
		if _, err := b.tasks.StartLocalTask(b.ctx, behavior.LocalTaskRequest{WorkspaceID: workspaceA, IntegrationID: integrationA,
			ChatID: 7001, UpdateID: b.update, Repo: repo, TaskRef: task, ClickUpTaskID: task}); err != nil {
			t.Fatalf("start %s %s: %v", task, repo, err)
		}
	}
	got := func(ws, task string) string {
		t.Helper()
		repo, err := b.tasks.LastTaskRepo(b.ctx, ws, task)
		if err != nil {
			t.Fatalf("LastTaskRepo(%q): %v", task, err)
		}
		return repo
	}
	if got(workspaceA, "86abc") != "" {
		t.Fatal("repo remembered before any task")
	}
	start("86abc", "acme/api")
	if r := got(workspaceA, "86abc"); r != "acme/api" {
		t.Fatalf("got %q", r)
	}
	start("86abc", "acme/web")
	if r := got(workspaceA, "86abc"); r != "acme/web" {
		t.Fatalf("newest task must win: %q", r)
	}
	start("86other", "acme/docs")
	if r := got(workspaceA, "86abc"); r != "acme/web" {
		t.Fatalf("another task changed the memory: %q", r)
	}
	if r := got(workspaceA, "86other"); r != "acme/docs" {
		t.Fatalf("got %q", r)
	}
	// Scope and bounds.
	if r := got(workspaceB, "86abc"); r != "" {
		t.Fatalf("workspace B reads A's memory: %q", r)
	}
	if r := got(workspaceA, ""); r != "" {
		t.Fatalf("empty id: %q", r)
	}
	long := make([]byte, 65)
	for i := range long {
		long[i] = 'a'
	}
	if r := got(workspaceA, string(long)); r != "" {
		t.Fatalf("over-long id: %q", r)
	}
	if r := got(workspaceA, "86nothing"); r != "" {
		t.Fatalf("unknown task: %q", r)
	}
	if _, err := b.tasks.LastTaskRepo(b.ctx, "not-a-uuid", "86abc"); !errors.Is(err, behavior.ErrInvalid) {
		t.Fatalf("bad workspace: %v", err)
	}
	// The lookup uses the index the migration adds.
	var n int
	if err := b.pool.QueryRow(b.ctx, `SELECT count(*) FROM pg_indexes WHERE indexname='worker_task_routes_clickup_task_idx'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("index count %d err %v", n, err)
	}
}

// TC-TT-6 (TD-TT-6): migration 00027 Down removes the column and the index; Up restores them.
func TestTD_TT6_Migration00027DownUp(t *testing.T) {
	e, s := topicEnv(t)
	topic := e.linkTopic(t, s, "86a1", 11)
	if ok, _ := s.ClaimTopicLink(e.ctx, workspaceA, topic.ID); !ok {
		t.Fatal("claim")
	}
	mustExec(t, e.ctx, e.pool, downSection(t, "00027_topic_link_task_repo.sql"))
	var cols, idx int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name='telegram_topics' AND column_name='link_message_id'`).Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("Down left the column: %d %v", cols, err)
	}
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM pg_indexes WHERE indexname='worker_task_routes_clickup_task_idx'`).Scan(&idx); err != nil || idx != 0 {
		t.Fatalf("Down left the index: %d %v", idx, err)
	}
	// The topic itself survives Down.
	if e.count(`SELECT count(*) FROM telegram_topics WHERE id=$1`, topic.ID) != 1 {
		t.Fatal("Down removed the topic")
	}
	up := upSection(t, "00027_topic_link_task_repo.sql")
	mustExec(t, e.ctx, e.pool, up)
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name='telegram_topics' AND column_name='link_message_id'`).Scan(&cols); err != nil || cols != 1 {
		t.Fatalf("Up did not restore the column: %d %v", cols, err)
	}
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM pg_indexes WHERE indexname='worker_task_routes_clickup_task_idx'`).Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("Up did not restore the index: %d %v", idx, err)
	}
	if v := e.linkValue(t, topic.ID); v != nil {
		t.Fatalf("restored column must start NULL, got %d", *v)
	}
	if ok, _ := s.ClaimTopicLink(e.ctx, workspaceA, topic.ID); !ok {
		t.Fatal("claim after Up")
	}
	_, err := e.pool.Exec(e.ctx, `UPDATE telegram_topics SET link_message_id=-1 WHERE id=$1`, topic.ID)
	requirePGCode(t, err, "23514")
}
