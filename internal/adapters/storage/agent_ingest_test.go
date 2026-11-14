package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

const ownSender = "999"

// newAgentDB seeds a ClickUp source with the agent switched on or off.
func newAgentDB(t *testing.T, agentEnabled bool) (context.Context, *pgxpool.Pool, *NoticeRepository) {
	t.Helper()
	ctx, pool := testDB(t)
	mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'A')`, workspaceA)
	mustExec(t, ctx, pool, `INSERT INTO integrations(id,workspace_id,platform,role,config) VALUES
	 ($1,$2,'clickup','source',jsonb_build_object('clickup_team_id','team','webhook_id','w1','webhook_secret_env','SECRET','own_sender_id',$4::text,
	   'target_integration_id',($3::uuid)::text,'agent',jsonb_build_object('enabled',$5::boolean))),
	 ($3::uuid,$2,'telegram','control','{"chat_id":"-10042","bot_token_env":"TOKEN"}')`, integrationA, workspaceA, integrationB, ownSender, agentEnabled)
	var n atomic.Int64
	r, err := NewNoticeRepository(pool, func() (string, error) { return fmt.Sprintf("019a6690-abcd-7000-8000-%012d", 2000+n.Add(1)), nil }, DebounceSettings{Window: 8 * time.Second, MaxWait: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	r.SetKnownTools(map[string]tenant.Tier{"code_search": tenant.TierRead})
	return ctx, pool, r
}

func scalar[T any](t *testing.T, ctx context.Context, pool *pgxpool.Pool, q string, args ...any) T {
	t.Helper()
	var v T
	if err := pool.QueryRow(ctx, q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func TestAgentIngest_CommentEnqueuesJobNotNotice(t *testing.T) {
	ctx, pool, r := newAgentDB(t, true)
	res, err := r.Accept(ctx, []domain.NormalizedEvent{noticeEvent("c1", "task-1")})
	if err != nil || res.Accepted != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if n := countRows(t, ctx, pool, "notification_outbox"); n != 0 {
		t.Fatalf("notices %d", n)
	}
	if role := scalar[string](t, ctx, pool, `SELECT sender_role FROM message_logs`); role != "external_user" {
		t.Fatalf("role %s", role)
	}
	if n := scalar[int](t, ctx, pool, `SELECT count(*) FROM agent_jobs WHERE status='pending' AND run_at > now() AND run_at <= now()+interval '9 seconds'`); n != 1 {
		t.Fatalf("pending jobs %d", n)
	}
}

func TestAgentIngest_BurstCoalesces(t *testing.T) {
	ctx, pool, r := newAgentDB(t, true)
	for i := 0; i < 3; i++ {
		if _, err := r.Accept(ctx, []domain.NormalizedEvent{noticeEvent(fmt.Sprintf("b%d", i), "task-1")}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Accept(ctx, []domain.NormalizedEvent{noticeEvent("other", "task-2")}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, ctx, pool, "agent_jobs"); n != 2 {
		t.Fatalf("jobs %d", n)
	}
	if n := countRows(t, ctx, pool, "message_logs"); n != 4 {
		t.Fatalf("messages %d", n)
	}
	// run_at never passes first_enqueued_at + max wait.
	mustExec(t, ctx, pool, `UPDATE agent_jobs SET first_enqueued_at=now()-interval '25 seconds'`)
	if _, err := r.Accept(ctx, []domain.NormalizedEvent{noticeEvent("b9", "task-1")}); err != nil {
		t.Fatal(err)
	}
	ok := scalar[bool](t, ctx, pool, `SELECT run_at <= first_enqueued_at+interval '30 seconds' + interval '50 milliseconds' AND run_at < now()+interval '6 seconds' FROM agent_jobs j JOIN hub_threads h ON h.id=j.thread_id WHERE h.external_id='task-1'`)
	if !ok {
		t.Fatal("max wait not applied")
	}
}

func TestAgentIngest_StatusStillNotice(t *testing.T) {
	ctx, pool, r := newAgentDB(t, true)
	e := noticeEvent("s1", "task-1")
	e.EventType = domain.EventStatusChanged
	if _, err := r.Accept(ctx, []domain.NormalizedEvent{e}); err != nil {
		t.Fatal(err)
	}
	if countRows(t, ctx, pool, "notification_outbox") != 1 || countRows(t, ctx, pool, "agent_jobs") != 0 {
		t.Fatal("status change not routed to notice")
	}
}

func TestAgentIngest_OwnerCommentContextOnly(t *testing.T) {
	ctx, pool, r := newAgentDB(t, true)
	e := noticeEvent("o1", "task-1")
	e.SenderID = ownSender
	if _, err := r.Accept(ctx, []domain.NormalizedEvent{e}); err != nil {
		t.Fatal(err)
	}
	if countRows(t, ctx, pool, "notification_outbox") != 0 || countRows(t, ctx, pool, "agent_jobs") != 0 {
		t.Fatal("owner comment produced work")
	}
	if role := scalar[string](t, ctx, pool, `SELECT sender_role FROM message_logs`); role != "human_owner" {
		t.Fatalf("role %s", role)
	}
}

func TestAgentIngest_DisabledSourceUnchanged(t *testing.T) {
	ctx, pool, r := newAgentDB(t, false)
	if _, err := r.Accept(ctx, []domain.NormalizedEvent{noticeEvent("d1", "task-1")}); err != nil {
		t.Fatal(err)
	}
	if countRows(t, ctx, pool, "notification_outbox") != 1 || countRows(t, ctx, pool, "agent_jobs") != 0 {
		t.Fatal("disabled agent changed Phase 1 behavior")
	}
}

func TestAgentIngest_DuplicateCallback(t *testing.T) {
	ctx, pool, r := newAgentDB(t, true)
	e := noticeEvent("dup", "task-1")
	if _, err := r.Accept(ctx, []domain.NormalizedEvent{e}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, pool, `UPDATE agent_jobs SET status='done'`)
	res, err := r.Accept(ctx, []domain.NormalizedEvent{e})
	if err != nil || res.Duplicates != 1 || res.Accepted != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if countRows(t, ctx, pool, "message_logs") != 1 || countRows(t, ctx, pool, "agent_jobs") != 1 {
		t.Fatal("duplicate created work")
	}
}

func TestMigration00003_Constraints(t *testing.T) {
	ctx, pool := testDB(t)
	mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'A')`, workspaceA)
	insert := `INSERT INTO credentials(id,workspace_id,name,kind,ciphertext,nonce) VALUES(gen_random_uuid(),$1,$2,$3,'\x00',$4)`
	if _, err := pool.Exec(ctx, insert, workspaceA, "Bad Name", "github_pat", make([]byte, 12)); err == nil {
		t.Fatal("bad name accepted")
	}
	if _, err := pool.Exec(ctx, insert, workspaceA, "gh", "github_pat", make([]byte, 11)); err == nil {
		t.Fatal("short nonce accepted")
	}
	if _, err := pool.Exec(ctx, insert, workspaceA, "gh", "ssh_key", make([]byte, 12)); err == nil {
		t.Fatal("unknown kind accepted")
	}
	mustExec(t, ctx, pool, insert, workspaceA, "gh", "github_pat", make([]byte, 12))
	if _, err := pool.Exec(ctx, insert, workspaceA, "gh", "github_pat", make([]byte, 12)); err == nil {
		t.Fatal("duplicate name accepted")
	}
	for _, table := range []string{"agent_jobs", "outbound_actions", "tool_runs", "credentials", "app_settings"} {
		if !scalar[bool](t, ctx, pool, `SELECT relrowsecurity FROM pg_class WHERE oid=('public.'||$1)::regclass`, table) {
			t.Fatalf("RLS off on %s", table)
		}
	}
	// goose runs Downs newest first: every later migration's Down runs
	// before 00003's (later tables reference agent_jobs), then 00003 Down,
	// then every Up again in order. The 00003 round trip is still proven.
	_, file, _, _ := runtime.Caller(0)
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(file), "../../../db/migrations/*.sql"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("migrations: %v", err)
	}
	sort.Strings(paths)
	type migration struct{ name, up, down string }
	var later []migration
	var core migration
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		up, down, _ := strings.Cut(string(data), "-- +goose Down")
		m := migration{name: filepath.Base(path), up: up, down: down}
		switch {
		case m.name < "00003":
		case strings.HasPrefix(m.name, "00003_"):
			core = m
		default:
			later = append(later, m)
		}
	}
	if core.name != "00003_create_agent_core.sql" {
		t.Fatalf("00003 not found: %q", core.name)
	}
	for i := len(later) - 1; i >= 0; i-- {
		if _, err := pool.Exec(ctx, later[i].down); err != nil {
			t.Fatalf("down %s: %v", later[i].name, err)
		}
	}
	mustExec(t, ctx, pool, core.down)
	if scalar[bool](t, ctx, pool, `SELECT to_regclass('public.agent_jobs') IS NOT NULL`) {
		t.Fatal("Down left agent_jobs")
	}
	mustExec(t, ctx, pool, core.up)
	if !scalar[bool](t, ctx, pool, `SELECT to_regclass('public.agent_jobs') IS NOT NULL`) {
		t.Fatal("Up did not recreate agent_jobs")
	}
	for _, m := range later {
		if _, err := pool.Exec(ctx, m.up); err != nil {
			t.Fatalf("up %s: %v", m.name, err)
		}
	}
}
