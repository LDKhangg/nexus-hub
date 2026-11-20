package storage

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

type phase12Sender struct {
	calls  atomic.Int32
	onSend func(ctx context.Context)
}

func (s *phase12Sender) Send(ctx context.Context, _ tenant.TelegramTargetConfig, _ domain.NormalizedEvent) (relay.DeliveryReceipt, error) {
	n := s.calls.Add(1)
	if s.onSend != nil {
		s.onSend(ctx)
	}
	return relay.DeliveryReceipt{MessageID: int64(n)}, nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TC-1 (REQ-1, REQ-2): the same ClickUp event delivered twice in a row is
// stored once; the second delivery is a counted duplicate, not an error.
func TestPhase12ClickUpDuplicateSequentialStoresOnce(t *testing.T) {
	ctx, pool, r := newNoticeDB(t)
	e := noticeEvent("p12-seq", "task-1")
	first, err := r.Accept(ctx, []domain.NormalizedEvent{e})
	if err != nil || first.Accepted != 1 || first.Duplicates != 0 {
		t.Fatalf("TC-1: first %+v %v", first, err)
	}
	second, err := r.Accept(ctx, []domain.NormalizedEvent{e})
	if err != nil || second.Accepted != 0 || second.Duplicates != 1 {
		t.Fatalf("TC-1: second %+v %v", second, err)
	}
	for _, table := range []string{"message_logs", "notification_outbox", "hub_threads"} {
		if n := countRows(t, ctx, pool, table); n != 1 {
			t.Fatalf("TC-1: %s has %d rows, want 1", table, n)
		}
	}
}

// TC-2 (REQ-2, human retry storm): 20 identical deliveries at the same time
// all succeed and exactly one is accepted.
func TestPhase12ClickUpDuplicateConcurrentStoresOnce(t *testing.T) {
	ctx, pool, r := newNoticeDB(t)
	e := noticeEvent("p12-storm", "task-1")
	const n = 20
	var accepted, duplicates atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := r.Accept(ctx, []domain.NormalizedEvent{e})
			if err != nil {
				errs <- err
				return
			}
			accepted.Add(int32(res.Accepted))
			duplicates.Add(int32(res.Duplicates))
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("TC-2: a delivery failed (the sender would see a non-2xx): %v", err)
	}
	if accepted.Load() != 1 || duplicates.Load() != n-1 {
		t.Fatalf("TC-2: accepted %d duplicates %d", accepted.Load(), duplicates.Load())
	}
	if c := countRows(t, ctx, pool, "notification_outbox"); c != 1 {
		t.Fatalf("TC-2: %d outbox rows", c)
	}
}

// TC-5 (REQ-2, abuse): the same key from another source integration is a
// different event; an empty or oversized key never dedups across keys and
// never stores a half record.
func TestPhase12ClickUpDedupKeyEdges(t *testing.T) {
	ctx, pool, r := newNoticeDB(t)
	e := noticeEvent("p12-key", "task-1")
	if _, err := r.Accept(ctx, []domain.NormalizedEvent{e}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, pool, `INSERT INTO integrations(id,workspace_id,platform,role,config) SELECT $1,workspace_id,platform,role,config FROM integrations WHERE id=$2`, missingID, integrationA)
	other := e
	other.IntegrationID = missingID
	if res, err := r.Accept(ctx, []domain.NormalizedEvent{other}); err != nil || res.Accepted != 1 {
		t.Fatalf("TC-5: same key, other source: %+v %v", res, err)
	}
	if n := countRows(t, ctx, pool, "notification_outbox"); n != 2 {
		t.Fatalf("TC-5: %d outbox rows, want 2", n)
	}
	before := countRows(t, ctx, pool, "message_logs")
	empty := noticeEvent("p12-empty", "task-1")
	empty.EventKey = ""
	if _, err := r.Accept(ctx, []domain.NormalizedEvent{empty}); err == nil {
		t.Fatal("TC-5: empty event key accepted")
	}
	huge := noticeEvent("p12-huge", "task-1")
	huge.EventKey = "w1:" + strings.Repeat("k", 100000)
	_, hugeErr := r.Accept(ctx, []domain.NormalizedEvent{huge})
	after := countRows(t, ctx, pool, "message_logs")
	if hugeErr != nil && after != before {
		t.Fatalf("TC-5: oversized key was refused but left %d new message rows", after-before)
	}
	if hugeErr == nil && after != before+1 {
		t.Fatalf("TC-5: oversized key accepted with %d new message rows", after-before)
	}
	if res, err := r.Accept(ctx, []domain.NormalizedEvent{noticeEvent("p12-after", "task-1")}); err != nil || res.Accepted != 1 {
		t.Fatalf("TC-5: a normal event after the bad ones: %+v %v", res, err)
	}
}

