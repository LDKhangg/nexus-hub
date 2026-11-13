package storage

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	dbsqlc "github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/jackc/pgx/v5"
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
		if _, err := pool.Exec(ctx, down); err != nil {
			t.Fatal(err)
		}
		mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'A'),($2,'B')`, workspaceA, workspaceB)
		mustExec(t, ctx, pool, `INSERT INTO app_settings(key,value) VALUES ('owner_telegram_user_id','"4242"'),('owner_clickup_user_id','"77"')`)
		mustExec(t, ctx, pool, `INSERT INTO integrations(id,workspace_id,platform,config) VALUES($1,$2,'github','{"agent":{"tools":{"github":{"accounts":{"main":{"login":"owner-gh"}}}}}}')`, integrationA, workspaceA)
		if _, err := pool.Exec(ctx, up); err != nil {
			t.Fatal(err)
		}
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
		if _, err := pool.Exec(ctx, down); err != nil {
			t.Fatal(err)
		}
		mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'A')`, workspaceA)
		mustExec(t, ctx, pool, `INSERT INTO app_settings(key,value) VALUES ('owner_telegram_user_id','"4242"')`)
		if _, err := pool.Exec(ctx, up); err != nil {
			t.Fatal(err)
		}
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
