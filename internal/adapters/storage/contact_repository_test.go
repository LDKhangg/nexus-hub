package storage

import (
	"testing"
	"time"

	dbsqlc "github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/jackc/pgx/v5/pgtype"
)

func testUUID(t *testing.T, value string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := id.Scan(value); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestContactQueries_WorkspaceScopeUniquenessAndAliases(t *testing.T) {
	ctx, pool := testDB(t)
	mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'A'),($2,'B')`, workspaceA, workspaceB)
	q := dbsqlc.New(pool)
	seen := pgtype.Timestamptz{Time: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC), Valid: true}

	t.Run("TC-1 workspace scope uniqueness and no email column", func(t *testing.T) {
		first, err := q.UpsertContactAccount(ctx, dbsqlc.UpsertContactAccountParams{
			AccountID: testUUID(t, "019a6690-abcd-7000-8000-000000000101"), WorkspaceID: testUUID(t, workspaceA),
			Platform: "github", ExternalID: "person", PlatformName: pgtype.Text{String: "First", Valid: true},
			Source: "auto", SeenAt: seen, ContactID: testUUID(t, "019a6690-abcd-7000-8000-000000000111"),
		})
		if err != nil {
			t.Fatal(err)
		}
		again, err := q.UpsertContactAccount(ctx, dbsqlc.UpsertContactAccountParams{
			AccountID: testUUID(t, "019a6690-abcd-7000-8000-000000000102"), WorkspaceID: testUUID(t, workspaceA),
			Platform: "github", ExternalID: "person", PlatformName: pgtype.Text{String: "Updated", Valid: true},
			Source: "auto", SeenAt: seen, ContactID: testUUID(t, "019a6690-abcd-7000-8000-000000000112"),
		})
		if err != nil {
			t.Fatal(err)
		}
		other, err := q.UpsertContactAccount(ctx, dbsqlc.UpsertContactAccountParams{
			AccountID: testUUID(t, "019a6690-abcd-7000-8000-000000000103"), WorkspaceID: testUUID(t, workspaceB),
			Platform: "github", ExternalID: "person", PlatformName: pgtype.Text{String: "Other", Valid: true},
			Source: "auto", SeenAt: seen, ContactID: testUUID(t, "019a6690-abcd-7000-8000-000000000113"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if first.ID != again.ID || first.ContactID != again.ContactID {
			t.Fatalf("same workspace account duplicated: first=%v/%v again=%v/%v", first.ID, first.ContactID, again.ID, again.ContactID)
		}
		if first.ID == other.ID || first.ContactID == other.ContactID {
			t.Fatal("account identity leaked across workspaces")
		}
		var accounts, contacts, emailColumns int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM contact_accounts WHERE platform='github' AND external_id='person'`).Scan(&accounts); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM contacts`).Scan(&contacts); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name IN ('contacts','contact_accounts') AND column_name ILIKE '%email%'`).Scan(&emailColumns); err != nil {
			t.Fatal(err)
		}
		if accounts != 2 || contacts != 2 || emailColumns != 0 {
			t.Fatalf("accounts=%d contacts=%d email columns=%d, want 2, 2, 0", accounts, contacts, emailColumns)
		}
	})

	t.Run("TC-2 aliases remain per workspace", func(t *testing.T) {
		rowA, err := q.GetContactByAccount(ctx, dbsqlc.GetContactByAccountParams{WorkspaceID: testUUID(t, workspaceA), Platform: "github", ExternalID: "person"})
		if err != nil {
			t.Fatal(err)
		}
		rowB, err := q.GetContactByAccount(ctx, dbsqlc.GetContactByAccountParams{WorkspaceID: testUUID(t, workspaceB), Platform: "github", ExternalID: "person"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.SetContactAlias(ctx, dbsqlc.SetContactAliasParams{DisplayName: pgtype.Text{String: "Lan", Valid: true}, WorkspaceID: testUUID(t, workspaceA), ContactID: rowA.ID}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.SetContactAlias(ctx, dbsqlc.SetContactAliasParams{DisplayName: pgtype.Text{String: "Mai", Valid: true}, WorkspaceID: testUUID(t, workspaceB), ContactID: rowB.ID}); err != nil {
			t.Fatal(err)
		}
		gotA, _ := q.GetContactByAccount(ctx, dbsqlc.GetContactByAccountParams{WorkspaceID: testUUID(t, workspaceA), Platform: "github", ExternalID: "person"})
		gotB, _ := q.GetContactByAccount(ctx, dbsqlc.GetContactByAccountParams{WorkspaceID: testUUID(t, workspaceB), Platform: "github", ExternalID: "person"})
		if gotA.DisplayName.String != "Lan" || gotB.DisplayName.String != "Mai" {
			t.Fatalf("aliases leaked or were lost: A=%q B=%q", gotA.DisplayName.String, gotB.DisplayName.String)
		}
	})
}

func TestContactQueries_MergeSplitForgetLifecycle(t *testing.T) {
	ctx, pool := testDB(t)
	mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'A')`, workspaceA)
	q := dbsqlc.New(pool)
	seen := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	ids := []struct{ account, contact, platform, external string }{
		{"019a6690-abcd-7000-8000-000000000121", "019a6690-abcd-7000-8000-000000000131", "github", "one"},
		{"019a6690-abcd-7000-8000-000000000122", "019a6690-abcd-7000-8000-000000000132", "telegram", "two"},
	}
	for _, item := range ids {
		if _, err := q.UpsertContactAccount(ctx, dbsqlc.UpsertContactAccountParams{AccountID: testUUID(t, item.account), WorkspaceID: testUUID(t, workspaceA), Platform: item.platform, ExternalID: item.external, Source: "manual", SeenAt: seen, ContactID: testUUID(t, item.contact)}); err != nil {
			t.Fatal(err)
		}
	}
	target, err := q.MergeContacts(ctx, dbsqlc.MergeContactsParams{WorkspaceID: testUUID(t, workspaceA), SourceID: testUUID(t, ids[0].contact), TargetID: testUUID(t, ids[1].contact)})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM contact_accounts WHERE contact_id=$1`, target.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("merge account count=%d err=%v, want 2", count, err)
	}
	newContact := testUUID(t, "019a6690-abcd-7000-8000-000000000133")
	if _, err := q.SplitContactAccount(ctx, dbsqlc.SplitContactAccountParams{WorkspaceID: testUUID(t, workspaceA), AccountID: testUUID(t, ids[0].account), NewContactID: newContact}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.ForgetContact(ctx, dbsqlc.ForgetContactParams{WorkspaceID: testUUID(t, workspaceA), ContactID: target.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.ForgetContact(ctx, dbsqlc.ForgetContactParams{WorkspaceID: testUUID(t, workspaceA), ContactID: newContact}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM contact_accounts`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("forget left %d account rows (err=%v)", count, err)
	}
}