// TC-6 (REQ-1): store-then-ack is atomic. A batch with a record the database
// refuses stores nothing and returns an error (so the sender sees a non-2xx
// and retries); the retry stores it once.
func TestPhase12ClickUpBatchFailureStoresNothingAndRetrySucceeds(t *testing.T) {
	ctx, pool, r := newNoticeDB(t)
	mustExec(t, ctx, pool, `ALTER TABLE notification_outbox ADD CONSTRAINT p12_reject CHECK (event_key <> 'w1:p12-bad')`)
	batch := []domain.NormalizedEvent{noticeEvent("p12-good", "task-1"), noticeEvent("p12-bad", "task-1")}
	if _, err := r.Accept(ctx, batch); err == nil {
		t.Fatal("TC-6: failing batch reported success")
	}
	for _, table := range []string{"message_logs", "notification_outbox", "hub_threads"} {
		if n := countRows(t, context.Background(), pool, table); n != 0 {
			t.Fatalf("TC-6: %s kept %d rows from a failed batch", table, n)
		}
	}
	mustExec(t, ctx, pool, `ALTER TABLE notification_outbox DROP CONSTRAINT p12_reject`)
	res, err := r.Accept(ctx, batch)
	if err != nil || res.Accepted != 2 {
		t.Fatalf("TC-6: retry %+v %v", res, err)
	}
	if n := countRows(t, ctx, pool, "notification_outbox"); n != 2 {
		t.Fatalf("TC-6: %d outbox rows after retry", n)
	}
}

