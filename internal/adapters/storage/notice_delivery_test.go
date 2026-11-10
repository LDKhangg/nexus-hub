package storage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/relay"
)

func TestNoticeClaimRecovery(t *testing.T) {
	ctx, pool, r := newNoticeDB(t)
	e := noticeEvent("h1", "task-1")
	if _, err := r.Accept(ctx, []domain.NormalizedEvent{e}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Second)
	first, err := r.Claim(ctx, now)
	if err != nil || first == nil || first.Attempts != 1 {
		t.Fatal(first, err)
	}
	if j, err := r.Claim(ctx, now); err != nil || j != nil {
		t.Fatal("leased twice", j, err)
	}
	second, err := r.Claim(ctx, now.Add(61*time.Second))
	if err != nil || second == nil || second.ClaimToken == first.ClaimToken || second.Attempts != 2 {
		t.Fatal(second, err)
	}
	if err := r.MarkSent(ctx, *first, relay.DeliveryReceipt{MessageID: 1}); !errors.Is(err, relay.ErrStaleNoticeClaim) {
		t.Fatal("stale ack accepted", err)
	}
	if err := r.MarkRetry(ctx, *first, now, "network"); !errors.Is(err, relay.ErrStaleNoticeClaim) {
		t.Fatal("stale retry accepted", err)
	}
	if err := r.MarkFailed(ctx, *first, "bad"); !errors.Is(err, relay.ErrStaleNoticeClaim) {
		t.Fatal("stale failure accepted", err)
	}
	if target, err := r.ResolveTarget(ctx, *second); err != nil || target.ChatID != "-10042" {
		t.Fatal(target, err)
	}
	// A committed claim must release its connection before any remote call.
	if pool.Stat().AcquiredConns() != 0 {
		t.Fatal("claim held connection")
	}
	mustExec(t, ctx, pool, `UPDATE integrations SET config=jsonb_set(config,'{target_integration_id}',to_jsonb($1::text)) WHERE id=$2`, missingID, integrationA)
	if _, err := r.ResolveTarget(ctx, *second); err != nil {
		t.Fatal("pending job rerouted by source change", err)
	}
	mustExec(t, ctx, pool, `UPDATE integrations SET is_enabled=false WHERE id=$1`, integrationB)
	if _, err := r.ResolveTarget(ctx, *second); err == nil {
		t.Fatal("disabled target sent")
	}
	mustExec(t, ctx, pool, `UPDATE integrations SET is_enabled=true WHERE id=$1`, integrationB)
	if err := r.MarkRetry(ctx, *second, now.Add(90*time.Second), "network"); err != nil {
		t.Fatal(err)
	}
	// A new repository instance models restart; due work remains in PostgreSQL.
	fresh, _ := NewNoticeRepository(pool, r.newID)
	third, err := fresh.Claim(ctx, now.Add(91*time.Second))
	if err != nil || third == nil {
		t.Fatal(third, err)
	}
	if err := fresh.MarkSent(ctx, *third, relay.DeliveryReceipt{MessageID: 99}); err != nil {
		t.Fatal(err)
	}
	var status string
	var id int64
	if err := pool.QueryRow(ctx, `SELECT status,telegram_message_id FROM notification_outbox`).Scan(&status, &id); err != nil || status != "sent" || id != 99 {
		t.Fatal(status, id, err)
	}
}

func TestNoticeConcurrentClaimAndFinalLease(t *testing.T) {
	ctx, pool, r := newNoticeDB(t)
	if _, err := r.Accept(ctx, []domain.NormalizedEvent{noticeEvent("h1", "task-1")}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Second)
	jobs := make(chan *relay.NoticeJob, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); j, err := r.Claim(ctx, now); jobs <- j; errs <- err }()
	}
	wg.Wait()
	close(jobs)
	close(errs)
	n := 0
	for j := range jobs {
		if j != nil {
			n++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n != 1 {
		t.Fatal("concurrent duplicate claims", n)
	}
	mustExec(t, ctx, pool, `UPDATE notification_outbox SET attempts=8,locked_until=$1`, now.Add(-time.Second))
	if j, err := r.Claim(context.Background(), now); err != nil || j != nil {
		t.Fatal(j, err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM notification_outbox`).Scan(&status); err != nil || status != "failed" {
		t.Fatal("final lease stranded", status, err)
	}
}
