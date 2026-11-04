package relay

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// Đúng: 2 lần flush ("a" rồi "b"). Bug: 1 lần flush "a\nb" sớm, timer mới không còn gì để flush.
func TestEarlyFlush(t *testing.T) {
	bug := 0
	for i := 0; i < 30; i++ {
		var n atomic.Int32
		d := NewMemoryDebouncer(50*time.Millisecond, func(ctx context.Context, e domain.NormalizedEvent) { n.Add(1) })
		d.Push(context.Background(), domain.NormalizedEvent{ExternalID: "x", Content: "a"})
		time.Sleep(40 * time.Millisecond)
		d.mu.Lock()
		time.Sleep(20 * time.Millisecond)
		go d.Push(context.Background(), domain.NormalizedEvent{ExternalID: "x", Content: "b"})
		time.Sleep(2 * time.Millisecond)
		d.mu.Unlock()
		time.Sleep(150 * time.Millisecond)
		if n.Load() == 1 {
			bug++
		}
	}
	t.Logf("số lần event mới bị flush sớm: %d/30", bug)
}