func outboxSent(t *testing.T, ctx context.Context, r *NoticeRepository) int {
	t.Helper()
	var n int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE status='sent'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TC-7 (REQ-1, REQ-3): a stored event whose claim expired (the worker
// crashed after taking it) is delivered exactly once by the next process.
func TestPhase12CrashedNoticeClaimIsDeliveredOnceAfterRestart(t *testing.T) {
	ctx, pool, r := newNoticeDB(t)
	if _, err := r.Accept(ctx, []domain.NormalizedEvent{noticeEvent("p12-crash", "task-1")}); err != nil {
		t.Fatal(err)
	}
	if j, err := r.Claim(ctx, time.Now().Add(time.Second)); err != nil || j == nil {
		t.Fatalf("setup claim: %v %v", j, err)
	}
	mustExec(t, ctx, pool, `UPDATE notification_outbox SET locked_until = now() - interval '1 second'`)
	restarted, err := NewNoticeRepository(pool, r.newID, DebounceSettings{})
	if err != nil {
		t.Fatal(err)
	}
	sender := &phase12Sender{}
	w1 := relay.NewNoticeWorker(restarted, sender, relay.WorkerSettings{PollInterval: 10 * time.Millisecond})
	w2 := relay.NewNoticeWorker(restarted, sender, relay.WorkerSettings{PollInterval: 10 * time.Millisecond})
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for _, w := range []*relay.NoticeWorker{w1, w2} {
		wg.Add(1)
		go func() { defer wg.Done(); _ = w.Run(wctx) }()
	}
	waitFor(t, "the notice to be sent", func() bool { return outboxSent(t, ctx, restarted) == 1 })
	time.Sleep(200 * time.Millisecond)
	cancel()
	wg.Wait()
	if n := sender.calls.Load(); n != 1 {
		t.Fatalf("TC-7: sender called %d times, want exactly 1", n)
	}
}

// TC-8 (REQ-1): a live claim (not expired) is left alone by a second worker;
// only after it expires is the notice delivered, once.
func TestPhase12LiveNoticeClaimIsNotStolen(t *testing.T) {
	ctx, pool, r := newNoticeDB(t)
	if _, err := r.Accept(ctx, []domain.NormalizedEvent{noticeEvent("p12-live", "task-1")}); err != nil {
		t.Fatal(err)
	}
	if j, err := r.Claim(ctx, time.Now().Add(time.Second)); err != nil || j == nil {
		t.Fatalf("setup claim: %v %v", j, err)
	}
	sender := &phase12Sender{}
	w := relay.NewNoticeWorker(r, sender, relay.WorkerSettings{PollInterval: 10 * time.Millisecond})
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = w.Run(wctx); close(done) }()
	time.Sleep(400 * time.Millisecond)
	if n := sender.calls.Load(); n != 0 {
		t.Fatalf("TC-8: a live claim was taken over (%d sends)", n)
	}
	mustExec(t, ctx, pool, `UPDATE notification_outbox SET locked_until = now() - interval '1 second'`)
	waitFor(t, "the expired claim to be delivered", func() bool { return outboxSent(t, ctx, r) == 1 })
	cancel()
	<-done
	if n := sender.calls.Load(); n != 1 {
		t.Fatalf("TC-8: %d sends, want 1", n)
	}
}

// TC-15 (REQ-4): on stop, a notice being sent finishes and its result is
// stored (hard stays open); the worker then returns without taking more.
func TestPhase12NoticeWorkerDrainsInFlightSend(t *testing.T) {
	ctx, _, r := newNoticeDB(t)
	for _, k := range []string{"p12-d1", "p12-d2"} {
		if _, err := r.Accept(ctx, []domain.NormalizedEvent{noticeEvent(k, "task-"+k)}); err != nil {
			t.Fatal(err)
		}
	}
	inSend := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	sender := &phase12Sender{onSend: func(context.Context) {
		once.Do(func() { close(inSend) })
		<-release
	}}
	w := relay.NewNoticeWorker(r, sender, relay.WorkerSettings{PollInterval: 10 * time.Millisecond})
	stop, stopCancel := context.WithCancel(ctx)
	hard, hardCancel := context.WithCancel(ctx)
	defer hardCancel()
	done := make(chan error, 1)
	go func() { done <- w.RunDrain(stop, hard) }()
	<-inSend
	stopCancel()
	time.Sleep(100 * time.Millisecond)
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("TC-15: RunDrain did not return")
	}
	if n := outboxSent(t, ctx, r); n != 1 {
		t.Fatalf("TC-15: %d notices recorded as sent, want the in-flight one only", n)
	}
	if n := sender.calls.Load(); n != 1 {
		t.Fatalf("TC-15: %d sends, want 1 (no claim after stop)", n)
	}
}

// TC-3 (REQ-2): the same Telegram update_id stored twice makes one row and
// one unit of work; the repeat reports "not created".
func TestPhase12TelegramDuplicateUpdateSequential(t *testing.T) {
	e := newWFEnv(t)
	u := behavior.InboundUpdate{UpdateID: 9001, ChatID: 42, Kind: behavior.UpdateCommand, Command: "/status", Args: "tóm tắt 🚀"}
	if ok, err := e.behaviors.RecordUpdate(e.ctx, workspaceA, u); err != nil || !ok {
		t.Fatalf("TC-3: first %v %v", ok, err)
	}
	if ok, err := e.behaviors.RecordUpdate(e.ctx, workspaceA, u); err != nil || ok {
		t.Fatalf("TC-3: repeat created=%v err=%v", ok, err)
	}
	if n := e.count(`SELECT count(*) FROM telegram_updates WHERE update_id=9001`); n != 1 {
		t.Fatalf("TC-3: %d rows", n)
	}
	now := time.Now().Add(time.Minute)
	first, err := e.behaviors.ClaimUpdate(e.ctx, now, time.Minute)
	if err != nil || first == nil || first.Args != u.Args {
		t.Fatalf("TC-3: claim %+v %v", first, err)
	}
	if again, err := e.behaviors.ClaimUpdate(e.ctx, now, time.Minute); err != nil || again != nil {
		t.Fatalf("TC-3: second unit of work for one update: %+v %v", again, err)
	}
}

