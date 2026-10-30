package relay

import (
	"context"
	"sync"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// interface cho phase 2 scale redis
type EventDebounce interface {
	Push(ctx context.Context, event domain.NormalizedEvent)
}

type MemoryDebouncer struct {
	duration    time.Duration
	mu          sync.Mutex
	timers      map[string]*time.Timer
	accumulater map[string]domain.NormalizedEvent
	onFlush     func(ctx context.Context, event domain.NormalizedEvent)
}

func NewMemoryDebouncer(duration time.Duration, onFlushFunc func(ctx context.Context, event domain.NormalizedEvent)) *MemoryDebouncer {
	return &MemoryDebouncer{
		duration:    duration,
		onFlush:     onFlushFunc,
		timers:      make(map[string]*time.Timer),
		accumulater: make(map[string]domain.NormalizedEvent),
	}
}

func (d *MemoryDebouncer) Push(ctx context.Context, event domain.NormalizedEvent) {
	d.mu.Lock()

	defer d.mu.Unlock()

	key := event.DebounceKey()
	if existing, exists := d.accumulater[key]; exists {
		event.Content = existing.Content + "n" + event.Content
	}
	d.accumulater[key] = event
	if t, exists := d.timers[key]; exists {
		t.Stop()
	}
	d.timers[key] = time.AfterFunc(d.duration, func() {
		d.flushKey(ctx, key)
	})
}

func (d *MemoryDebouncer) flushKey(ctx context.Context, key string) {
	d.mu.Lock()
	// tim voi key do thi Event don dc cai gi roi lay ra
	event, exist := d.accumulater[key]
	if !exist {
		defer d.mu.Unlock()
		return
	}
	// clear trong debounce
	delete(d.accumulater, key)
	delete(d.timers, key)
	d.mu.Unlock()
	// goi callback
	if d.onFlush != nil {
		d.onFlush(ctx, event)
	}
}
