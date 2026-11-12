package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNoticeMigrationRollback(t *testing.T) {
	ctx, pool := testDB(t)
	_, file, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../../db/migrations/00002_create_notification_outbox.sql"))
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(data), "-- +goose Down")
	if !ok {
		t.Fatal("missing Down")
	}
	mustExec(t, ctx, pool, down)
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.notification_outbox') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatal("Down did not remove outbox")
	}
	mustExec(t, ctx, pool, up)
	if err := pool.QueryRow(ctx, `SELECT relrowsecurity FROM pg_class WHERE oid='public.notification_outbox'::regclass`).Scan(&exists); err != nil || !exists {
		t.Fatal("Up did not restore RLS")
	}
	if countRows(t, ctx, pool, "workspaces") != 0 {
		t.Fatal("unexpected parent modification")
	}
}

func newNoticeDB(t *testing.T) (context.Context, *pgxpool.Pool, *NoticeRepository) {
	t.Helper()
	ctx, pool := testDB(t)
	mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'A'),($2,'B')`, workspaceA, workspaceB)
	mustExec(t, ctx, pool, `INSERT INTO integrations(id,workspace_id,platform,role,config) VALUES($1,$2,'clickup','source',jsonb_build_object('clickup_team_id','team','webhook_id','w1','webhook_secret_env','SECRET','target_integration_id',($3::uuid)::text)),($3::uuid,$2,'telegram','control','{"chat_id":"-10042","bot_token_env":"TOKEN"}')`, integrationA, workspaceA, integrationB)
	var n atomic.Int64
	r, err := NewNoticeRepository(pool, func() (string, error) { return fmt.Sprintf("019a6690-abcd-7000-8000-%012d", 1000+n.Add(1)), nil }, DebounceSettings{})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, pool, r
}
func noticeEvent(key, task string) domain.NormalizedEvent {
	return domain.NormalizedEvent{WorkspaceID: workspaceA, IntegrationID: integrationA, Source: domain.SourceClickup, EventType: domain.EventCommandPosted, EventKey: "w1:" + key, ExternalMessageID: "comment:" + key, ExternalID: task, SenderID: "123", SenderName: "Author", Content: "original Việt", OccurredAt: time.Unix(1700000000, 0).UTC(), Metadata: map[string]string{"webhook_id": "w1"}}
}
func countRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNoticeAcceptAtomic(t *testing.T) {
	ctx, pool, r := newNoticeDB(t)
	e := noticeEvent("h1", "task-1")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := r.Accept(ctx, []domain.NormalizedEvent{e}); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if countRows(t, ctx, pool, "hub_threads") != 1 || countRows(t, ctx, pool, "message_logs") != 1 || countRows(t, ctx, pool, "notification_outbox") != 1 {
		t.Fatal("concurrent duplicate created extra records")
	}
	result, err := r.Accept(ctx, []domain.NormalizedEvent{e, noticeEvent("h2", "task-1"), noticeEvent("h3", "task-2")})
	if err != nil || result.Accepted != 2 || result.Duplicates != 1 {
		t.Fatalf("result %v %v", result, err)
	}
	var content, sender string
	var when time.Time
	if err := pool.QueryRow(ctx, `SELECT content,sender_name,occurred_at FROM message_logs ORDER BY id LIMIT 1`).Scan(&content, &sender, &when); err != nil || content != e.Content || sender != e.SenderName || !when.Equal(e.OccurredAt) {
		t.Fatalf("original lost %v", err)
	}
	other := missingID
	mustExec(t, ctx, pool, `INSERT INTO integrations(id,workspace_id,platform,role,config) SELECT $1,workspace_id,platform,role,config FROM integrations WHERE id=$2`, other, integrationA)
	e.IntegrationID = other
	if _, err := r.Accept(ctx, []domain.NormalizedEvent{e}); err != nil {
		t.Fatal(err)
	}
	if countRows(t, ctx, pool, "hub_threads") != 3 {
		t.Fatal("source namespaces merged")
	}
}

func TestNoticeAcceptRollbackAndRouting(t *testing.T) {
	for _, mode := range []string{"cross tenant", "source disabled", "target disabled", "db failure", "canceled", "mixed source"} {
		t.Run(mode, func(t *testing.T) {
			ctx, pool, r := newNoticeDB(t)
			events := []domain.NormalizedEvent{noticeEvent("h1", "task-1")}
			switch mode {
			case "cross tenant":
				mustExec(t, ctx, pool, `UPDATE integrations SET workspace_id=$1 WHERE id=$2`, workspaceB, integrationB)
			case "source disabled":
				mustExec(t, ctx, pool, `UPDATE integrations SET is_enabled=false WHERE id=$1`, integrationA)
			case "target disabled":
				mustExec(t, ctx, pool, `UPDATE integrations SET is_enabled=false WHERE id=$1`, integrationB)
			case "db failure":
				mustExec(t, ctx, pool, `ALTER TABLE notification_outbox ADD CONSTRAINT reject_job CHECK (event_key='never')`)
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "mixed source":
				e := noticeEvent("h2", "task-1")
				e.IntegrationID = missingID
				events = append(events, e)
			}
			if _, err := r.Accept(ctx, events); err == nil {
				t.Fatal("invalid acceptance succeeded")
			}
			check := context.Background()
			if countRows(t, check, pool, "message_logs") != 0 || countRows(t, check, pool, "notification_outbox") != 0 || countRows(t, check, pool, "hub_threads") != 0 {
				t.Fatal("rollback left records")
			}
		})
	}
}
