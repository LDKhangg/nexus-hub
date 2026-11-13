package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/people"
	"github.com/LDKhangg/nexus-hub/internal/reporting"
	"github.com/jackc/pgx/v5/pgxpool"
)

func phase4Directory(t *testing.T) (context.Context, *pgxpool.Pool, *PeopleRepository) {
	t.Helper()
	ctx, pool := testDB(t)
	mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'A'),($2,'B')`, workspaceA, workspaceB)
	repo, err := NewPeopleRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, pool, repo
}

func sight(t *testing.T, ctx context.Context, repo *PeopleRepository, ws, platform, id, name, username string, at time.Time) {
	t.Helper()
	if err := repo.Sighting(ctx, people.Sighting{WorkspaceID: ws, Platform: platform, ExternalID: id, PlatformName: name, Username: username, Source: "auto", SeenAt: at}); err != nil {
		t.Fatalf("sighting %s:%s: %v", platform, id, err)
	}
}

func countSQL(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func displayName(t *testing.T, ctx context.Context, repo *PeopleRepository, ws, platform, id string) string {
	t.Helper()
	name, ok := repo.DisplayName(ctx, ws, platform, id)
	if !ok {
		return ""
	}
	return name
}

func TestPhase4PeopleDirectory(t *testing.T) {
	base := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

	// TC-3 (REQ-2): sightings from the three platforms create one contact
	// each, and a later sighting updates the name without a new contact.
	t.Run("TC-3 sightings create contacts once and update names", func(t *testing.T) {
		ctx, pool, repo := phase4Directory(t)
		sight(t, ctx, repo, workspaceA, "clickup", "106", "Định Nguyễn", "dinh", base)
		sight(t, ctx, repo, workspaceA, "github", "dinh-gh", "", "dinh-gh", base)
		sight(t, ctx, repo, workspaceA, "telegram", "5001", "Lan", "lan_tg", base)
		if got := displayName(t, ctx, repo, workspaceA, "clickup", "106"); got != "Định Nguyễn" {
			t.Fatalf("first sighting name=%q", got)
		}
		sight(t, ctx, repo, workspaceA, "clickup", "106", "Định N.", "dinh", base.Add(time.Minute))
		if got := displayName(t, ctx, repo, workspaceA, "clickup", "106"); got != "Định N." {
			t.Fatalf("re-sighting did not update the cached name: %q", got)
		}
		if c, a := countSQL(t, ctx, pool, `SELECT count(*) FROM contacts WHERE workspace_id=$1`, workspaceA), countSQL(t, ctx, pool, `SELECT count(*) FROM contact_accounts WHERE workspace_id=$1`, workspaceA); c != 3 || a != 3 {
			t.Fatalf("contacts=%d accounts=%d, want 3 and 3", c, a)
		}
		var lastSeen time.Time
		var source string
		if err := pool.QueryRow(ctx, `SELECT last_seen, source FROM contact_accounts WHERE workspace_id=$1 AND platform='clickup' AND external_id='106'`, workspaceA).Scan(&lastSeen, &source); err != nil {
			t.Fatal(err)
		}
		if !lastSeen.Equal(base.Add(time.Minute)) || source != "auto" {
			t.Fatalf("last_seen=%v source=%q", lastSeen, source)
		}
		var alias *string
		if err := pool.QueryRow(ctx, `SELECT c.display_name FROM contacts c JOIN contact_accounts a ON a.contact_id=c.id WHERE a.external_id='106'`).Scan(&alias); err != nil || alias != nil {
			t.Fatalf("a new account must create a contact with no alias: alias=%v err=%v", alias, err)
		}
		if err := repo.Sighting(ctx, people.Sighting{WorkspaceID: workspaceA, Platform: "clickup", ExternalID: "", Source: "auto"}); err == nil {
			t.Fatal("sighting without an external id accepted")
		}
		if err := repo.Sighting(ctx, people.Sighting{WorkspaceID: workspaceA, Platform: "slack", ExternalID: "1", Source: "auto"}); err == nil {
			t.Fatal("sighting for an unsupported platform accepted")
		}
	})

	// TC-4 (REQ-2): duplicate and out-of-order sightings neither duplicate
	// rows nor overwrite a newer name or the owner's alias.
	t.Run("TC-4 duplicate and out-of-order sightings", func(t *testing.T) {
		ctx, pool, repo := phase4Directory(t)
		for i := 0; i < 3; i++ {
			sight(t, ctx, repo, workspaceA, "clickup", "106", "Định New", "dinh", base.Add(time.Hour))
		}
		sight(t, ctx, repo, workspaceA, "clickup", "106", "Định Old", "dinh_old", base)
		if got := displayName(t, ctx, repo, workspaceA, "clickup", "106"); got != "Định New" {
			t.Fatalf("older sighting overwrote the newer name: %q", got)
		}
		var lastSeen time.Time
		var username string
		if err := pool.QueryRow(ctx, `SELECT last_seen, username FROM contact_accounts WHERE external_id='106'`).Scan(&lastSeen, &username); err != nil {
			t.Fatal(err)
		}
		if !lastSeen.Equal(base.Add(time.Hour)) || username != "dinh" {
			t.Fatalf("out-of-order sighting moved last_seen=%v username=%q", lastSeen, username)
		}
		sight(t, ctx, repo, workspaceA, "clickup", "106", "", "", base.Add(2*time.Hour))
		if got := displayName(t, ctx, repo, workspaceA, "clickup", "106"); got != "Định New" {
			t.Fatalf("a sighting without a name erased the stored name: %q", got)
		}
		if _, err := repo.SetAlias(ctx, workspaceA, "clickup:106", "anh Định"); err != nil {
			t.Fatal(err)
		}
		sight(t, ctx, repo, workspaceA, "clickup", "106", "Renamed On ClickUp", "dinh", base.Add(3*time.Hour))
		if got := displayName(t, ctx, repo, workspaceA, "clickup", "106"); got != "anh Định" {
			t.Fatalf("sighting overwrote the owner's alias: %q", got)
		}
		if c, a := countSQL(t, ctx, pool, `SELECT count(*) FROM contacts`), countSQL(t, ctx, pool, `SELECT count(*) FROM contact_accounts`); c != 1 || a != 1 {
			t.Fatalf("duplicates: contacts=%d accounts=%d", c, a)
		}
	})

	// TC-4 (REQ-2): two first sightings of one new account that arrive at
	// the same time (two webhooks) still make one contact. The workspace row
	// is held locked so both statements start before either commits.
	t.Run("TC-4 concurrent first sightings make one contact", func(t *testing.T) {
		ctx, pool, repo := phase4Directory(t)
		holder, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Rollback(ctx)
		if _, err := holder.Exec(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceA); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- repo.Sighting(ctx, people.Sighting{WorkspaceID: workspaceA, Platform: "clickup", ExternalID: "777", PlatformName: "Burst", Source: "auto", SeenAt: base})
			}()
		}
		deadline := time.Now().Add(10 * time.Second)
		for countSQL(t, ctx, pool, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%locked_workspace%'`) < 2 {
			if time.Now().After(deadline) {
				t.Fatal("sightings did not reach the workspace lock")
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err := holder.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent sighting failed: %v", err)
			}
		}
		contacts := countSQL(t, ctx, pool, `SELECT count(*) FROM contacts WHERE workspace_id=$1`, workspaceA)
		orphans := countSQL(t, ctx, pool, `SELECT count(*) FROM contacts c WHERE NOT EXISTS (SELECT 1 FROM contact_accounts a WHERE a.contact_id=c.id)`)
		if contacts != 1 || orphans != 0 {
			t.Fatalf("concurrent first sightings: contacts=%d orphan contacts=%d, want 1 and 0", contacts, orphans)
		}
	})

	// TC-5 (REQ-3): alias, then platform name, then username, then
	// "<platform> <id>".
	t.Run("TC-5 resolution priority", func(t *testing.T) {
		ctx, _, repo := phase4Directory(t)
		sight(t, ctx, repo, workspaceA, "clickup", "1", "Platform Name", "user1", base)
		sight(t, ctx, repo, workspaceA, "clickup", "2", "", "user2", base)
		sight(t, ctx, repo, workspaceA, "clickup", "3", "", "", base)
		sight(t, ctx, repo, workspaceA, "clickup", "4", "\u202e\n", "@x", base)
		want := map[string]string{"1": "Platform Name", "2": "user2", "3": "clickup 3", "4": "＠x"}
		for id, name := range want {
			if got := displayName(t, ctx, repo, workspaceA, "clickup", id); got != name {
				t.Fatalf("id %s resolved to %q, want %q", id, got, name)
			}
		}
		if _, err := repo.SetAlias(ctx, workspaceA, "clickup:1", "  anh\nĐịnh @all "); err != nil {
			t.Fatal(err)
		}
		if got := displayName(t, ctx, repo, workspaceA, "clickup", "1"); got != "anh Định ＠all" {
			t.Fatalf("alias not cleaned or not first: %q", got)
		}
		if has, err := repo.HasAlias(ctx, workspaceA, "clickup", "1"); err != nil || !has {
			t.Fatalf("HasAlias=%v err=%v", has, err)
		}
		if has, err := repo.HasAlias(ctx, workspaceA, "clickup", "2"); err != nil || has {
			t.Fatalf("unnamed account HasAlias=%v err=%v", has, err)
		}
		if _, ok := repo.DisplayName(ctx, workspaceA, "clickup", "unknown"); ok {
			t.Fatal("unknown account resolved")
		}
		if _, err := repo.SetAlias(ctx, workspaceA, "clickup:2", " \n\u202e "); err == nil {
			t.Fatal("empty alias after cleaning accepted")
		}
		out, err := reporting.NewRenderer(nil, repo).Render(ctx, reporting.Report{Kind: reporting.KindPRApprove, WorkspaceID: workspaceA, Language: "vi", Platform: "clickup", ExternalID: "1", Values: reporting.Values{PRNumber: "1601"}})
		if err != nil || out != "✅ Approved PR #1601 cho anh Định ＠all" {
			t.Fatalf("DM report through the directory=%q err=%v", out, err)
		}
	})

	// TC-7 (REQ-4) and TC-10 (REQ-5): merge, split and forget stay inside
	// one workspace; a forgotten or foreign account falls back to its id.
	t.Run("TC-7 TC-10 lifecycle and workspace isolation", func(t *testing.T) {
		ctx, pool, repo := phase4Directory(t)
		sight(t, ctx, repo, workspaceA, "clickup", "106", "Định CU", "", base)
		sight(t, ctx, repo, workspaceA, "github", "dinh", "", "dinh", base)
		sight(t, ctx, repo, workspaceB, "clickup", "106", "Định B", "", base)
		listA, err := repo.List(ctx, workspaceA, "", "", 10)
		if err != nil || len(listA) != 2 {
			t.Fatalf("list A=%+v err=%v", listA, err)
		}
		var cu, gh people.Contact
		for _, c := range listA {
			switch c.Accounts[0].Platform {
			case "clickup":
				cu = c
			case "github":
				gh = c
			}
		}
		if filtered, err := repo.List(ctx, workspaceA, "github", "", 10); err != nil || len(filtered) != 1 || filtered[0].ID != gh.ID {
			t.Fatalf("platform filter=%+v err=%v", filtered, err)
		}
		if found, err := repo.List(ctx, workspaceA, "", "cu", 10); err != nil || len(found) != 1 || found[0].ID != cu.ID {
			t.Fatalf("search=%+v err=%v", found, err)
		}
		listB, _ := repo.List(ctx, workspaceB, "", "", 10)
		if len(listB) != 1 {
			t.Fatalf("list B=%+v", listB)
		}
		// Foreign-workspace references are refused and change nothing.
		if _, err := repo.SetAlias(ctx, workspaceB, cu.ID, "hijack"); err == nil {
			t.Fatal("alias through another workspace's contact id accepted")
		}
		if _, err := repo.Merge(ctx, workspaceA, cu.ID, listB[0].ID); err == nil {
			t.Fatal("merge across workspaces accepted")
		}
		if _, err := repo.Show(ctx, workspaceB, cu.ID); err == nil {
			t.Fatal("show through another workspace accepted")
		}
		if err := repo.Forget(ctx, workspaceB, cu.ID); err == nil {
			t.Fatal("forget through another workspace accepted")
		}
		if got := displayName(t, ctx, repo, workspaceA, "clickup", "106"); got != "Định CU" {
			t.Fatalf("workspace A changed by foreign operations: %q", got)
		}
		if got := displayName(t, ctx, repo, workspaceA, "telegram", "106"); got != "" {
			t.Fatalf("platform confusion resolved %q", got)
		}
		// Merge github into clickup; the alias then names both accounts.
		merged, err := repo.Merge(ctx, workspaceA, gh.ID, cu.ID)
		if err != nil || merged.ID != cu.ID || len(merged.Accounts) != 2 {
			t.Fatalf("merge=%+v err=%v", merged, err)
		}
		if _, err := repo.SetAlias(ctx, workspaceA, cu.ID, "anh Định"); err != nil {
			t.Fatal(err)
		}
		if got := displayName(t, ctx, repo, workspaceA, "github", "dinh"); got != "anh Định" {
			t.Fatalf("merged account name=%q", got)
		}
		if got := displayName(t, ctx, repo, workspaceB, "clickup", "106"); got != "Định B" {
			t.Fatalf("alias leaked to workspace B: %q", got)
		}
		// Split github out again: it loses the alias.
		var ghAccount string
		for _, a := range merged.Accounts {
			if a.Platform == "github" {
				ghAccount = a.ID
			}
		}
		split, err := repo.Split(ctx, workspaceA, ghAccount)
		if err != nil || split.ID == cu.ID || len(split.Accounts) != 1 || split.Alias != "" {
			t.Fatalf("split=%+v err=%v", split, err)
		}
		if got := displayName(t, ctx, repo, workspaceA, "github", "dinh"); got != "dinh" {
			t.Fatalf("split account name=%q", got)
		}
		// Forget removes the contact and every account copy in A only.
		if err := repo.Forget(ctx, workspaceA, cu.ID); err != nil {
			t.Fatal(err)
		}
		if n := countSQL(t, ctx, pool, `SELECT count(*) FROM contact_accounts WHERE workspace_id=$1 AND platform='clickup' AND external_id='106'`, workspaceA); n != 0 {
			t.Fatalf("forget left %d account rows", n)
		}
		if n := countSQL(t, ctx, pool, `SELECT count(*) FROM contacts WHERE id=$1`, cu.ID); n != 0 {
			t.Fatal("forget left the contact row")
		}
		if got := displayName(t, ctx, repo, workspaceA, "clickup", "106"); got != "" {
			t.Fatalf("forgotten contact still resolves: %q", got)
		}
		out, err := reporting.NewRenderer(nil, repo).Render(ctx, reporting.Report{Kind: reporting.KindNotify, WorkspaceID: workspaceA, Language: "en", Platform: "clickup", ExternalID: "106"})
		if err != nil || out != "ℹ️ For your information from clickup 106" {
			t.Fatalf("forgotten contact fallback=%q err=%v", out, err)
		}
		if got := displayName(t, ctx, repo, workspaceB, "clickup", "106"); got != "Định B" {
			t.Fatalf("forget in A removed B's contact: %q", got)
		}
		// A later sighting adds the person again, without the old alias.
		sight(t, ctx, repo, workspaceA, "clickup", "106", "Định Again", "", base.Add(time.Hour))
		if got := displayName(t, ctx, repo, workspaceA, "clickup", "106"); got != "Định Again" {
			t.Fatalf("re-added contact=%q", got)
		}
	})

	// TC-R3 support (REQ-5): the behavior store resolves through the same
	// directory, so traces built from it use the current name.
	t.Run("TC-R3 behavior store resolves trace sender names", func(t *testing.T) {
		ctx, pool, _ := phase4Directory(t)
		store, err := NewBehaviorStore(pool)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Sighting(ctx, people.Sighting{WorkspaceID: workspaceA, Platform: "clickup", ExternalID: "106", PlatformName: "Định", Source: "auto", SeenAt: base}); err != nil {
			t.Fatal(err)
		}
		trigger, _ := json.Marshal(behavior.TriggerData{Type: behavior.TriggerChatMessage, SenderID: "106", MessageID: "m1", ChannelType: "dm"})
		tr := behavior.Trace{Run: behavior.TraceRun{ID: 9, TriggerKind: behavior.TriggerChatMessage, Trigger: trigger}}
		behavior.ResolveTraceSender(ctx, store, workspaceA, &tr)
		if tr.Run.SenderName != "Định" || !strings.Contains(behavior.RenderTraceText(tr, time.UTC, false), "người gửi Định (106)") {
			t.Fatalf("trace sender=%q", tr.Run.SenderName)
		}
		other := behavior.Trace{Run: behavior.TraceRun{ID: 9, TriggerKind: behavior.TriggerChatMessage, Trigger: trigger}}
		behavior.ResolveTraceSender(ctx, store, workspaceB, &other)
		if other.Run.SenderName != "" {
			t.Fatalf("trace in workspace B used A's name: %q", other.Run.SenderName)
		}
	})
}

