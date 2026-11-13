package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/jackc/pgx/v5/pgxpool"
)

func s23Store(t *testing.T) (context.Context, *pgxpool.Pool, *BehaviorStore) {
	t.Helper()
	ctx, pool := testDB(t)
	mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'P3 owner'),($2,'P3 other')`, workspaceA, workspaceB)
	s, err := NewBehaviorStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, pool, s
}

func s23Version(t *testing.T, ctx context.Context, s *BehaviorStore, name string) behavior.Version {
	t.Helper()
	_, err := s.SaveDraft(ctx, workspaceA, name, json.RawMessage(`{"schema":1,"name":"`+name+`"}`), nil, "original YAML\n")
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.Publish(ctx, workspaceA, name, nil)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func s23Run(t *testing.T, ctx context.Context, s *BehaviorStore, v behavior.Version, rev string) behavior.Run {
	t.Helper()
	r, created, err := s.CreateRun(ctx, workspaceA, behavior.NewRun{BehaviorID: v.BehaviorID, VersionID: v.ID, Version: v.Version, TriggerKind: behavior.TriggerTaskStatusChanged, Subject: behavior.TaskSubject("abc"), Revision: rev})
	if err != nil || !created {
		t.Fatalf("create run: created=%v error=%v", created, err)
	}
	return r
}

func s23Item(t *testing.T, ctx context.Context, s *BehaviorStore, r behavior.Run, key string, class behavior.Class) behavior.Item {
	t.Helper()
	it, created, err := s.AddItem(ctx, workspaceA, behavior.NewItem{RunID: r.ID, Key: key, Text: "Câu hỏi tiếng Việt 🧪", Proposal: "Use pagination", Topic: "pagination", Class: class, Rule: "contract", Confidence: .9})
	if err != nil || !created {
		t.Fatalf("add item: created=%v error=%v", created, err)
	}
	return it
}

// P3-DB-01 (REQ-1, REQ-3): draft replacement and concurrent publish cannot mutate a pinned run.
func TestS23PublishedVersionsStayPinnedAndWorkspaceIsolated(t *testing.T) {
	ctx, _, s := s23Store(t)
	v := s23Version(t, ctx, s, "pinned")
	r := s23Run(t, ctx, s, v, "revision-1")
	if _, err := s.SaveDraft(ctx, workspaceA, v.Name, json.RawMessage(`{"schema":1,"instructions":"new"}`), json.RawMessage(`{"status":"new"}`), "new YAML\n"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	versions := make(chan int, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			next, err := s.Publish(ctx, workspaceA, v.Name, nil)
			versions <- next.Version
			errs <- err
		}()
	}
	wg.Wait()
	close(versions)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[int]bool{}
	for n := range versions {
		if n < 2 || n > 9 || seen[n] {
			t.Fatalf("non-monotonic or duplicate version %d", n)
		}
		seen[n] = true
	}
	got, err := s.GetVersion(ctx, workspaceA, v.Name, 1)
	if err != nil || string(got.Document) != string(v.Document) || got.Source != v.Source {
		t.Fatalf("published version mutated: %v", err)
	}
	_, mutateErr := s.pool.Exec(ctx, `UPDATE behavior_versions SET source='mutated' WHERE id=$1`, v.ID)
	requirePGCode(t, mutateErr, "23514")
	stored, err := s.GetRun(ctx, workspaceA, r.ID)
	if err != nil || stored.VersionID != v.ID || stored.Version != 1 {
		t.Fatalf("run lost pin: %v", err)
	}
	if _, err := s.GetVersionByID(ctx, workspaceB, v.ID); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("cross-workspace version: %v", err)
	}
	if _, err := s.GetRun(ctx, workspaceB, r.ID); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("cross-workspace run: %v", err)
	}
	blocked := errors.New("schema rejected")
	if _, err := s.Publish(ctx, workspaceA, v.Name, func(behavior.Behavior) error { return blocked }); !errors.Is(err, blocked) {
		t.Fatal("validation failure ignored")
	}
	list, err := s.ListVersions(ctx, workspaceA, v.Name)
	if err != nil || len(list) != 9 {
		t.Fatalf("failed publish consumed version: count=%d error=%v", len(list), err)
	}
}

// P3-DB-08 (REQ-9, REQ-12, REQ-18): a malformed item batch rolls back all
// rows; repeated keys preserve the original classified item and status CAS prevents stale writes.
func TestS23ItemBatchAtomicityDedupeAndStatusFence(t *testing.T) {
	ctx, _, s := s23Store(t)
	v := s23Version(t, ctx, s, "atomic-items")
	r := s23Run(t, ctx, s, v, "one")
	claimed, err := s.ClaimRun(ctx, time.Now().Add(time.Second), time.Minute, 3)
	if err != nil || claimed == nil || claimed.ID != r.ID {
		t.Fatalf("claim: %v", err)
	}
	good := behavior.NewItem{Key: "one", Text: "original", Proposal: "approved proposal", Class: behavior.ClassAuto, Rule: "verified_citation", Confidence: .99}
	bad := good
	bad.Key = "two"
	bad.Confidence = 2
	if _, err := s.AddRunItems(ctx, workspaceA, r.ID, claimed.ClaimToken, []behavior.NewItem{good, bad}); !errors.Is(err, behavior.ErrInvalid) {
		t.Fatalf("bad item batch accepted: %v", err)
	}
	if items, err := s.ListItems(ctx, workspaceA, r.ID); err != nil || len(items) != 0 {
		t.Fatal("partially committed invalid model item batch")
	}
	items, err := s.AddRunItems(ctx, workspaceA, r.ID, claimed.ClaimToken, []behavior.NewItem{good})
	if err != nil || len(items) != 1 {
		t.Fatalf("valid item batch: %v", err)
	}
	good.Text = "overwritten"
	good.Class = behavior.ClassAsk
	if _, err := s.AddRunItems(ctx, workspaceA, r.ID, claimed.ClaimToken, []behavior.NewItem{good}); err != nil {
		t.Fatal(err)
	}
	it, err := s.GetItem(ctx, workspaceA, items[0].ID)
	if err != nil || it.Text != "original" || it.Class != behavior.ClassAuto {
		t.Fatal("repeat model key overwrote pinned decision")
	}
	result := json.RawMessage(`{"actions":[{"type":"telegram.message","status":"delivered"}]}`)
	if _, err := s.TransitionItem(ctx, workspaceA, it.ID, []behavior.ItemStatus{behavior.ItemDrafted}, behavior.ItemDone, result); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionItem(ctx, workspaceA, it.ID, []behavior.ItemStatus{behavior.ItemDrafted}, behavior.ItemFailed, json.RawMessage(`{}`)); !errors.Is(err, behavior.ErrConflict) {
		t.Fatal("stale item transition replaced completed action")
	}
	it, err = s.GetItem(ctx, workspaceA, it.ID)
	if err != nil || it.Status != behavior.ItemDone || !strings.Contains(string(it.ActionResult), "delivered") {
		t.Fatal("stale update destroyed durable action result")
	}
}

// P3-DB-02 (REQ-3, REQ-4, REQ-8, REQ-19): one durable run, lease fencing, delayed retry and terminal exhaustion.
func TestS23ConcurrentRunDedupeLeaseRecoveryAndRetry(t *testing.T) {
	ctx, _, s := s23Store(t)
	v := s23Version(t, ctx, s, "leases")
	request := behavior.NewRun{BehaviorID: v.BehaviorID, VersionID: v.ID, Version: 1, TriggerKind: behavior.TriggerTaskStatusChanged, Subject: behavior.TaskSubject("abc"), Revision: "same"}
	var created atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	ids := make(chan int64, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, ok, err := s.CreateRun(ctx, workspaceA, request)
			if ok {
				created.Add(1)
			}
			ids <- r.ID
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if created.Load() != 1 {
		t.Fatalf("created %d duplicate runs", created.Load())
	}
	var id int64
	for n := range ids {
		if id == 0 {
			id = n
		}
		if n != id {
			t.Fatal("dedupe returned different runs")
		}
	}
	now := time.Now().Add(time.Second)
	first, err := s.ClaimRun(ctx, now, time.Minute, 2)
	if err != nil || first == nil || first.ID != id {
		t.Fatalf("first claim: %v", err)
	}
	if next, err := s.ClaimRun(ctx, now, time.Minute, 2); err != nil || next != nil {
		t.Fatalf("live lease stolen: %v", err)
	}
	second, err := s.ClaimRun(ctx, now.Add(2*time.Minute), time.Minute, 2)
	if err != nil || second == nil || second.ID != id || second.Attempts != 2 || second.ClaimToken == first.ClaimToken || second.CorrelationID != first.CorrelationID {
		t.Fatalf("lease recovery lost identity: %v", err)
	}
	for name, write := range map[string]func() error{
		"usage":   func() error { return s.AddUsage(ctx, id, first.ClaimToken, behavior.Usage{Steps: 1}) },
		"finish":  func() error { return s.FinishRun(ctx, id, first.ClaimToken, behavior.RunDone, "") },
		"retry":   func() error { return s.RetryRun(ctx, id, first.ClaimToken, now, "retry") },
		"context": func() error { return s.SetRunContext(ctx, id, first.ClaimToken, json.RawMessage(`{"version":1}`)) },
	} {
		if err := write(); !errors.Is(err, behavior.ErrStaleClaim) {
			t.Errorf("%s stale worker accepted: %v", name, err)
		}
	}
	if err := s.RetryRun(ctx, id, second.ClaimToken, now.Add(4*time.Minute), "read_transient"); err != nil {
		t.Fatal(err)
	}
	if claim, err := s.ClaimRun(ctx, now.Add(3*time.Minute), time.Minute, 3); err != nil || claim != nil {
		t.Fatalf("retry ran early: %v", err)
	}
	third, err := s.ClaimRun(ctx, now.Add(5*time.Minute), time.Minute, 3)
	if err != nil || third == nil || third.Attempts != 3 {
		t.Fatalf("due retry unavailable: %v", err)
	}
	if claim, err := s.ClaimRun(ctx, now.Add(7*time.Minute), time.Minute, 3); err != nil || claim != nil {
		t.Fatalf("exhausted run reclaimed: %v", err)
	}
	r, err := s.GetRun(ctx, workspaceA, id)
	if err != nil || r.Status != behavior.RunFailed || r.FinishedAt == nil {
		t.Fatalf("exhaustion not durable: status=%s error=%v", r.Status, err)
	}
}

// P3-DB-03 (REQ-5): simultaneous repeated Telegram updates have one effect, recover after a crash, fence old workers.
func TestS23InboundDedupeAndLeaseFence(t *testing.T) {
	ctx, _, s := s23Store(t)
	u := behavior.InboundUpdate{UpdateID: 4242, ChatID: 42, Kind: behavior.UpdateCommand, Command: "/review", Args: "abc"}
	var count atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.RecordUpdate(ctx, workspaceA, u)
			if ok {
				count.Add(1)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if count.Load() != 1 {
		t.Fatalf("stored %d repeated updates", count.Load())
	}
	now := time.Now().Add(time.Second)
	a, err := s.ClaimUpdate(ctx, now, time.Minute)
	if err != nil || a == nil {
		t.Fatalf("claim: %v", err)
	}
	if b, err := s.ClaimUpdate(ctx, now, time.Minute); err != nil || b != nil {
		t.Fatalf("concurrent inbox lease: %v", err)
	}
	b, err := s.ClaimUpdate(ctx, now.Add(2*time.Minute), time.Minute)
	if err != nil || b == nil || a.ID != b.ID || b.Attempts != 2 {
		t.Fatalf("inbox not recovered: %v", err)
	}
	if err := s.FinishUpdate(ctx, a.ID, a.ClaimToken, ""); !errors.Is(err, behavior.ErrStaleClaim) {
		t.Fatalf("stale completion: %v", err)
	}
	if err := s.FinishUpdate(ctx, b.ID, b.ClaimToken, ""); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.RecordUpdate(ctx, workspaceA, u); err != nil || ok {
		t.Fatal("processed update replayed")
	}
	if b, err := s.ClaimUpdate(ctx, now.Add(4*time.Minute), time.Minute); err != nil || b != nil {
		t.Fatal("finished update reclaimed")
	}
}

// P3-DB-04 (REQ-12, REQ-14): competing button presses preserve exactly one actor, answer, timestamp and binding.
func TestS23ApprovalDecisionRaceExpiryAndActorAudit(t *testing.T) {
	ctx, _, s := s23Store(t)
	v := s23Version(t, ctx, s, "approvals")
	r := s23Run(t, ctx, s, v, "one")
	it := s23Item(t, ctx, s, r, "ask", behavior.ClassAsk)
	now := time.Now().UTC().Truncate(time.Microsecond)
	a, err := s.CreateApproval(ctx, workspaceA, behavior.NewApproval{Kind: behavior.ApprovalItemAnswer, RunID: r.ID, ItemID: it.ID, Action: "clickup.description_block", Resource: r.Subject, Nonce: "s23_approval_nonce_0001", Options: json.RawMessage(`["yes","no"]`), ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	// Decide's at_time checks expiry; SQL records decided_at using DB now().
	// Bound that audit instant with the same database's real clock instead.
	var decisionStart, decisionEnd time.Time
	if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&decisionStart); err != nil {
		t.Fatal("cannot read database clock before approval decisions")
	}
	var winners atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Decide(ctx, workspaceA, a.Nonce, behavior.DecisionAnswered, fmt.Sprintf("answer-%d", i), fmt.Sprintf("telegram:%d", i), now)
			if err == nil {
				winners.Add(1)
			} else if !errors.Is(err, behavior.ErrDecided) {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&decisionEnd); err != nil {
		t.Fatal("cannot read database clock after approval decisions")
	}
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if winners.Load() != 1 {
		t.Fatalf("decision winners=%d", winners.Load())
	}
	got, err := s.GetApproval(ctx, workspaceA, a.ID)
	if err != nil {
		t.Fatal("cannot read stored approval audit")
	}
	if got.Decision != behavior.DecisionAnswered || got.RunID != r.ID || got.ItemID != it.ID || got.Resource != r.Subject || got.Action != a.Action {
		t.Fatalf("audit binding lost: approval_id=%d decision=%q run_id=%d want_run_id=%d item_id=%d want_item_id=%d resource_matches=%t action_matches=%t", got.ID, got.Decision, got.RunID, r.ID, got.ItemID, it.ID, got.Resource == r.Subject, got.Action == a.Action)
	}
	if got.DecidedAt == nil {
		t.Fatalf("audit timestamp missing: approval_id=%d", got.ID)
	}
	if got.DecidedAt.Before(decisionStart) || got.DecidedAt.After(decisionEnd) {
		t.Fatalf("audit timestamp outside DB decision window: approval_id=%d decided_at=%s start=%s end=%s", got.ID, got.DecidedAt.UTC().Format(time.RFC3339Nano), decisionStart.UTC().Format(time.RFC3339Nano), decisionEnd.UTC().Format(time.RFC3339Nano))
	}
	var actor int
	if _, err := fmt.Sscanf(got.DecidedBy, "telegram:%d", &actor); err != nil || got.Answer != fmt.Sprintf("answer-%d", actor) {
		t.Fatal("winning actor and answer disagree")
	}
	if _, err := s.Decide(ctx, workspaceB, a.Nonce, behavior.DecisionApproved, "", "attacker", now); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatalf("foreign decision: %v", err)
	}
	exp, err := s.CreateApproval(ctx, workspaceA, behavior.NewApproval{Kind: behavior.ApprovalPostDraft, RunID: r.ID, ItemID: it.ID, Nonce: "s23_approval_nonce_0002", ExpiresAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(ctx, workspaceA, exp.Nonce, behavior.DecisionApproved, "", "telegram:42", now); !errors.Is(err, behavior.ErrExpired) {
		t.Fatalf("expiry boundary accepted: %v", err)
	}
	if _, err := s.ExpireDue(ctx, now.Add(time.Second), 100); err != nil {
		t.Fatal(err)
	}
	exp, err = s.GetApproval(ctx, workspaceA, exp.ID)
	if err != nil || exp.Decision != behavior.DecisionExpired || exp.Answer != "" || exp.DecidedBy != "system" || exp.DecidedAt == nil {
		t.Fatalf("expiry audit missing or fabricated owner answer: %v", err)
	}
}

// P3-DB-05 (REQ-16, REQ-22, REQ-23, REQ-25): snapshots are write-once, stats are per version, memory forget/undo removes evidence only.
func TestS23SnapshotProfileMemoryAndVersionStats(t *testing.T) {
	ctx, _, s := s23Store(t)
	v := s23Version(t, ctx, s, "memory")
	r := s23Run(t, ctx, s, v, "one")
	it := s23Item(t, ctx, s, r, "auto", behavior.ClassAuto)
	snap := json.RawMessage(`{"description_before":"outside\noriginal","captured_at":1}`)
	if ok, err := s.SetSnapshot(ctx, workspaceA, r.ID, snap); err != nil || !ok {
		t.Fatalf("snapshot: %v", err)
	}
	if ok, err := s.SetSnapshot(ctx, workspaceA, r.ID, json.RawMessage(`{"overwrite":true}`)); err != nil || ok {
		t.Fatal("snapshot overwritten")
	}
	if err := s.RateItem(ctx, workspaceA, it.ID, behavior.RatingDown, "Không chính xác 🧪"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUndone(ctx, workspaceA, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUndone(ctx, workspaceA, r.ID); !errors.Is(err, behavior.ErrConflict) {
		t.Fatal("undo applied twice")
	}
	stored, err := s.GetRun(ctx, workspaceA, r.ID)
	var decoded map[string]any
	if err != nil || json.Unmarshal(stored.Snapshot, &decoded) != nil || decoded["description_before"] != "outside\noriginal" {
		t.Fatal("undo snapshot changed")
	}
	second, err := s.Publish(ctx, workspaceA, v.Name, nil)
	if err != nil {
		t.Fatal(err)
	}
	r2 := s23Run(t, ctx, s, second, "two")
	s23Item(t, ctx, s, r2, "notify", behavior.ClassNotify)
	stats, err := s.Stats(ctx, workspaceA, v.Name)
	if err != nil || len(stats) != 2 {
		t.Fatalf("stats: %v", err)
	}
	for _, st := range stats {
		switch st.Version {
		case 1:
			if st.Runs != 1 || st.AutoItems != 1 || st.NotifyItems != 0 || st.RatedDown != 1 || st.Undos != 1 {
				t.Fatalf("v1 counters: %+v", st)
			}
		case 2:
			if st.Runs != 1 || st.AutoItems != 0 || st.NotifyItems != 1 || st.RatedDown != 0 || st.Undos != 0 {
				t.Fatalf("v2 counters: %+v", st)
			}
		}
	}
	p := behavior.DefaultProfile()
	start, end := 22*60, 7*60
	p.QuietStart = &start
	p.QuietEnd = &end
	p.Tone = "ngắn gọn"
	p.DefaultModel = "fake-model"
	p.DigestMode = behavior.DigestHourly
	p.Budgets = json.RawMessage(`{"max_tokens":1234}`)
	if _, err := s.SaveProfile(ctx, workspaceA, p); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetProfile(ctx, workspaceA)
	if err != nil || got.Tone != p.Tone || got.QuietStart == nil || *got.QuietStart != start || got.QuietEnd == nil || *got.QuietEnd != end || got.DefaultModel != p.DefaultModel || got.DigestMode != p.DigestMode {
		t.Fatalf("profile roundtrip: %v", err)
	}
	other, err := s.GetProfile(ctx, workspaceB)
	if err != nil || other.Tone != "" || other.QuietStart != nil {
		t.Fatal("profile leaked workspace")
	}
	d := behavior.NewDecision{Topic: "pagination", Question: "pagination size", Answer: "use cursor", Scope: behavior.DecisionScope{Behavior: v.Name, List: "list-a", Repo: "acme/app"}, Source: behavior.SourceAutoPosted, RunID: r.ID, ItemID: it.ID}
	posted, err := s.AddDecision(ctx, workspaceA, d)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.SearchDecisions(ctx, workspaceA, "pagination", v.Name, 10)
	if err != nil || len(list) != 1 || list[0].Scope != d.Scope || list[0].Locator() != posted.Locator() {
		t.Fatalf("scoped memory lost: %v", err)
	}
	if list, err := s.SearchDecisions(ctx, workspaceA, "pagination", "other", 10); err != nil || len(list) != 0 {
		t.Fatal("wrong behavior memory leaked")
	}
	if list, err := s.SearchReusableDecisions(ctx, workspaceB, "pagination", 10); err != nil || len(list) != 0 {
		t.Fatal("foreign memory leaked")
	}
	d.Source = behavior.SourceAutoUndone
	if _, err := s.AddDecision(ctx, workspaceA, d); err != nil {
		t.Fatal(err)
	}
	if list, err := s.SearchReusableDecisions(ctx, workspaceA, "pagination", 10); err != nil || len(list) != 0 {
		t.Fatal("undone answer remains reusable evidence")
	}
	if err := s.ForgetDecision(ctx, workspaceA, posted.ID); err != nil {
		t.Fatal(err)
	}
	forgotten, err := s.GetDecision(ctx, workspaceA, posted.ID)
	if err != nil || forgotten.ForgottenAt == nil || forgotten.Answer != posted.Answer {
		t.Fatal("forget destroyed audit record")
	}
	if err := s.ForgetDecision(ctx, workspaceA, posted.ID); !errors.Is(err, behavior.ErrNotFound) {
		t.Fatal("forget repeat accepted")
	}
}

// P3-DB-06 (REQ-17, REQ-22): quiet-hour/hourly digest entries are durable,
// batched only when due, and message claims prevent immediate or queued duplicates.
func TestS23DigestQueueDueBatchRetryAndMessageIdempotency(t *testing.T) {
	ctx, pool, s := s23Store(t)
	mustExec(t, ctx, pool, `INSERT INTO integrations(id,workspace_id,platform,role,config) VALUES($1,$2,'telegram','control','{}')`, integrationA, workspaceA)
	v := s23Version(t, ctx, s, "digest")
	r1 := s23Run(t, ctx, s, v, "one")
	r2 := s23Run(t, ctx, s, v, "two")
	p := behavior.DefaultProfile()
	p.Timezone = "UTC"
	p.DigestMode = behavior.DigestHourly
	start, end := 22*60, 7*60
	p.QuietStart = &start
	p.QuietEnd = &end
	now := time.Date(2026, 11, 13, 23, 30, 0, 0, time.UTC)
	due, deferred := behavior.DigestDue(p, now)
	if !deferred || !due.Equal(time.Date(2026, 11, 14, 7, 0, 0, 0, time.UTC)) {
		t.Fatal("digest due ignores overnight quiet hours")
	}
	chat := behavior.ChatRef{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: 42}
	for _, r := range []behavior.Run{r1, r2} {
		if ok, err := s.QueueDigest(ctx, workspaceA, r.ID, behavior.DigestKeyNotify, chat, due); err != nil || !ok {
			t.Fatalf("queue digest: %v", err)
		}
		if ok, err := s.QueueDigest(ctx, workspaceA, r.ID, behavior.DigestKeyNotify, chat, due); err != nil || ok {
			t.Fatal("queued duplicate digest")
		}
		if ok, _, err := s.ClaimMessage(ctx, workspaceA, r.ID, behavior.DigestKeyNotify); err != nil || ok {
			t.Fatal("queued digest also claimable for immediate send")
		}
	}
	if b, err := s.ClaimDigestBatch(ctx, due.Add(-time.Second), 100); err != nil || b != nil {
		t.Fatal("digest sent during quiet hours")
	}
	b, err := s.ClaimDigestBatch(ctx, due, 100)
	if err != nil || b == nil || len(b.Entries) != 2 || b.Chat != chat {
		t.Fatalf("due batch: %v", err)
	}
	if other, err := s.ClaimDigestBatch(ctx, due, 100); err != nil || other != nil {
		t.Fatal("digest batch claimed twice")
	}
	if err := s.FinishDigestBatch(ctx, b.ID, "invalid-token", 10); !errors.Is(err, behavior.ErrStaleClaim) {
		t.Fatal("wrong digest worker accepted")
	}
	retryAt := due.Add(time.Minute)
	if err := s.ReleaseDigestBatch(ctx, b.ID, b.ClaimToken, retryAt); err != nil {
		t.Fatal(err)
	}
	if other, err := s.ClaimDigestBatch(ctx, due, 100); err != nil || other != nil {
		t.Fatal("released digest retried before backoff")
	}
	retry, err := s.ClaimDigestBatch(ctx, retryAt, 100)
	if err != nil || retry == nil || len(retry.Entries) != 2 {
		t.Fatalf("digest retry lost entries: %v", err)
	}
	if err := s.FinishDigestBatch(ctx, retry.ID, retry.ClaimToken, 101); err != nil {
		t.Fatal(err)
	}
	for _, r := range []behavior.Run{r1, r2} {
		if ok, id, err := s.ClaimMessage(ctx, workspaceA, r.ID, behavior.DigestKeyNotify); err != nil || ok || id != 101 {
			t.Fatal("sent batch message not retained per run")
		}
	}
	if other, err := s.ClaimDigestBatch(ctx, retryAt.Add(time.Hour), 100); err != nil || other != nil {
		t.Fatal("sent digest reclaimed")
	}
}

// P3-DB-07 (REQ-17, REQ-22; accepted T8 ruling): a crash after claim has an
// unknown delivery outcome and must not resend. Residual risk: an unsent digest can be lost.
func TestS23DigestClaimCrashNeverResendsUnknownOutcome(t *testing.T) {
	ctx, pool, s := s23Store(t)
	mustExec(t, ctx, pool, `INSERT INTO integrations(id,workspace_id,platform,role,config) VALUES($1,$2,'telegram','control','{}')`, integrationA, workspaceA)
	v := s23Version(t, ctx, s, "digest-crash")
	r := s23Run(t, ctx, s, v, "one")
	now := time.Now()
	chat := behavior.ChatRef{WorkspaceID: workspaceA, IntegrationID: integrationA, ChatID: 42}
	if ok, err := s.QueueDigest(ctx, workspaceA, r.ID, behavior.DigestKeyNotify, chat, now); err != nil || !ok {
		t.Fatalf("queue: %v", err)
	}
	first, err := s.ClaimDigestBatch(ctx, now.Add(time.Second), 100)
	if err != nil || first == nil {
		t.Fatalf("claim: %v", err)
	}
	// No Release or Finish: this is the exact durable state left by a killed process.
	restarted, err := NewBehaviorStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := restarted.ClaimDigestBatch(ctx, now.Add(24*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != nil {
		t.Fatal("unknown-outcome digest was reclaimed and could be sent twice")
	}
	if claimed, id, err := restarted.ClaimMessage(ctx, workspaceA, r.ID, behavior.DigestKeyNotify); err != nil || claimed || id != 0 {
		t.Fatal("unknown delivery was fabricated as sent or made resendable")
	}
}