// TC-4 (REQ-2, human burst): 20 simultaneous deliveries of one update_id
// create one row; the same update_id in another workspace is its own update.
func TestPhase12TelegramDuplicateUpdateConcurrentAndPerWorkspace(t *testing.T) {
	e := newWFEnv(t)
	u := behavior.InboundUpdate{UpdateID: 9002, ChatID: 42, Kind: behavior.UpdateText}
	var created, failed atomic.Int32
	parallel(20, func(int) {
		ok, err := e.behaviors.RecordUpdate(e.ctx, workspaceA, u)
		if err != nil {
			failed.Add(1)
			return
		}
		if ok {
			created.Add(1)
		}
	})
	if failed.Load() != 0 || created.Load() != 1 {
		t.Fatalf("TC-4: created %d failed %d", created.Load(), failed.Load())
	}
	if ok, err := e.behaviors.RecordUpdate(e.ctx, workspaceB, u); err != nil || !ok {
		t.Fatalf("TC-4: other workspace %v %v", ok, err)
	}
	if n := e.count(`SELECT count(*) FROM telegram_updates WHERE update_id=9002`); n != 2 {
		t.Fatalf("TC-4: %d rows, want one per workspace", n)
	}
	// Abuse: oversized arguments and an unknown kind are refused and stored nowhere.
	bad := []behavior.InboundUpdate{
		{UpdateID: 9003, ChatID: 42, Kind: behavior.UpdateCommand, Command: "/x", Args: strings.Repeat("ạ", 513)},
		{UpdateID: 9004, ChatID: 42, Kind: behavior.UpdateKind("weird")},
	}
	for _, b := range bad {
		if ok, err := e.behaviors.RecordUpdate(e.ctx, workspaceA, b); err == nil || ok {
			t.Fatalf("TC-4: bad update %d accepted (%v %v)", b.UpdateID, ok, err)
		}
	}
	if n := e.count(`SELECT count(*) FROM telegram_updates WHERE update_id IN (9003,9004)`); n != 0 {
		t.Fatalf("TC-4: %d rows from refused updates", n)
	}
}

// TC-7 (REQ-1, REQ-3): a Telegram update whose claim lapsed (crash after the
// 200) is handed out again once, and once finished it is never handed out
// again.
func TestPhase12TelegramCrashedClaimProcessedOnce(t *testing.T) {
	e := newWFEnv(t)
	if ok, err := e.behaviors.RecordUpdate(e.ctx, workspaceA, behavior.InboundUpdate{UpdateID: 9005, ChatID: 42, Kind: behavior.UpdateCommand, Command: "/help"}); err != nil || !ok {
		t.Fatalf("record: %v %v", ok, err)
	}
	now := time.Now().Add(time.Minute)
	first, err := e.behaviors.ClaimUpdate(e.ctx, now, time.Second)
	if err != nil || first == nil {
		t.Fatalf("claim: %v %v", first, err)
	}
	if live, err := e.behaviors.ClaimUpdate(e.ctx, now.Add(500*time.Millisecond), time.Second); err != nil || live != nil {
		t.Fatalf("TC-7: live claim stolen: %+v %v", live, err)
	}
	second, err := e.behaviors.ClaimUpdate(e.ctx, now.Add(2*time.Second), time.Second)
	if err != nil || second == nil || second.UpdateID != first.UpdateID || second.Attempts != 2 || second.ClaimToken == first.ClaimToken {
		t.Fatalf("TC-7: restart claim %+v %v", second, err)
	}
	if err := e.behaviors.FinishUpdate(e.ctx, second.ID, second.ClaimToken, "ok"); err != nil {
		t.Fatalf("TC-7: finish: %v", err)
	}
	if err := e.behaviors.FinishUpdate(e.ctx, first.ID, first.ClaimToken, "ok"); err == nil {
		t.Fatal("TC-7: the crashed claim could still finish the update")
	}
	if again, err := e.behaviors.ClaimUpdate(e.ctx, now.Add(time.Hour), time.Second); err != nil || again != nil {
		t.Fatalf("TC-7: finished update handed out again: %+v %v", again, err)
	}
}
