package storage

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	dbsqlc "github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const ownerUserID = "00000000-0000-0000-0000-000000000001"

func ownerMigrationParts(t *testing.T) (up, down, bootstrap string) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../../db/migrations/00012_people_reporting_owner.sql"))
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(data), "-- +goose Down")
	if !ok {
		t.Fatal("owner migration has no down section")
	}
	start := strings.Index(up, "INSERT INTO public.users")
	if start < 0 {
		t.Fatal("owner migration has no bootstrap")
	}
	return up, down, up[start:]
}

type migrationParts struct{ name, up, down string }

// laterThanOwnerMigrations returns the migrations after 00012 in version
// order. They depend on 00012's tables, so they must be reverted (in reverse
// order) before 00012's Down runs, and re-applied after its Up.
func laterThanOwnerMigrations(t *testing.T) []migrationParts {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	files, err := filepath.Glob(filepath.Join(filepath.Dir(file), "../../../db/migrations/*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	var later []migrationParts
	for _, path := range files {
		name := filepath.Base(path)
		if name <= "00012_people_reporting_owner.sql" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		up, down, ok := strings.Cut(string(data), "-- +goose Down")
		if !ok {
			t.Fatalf("migration %s has no down section", name)
		}
		later = append(later, migrationParts{name: name, up: up, down: down})
	}
	return later
}

// revertOwnerMigration reverts every later migration in reverse order and then
// 00012, leaving the schema at version 00011.
func revertOwnerMigration(t *testing.T, ctx context.Context, pool *pgxpool.Pool, down string, later []migrationParts) {
	t.Helper()
	for i := len(later) - 1; i >= 0; i-- {
		if _, err := pool.Exec(ctx, later[i].down); err != nil {
			t.Fatalf("revert %s: %v", later[i].name, err)
		}
	}
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
}

// reapplyOwnerMigration applies 00012 and then every later migration in order,
// leaving the schema at the latest version.
func reapplyOwnerMigration(t *testing.T, ctx context.Context, pool *pgxpool.Pool, up string, later []migrationParts) {
	t.Helper()
	if _, err := pool.Exec(ctx, up); err != nil {
		t.Fatal(err)
	}
	for _, m := range later {
		if _, err := pool.Exec(ctx, m.up); err != nil {
			t.Fatalf("reapply %s: %v", m.name, err)
		}
	}
}

func TestOwnerBootstrap(t *testing.T) {
	t.Run("TC-25 empty database remains valid", func(t *testing.T) {
		ctx, pool := testDB(t)
		q := dbsqlc.New(pool)
		var users, memberships, accounts int
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM workspace_members), (SELECT count(*) FROM contact_accounts)`).Scan(&users, &memberships, &accounts); err != nil {
			t.Fatal(err)
		}
		if users != 1 || memberships != 0 || accounts != 0 {
			t.Fatalf("empty bootstrap users=%d memberships=%d accounts=%d, want 1,0,0", users, memberships, accounts)
		}
		if _, err := q.GetOwnerUser(ctx); err != pgx.ErrNoRows {
			t.Fatalf("owner without persisted membership returned err=%v, want no rows", err)
		}
	})

	t.Run("TC-25 populated bootstrap is idempotent", func(t *testing.T) {
		ctx, pool := testDB(t)
		up, down, bootstrap := ownerMigrationParts(t)
		later := laterThanOwnerMigrations(t)
		revertOwnerMigration(t, ctx, pool, down, later)
		mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'A'),($2,'B')`, workspaceA, workspaceB)
		mustExec(t, ctx, pool, `INSERT INTO app_settings(key,value) VALUES ('owner_telegram_user_id','"4242"'),('owner_clickup_user_id','"77"')`)
		mustExec(t, ctx, pool, `INSERT INTO integrations(id,workspace_id,platform,config) VALUES($1,$2,'github','{"agent":{"tools":{"github":{"accounts":{"main":{"login":"owner-gh"}}}}}}')`, integrationA, workspaceA)
		reapplyOwnerMigration(t, ctx, pool, up, later)
		if _, err := pool.Exec(ctx, bootstrap); err != nil {
			t.Fatalf("bootstrap rerun failed: %v", err)
		}
		var users, memberships, contacts, accounts, distinctAccounts int
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM users WHERE id=$1), (SELECT count(*) FROM workspace_members WHERE user_id=$1 AND role='owner'), (SELECT count(*) FROM contacts), (SELECT count(*) FROM contact_accounts WHERE is_self), (SELECT count(DISTINCT (workspace_id,platform,external_id)) FROM contact_accounts WHERE is_self)`, ownerUserID).Scan(&users, &memberships, &contacts, &accounts, &distinctAccounts); err != nil {
			t.Fatal(err)
		}
		if users != 1 || memberships != 2 || contacts != 2 || accounts != 5 || distinctAccounts != 5 {
			t.Fatalf("idempotent graph users=%d memberships=%d contacts=%d accounts=%d distinct=%d, want 1,2,2,5,5", users, memberships, contacts, accounts, distinctAccounts)
		}
	})

	t.Run("TC-26 owner identity requires persisted membership and self account", func(t *testing.T) {
		ctx, pool := testDB(t)
		up, down, _ := ownerMigrationParts(t)
		later := laterThanOwnerMigrations(t)
		revertOwnerMigration(t, ctx, pool, down, later)
		mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'A')`, workspaceA)
		mustExec(t, ctx, pool, `INSERT INTO app_settings(key,value) VALUES ('owner_telegram_user_id','"4242"')`)
		reapplyOwnerMigration(t, ctx, pool, up, later)
		q := dbsqlc.New(pool)
		owner, err := q.GetOwnerUser(ctx)
		if err != nil || owner.ID != testUUID(t, ownerUserID) {
			t.Fatalf("persisted owner membership not resolved: owner=%v err=%v", owner.ID, err)
		}
		var self bool
		if err := pool.QueryRow(ctx, `SELECT is_self FROM contact_accounts WHERE workspace_id=$1 AND platform='telegram' AND external_id='4242'`, workspaceA).Scan(&self); err != nil || !self {
			t.Fatalf("persisted owner account is_self=%v err=%v", self, err)
		}
		mustExec(t, ctx, pool, `INSERT INTO users(id,display_name) VALUES('019a6690-abcd-7000-8000-000000000299','Impostor')`)
		mustExec(t, ctx, pool, `INSERT INTO workspace_members(user_id,workspace_id,role) VALUES('019a6690-abcd-7000-8000-000000000299',$1,'member')`, workspaceA)
		mustExec(t, ctx, pool, `INSERT INTO contacts(id,workspace_id) VALUES('019a6690-abcd-7000-8000-000000000298',$1)`, workspaceA)
		mustExec(t, ctx, pool, `INSERT INTO contact_accounts(id,workspace_id,contact_id,platform,external_id,source,is_self) VALUES('019a6690-abcd-7000-8000-000000000297',$1,'019a6690-abcd-7000-8000-000000000298','telegram','04242','manual',false)`, workspaceA)
		var impostorOwners, impostorSelf int
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM workspace_members WHERE user_id='019a6690-abcd-7000-8000-000000000299' AND role='owner'), (SELECT count(*) FROM contact_accounts WHERE external_id='04242' AND is_self)`).Scan(&impostorOwners, &impostorSelf); err != nil {
			t.Fatal(err)
		}
		if impostorOwners != 0 || impostorSelf != 0 {
			t.Fatalf("resembling identity gained owner state: memberships=%d self_accounts=%d", impostorOwners, impostorSelf)
		}
		mustExec(t, ctx, pool, `DELETE FROM workspace_members WHERE user_id=$1`, ownerUserID)
		if _, err := q.GetOwnerUser(ctx); err != pgx.ErrNoRows {
			t.Fatalf("self account without owner membership returned err=%v, want no rows", err)
		}
	})
}