// TC-R5: the approval store accepts the two Phase 4 approval kinds (the
// "Đặt tên" button and the assistant's confirm-before-write), and a decided
// or expired approval cannot be decided again.
func TestPhase4ApprovalKinds(t *testing.T) {
	ctx, pool := testDB(t)
	mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'A')`, workspaceA)
	store, err := NewBehaviorStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	nonces := map[behavior.ApprovalKind]string{
		behavior.ApprovalContactAlias:    "aliasNonce_0123456789abcdef",
		behavior.ApprovalAssistantAction: "assistNonce_0123456789abcdef",
	}
	for kind, nonce := range nonces {
		t.Run("TC-R5 accepts "+string(kind), func(t *testing.T) {
			ap, err := store.CreateApproval(ctx, workspaceA, behavior.NewApproval{
				Kind: kind, Action: "alias", Resource: "clickup:106", Payload: json.RawMessage(`{"ref":"clickup:106","name":"anh Định"}`),
				Nonce: nonce, ExpiresAt: now.Add(10 * time.Minute),
			})
			if err != nil {
				t.Fatalf("create %s: %v", kind, err)
			}
			got, err := store.GetApprovalByNonce(ctx, workspaceA, nonce)
			if err != nil || got.Kind != kind || got.ID != ap.ID || got.Decision != "" {
				t.Fatalf("read back=%+v err=%v", got, err)
			}
			if _, err := store.Decide(ctx, workspaceA, nonce, behavior.DecisionApproved, "", "telegram:77", now); err != nil {
				t.Fatalf("first decision: %v", err)
			}
			if _, err := store.Decide(ctx, workspaceA, nonce, behavior.DecisionApproved, "", "telegram:77", now); !errors.Is(err, behavior.ErrDecided) {
				t.Fatalf("second decision err=%v, want ErrDecided", err)
			}
		})
	}
	t.Run("TC-R5 expired assistant action cannot be decided", func(t *testing.T) {
		nonce := "expiredNonce_0123456789abcdef"
		if _, err := store.CreateApproval(ctx, workspaceA, behavior.NewApproval{Kind: behavior.ApprovalAssistantAction, Action: "prefs_style", Resource: "detailed", Nonce: nonce, ExpiresAt: now.Add(time.Minute)}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Decide(ctx, workspaceA, nonce, behavior.DecisionApproved, "", "telegram:77", now.Add(2*time.Minute)); !errors.Is(err, behavior.ErrExpired) {
			t.Fatalf("expired decision err=%v, want ErrExpired", err)
		}
	})
	t.Run("TC-R5 unknown kind is refused", func(t *testing.T) {
		if _, err := store.CreateApproval(ctx, workspaceA, behavior.NewApproval{Kind: "assistant_exec", Nonce: "unknownNonce_0123456789abcdef", ExpiresAt: now.Add(time.Minute)}); err == nil {
			t.Fatal("unknown approval kind accepted")
		}
	})
}
