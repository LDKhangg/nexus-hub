package relay

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

type recorder struct {
	mu     sync.Mutex
	events []domain.NormalizedEvent
}

func (r *recorder) flush(_ context.Context, e domain.NormalizedEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) snapshot() []domain.NormalizedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.NormalizedEvent(nil), r.events...)
}

func ev(content string) domain.NormalizedEvent {
	return domain.NormalizedEvent{WorkspaceID: "w", Source: domain.SourceClickup, ExternalID: "x", Content: content}
}

// Timer cũ đã fire (đang chờ lock) không được flush event đã gộp thêm sau đó.
func TestStaleTimerDoesNotFlushEarly(t *testing.T) {
	rec := &recorder{}
	d := NewMemoryDebouncer(50*time.Millisecond, 0, rec.flush)
	ctx := context.Background()
	key := ev("").DebounceKey()

	d.Push(ctx, ev("a"))
	d.mu.Lock()
	staleGen := d.pending[key].gen
	d.mu.Unlock()

	d.Push(ctx, ev("b"))
	d.flushKey(key, staleGen) // giả lập callback của timer cũ chạy muộn

	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("stale timer flushed early: %+v", got)
	}

	time.Sleep(120 * time.Millisecond)
	got := rec.snapshot()
	if len(got) != 1 || got[0].Content != "a\nb" {
		t.Fatalf("want 1 flush with %q, got %+v", "a\nb", got)
	}
}

func TestMaxWaitForcesFlush(t *testing.T) {
	rec := &recorder{}
	d := NewMemoryDebouncer(60*time.Millisecond, 100*time.Millisecond, rec.flush)
	ctx := context.Background()

	// Push liên tục mỗi 30ms: không có maxWait thì không bao giờ flush.
	for i := 0; i < 6; i++ {
		d.Push(ctx, ev("m"))
		time.Sleep(30 * time.Millisecond)
	}
	if len(rec.snapshot()) == 0 {
		t.Fatal("maxWait did not force a flush")
	}
}

func TestFlushAll(t *testing.T) {
	rec := &recorder{}
	d := NewMemoryDebouncer(time.Hour, 0, rec.flush)
	d.Push(context.Background(), ev("a"))
	d.FlushAll()

	got := rec.snapshot()
	if len(got) != 1 || got[0].Content != "a" {
		t.Fatalf("FlushAll: got %+v", got)
	}
	d.FlushAll()
	if len(rec.snapshot()) != 1 {
		t.Fatal("FlushAll flushed twice")
	}
}

func TestEmptyContentNotMerged(t *testing.T) {
	rec := &recorder{}
	d := NewMemoryDebouncer(time.Hour, 0, rec.flush)
	d.Push(context.Background(), ev("a"))
	d.Push(context.Background(), ev(""))
	d.FlushAll()
	if got := rec.snapshot(); got[0].Content != "a" {
		t.Fatalf("got %q", got[0].Content)
	}
}
