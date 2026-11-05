package relay

import (
	"context"
	"sync"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

type FlushFunc func(ctx context.Context, event domain.NormalizedEvent)

// EventDebounce là interface để phase 2 thay bằng Redis.
type EventDebounce interface {
	Push(ctx context.Context, event domain.NormalizedEvent)
	// FlushAll flush ngay mọi event đang chờ (dùng khi shutdown).
	FlushAll()
}

type pending struct {
	ctx       context.Context
	event     domain.NormalizedEvent
	gen       uint64
	firstSeen time.Time
	timer     *time.Timer
}

type MemoryDebouncer struct {
	duration time.Duration
	maxWait  time.Duration // 0 = không giới hạn
	onFlush  FlushFunc

	mu      sync.Mutex
	pending map[string]*pending
}

func NewMemoryDebouncer(duration, maxWait time.Duration, onFlush FlushFunc) *MemoryDebouncer {
	return &MemoryDebouncer{
		duration: duration,
		maxWait:  maxWait,
		onFlush:  onFlush,
		pending:  make(map[string]*pending),
	}
}

func (d *MemoryDebouncer) Push(ctx context.Context, event domain.NormalizedEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()

	key := event.DebounceKey()
	now := time.Now()

	p, ok := d.pending[key]
	if ok {
		p.timer.Stop()
		if event.Content != "" && p.event.Content != "" {
			event.Content = p.event.Content + "\n" + event.Content
		} else if event.Content == "" {
			event.Content = p.event.Content
		}
	} else {
		p = &pending{firstSeen: now}
		d.pending[key] = p
	}
	p.ctx = ctx
	p.event = event
	// Mỗi lần Push tăng gen; timer cũ (nếu đã fire và đang chờ lock) sẽ thấy gen lệch và bỏ qua.
	p.gen++
	gen := p.gen

	delay := d.duration
	if d.maxWait > 0 {
		if remaining := p.firstSeen.Add(d.maxWait).Sub(now); remaining < delay {
			delay = max(remaining, 0)
		}
	}
	p.timer = time.AfterFunc(delay, func() { d.flushKey(key, gen) })
}

func (d *MemoryDebouncer) flushKey(key string, gen uint64) {
	d.mu.Lock()
	p, ok := d.pending[key]
	if !ok || p.gen != gen {
		d.mu.Unlock()
		return
	}
	delete(d.pending, key)
	d.mu.Unlock()

	if d.onFlush != nil {
		d.onFlush(p.ctx, p.event)
	}
}

func (d *MemoryDebouncer) FlushAll() {
	d.mu.Lock()
	all := d.pending
	d.pending = make(map[string]*pending)
	for _, p := range all {
		p.timer.Stop()
	}
	d.mu.Unlock()

	if d.onFlush == nil {
		return
	}
	for _, p := range all {
		d.onFlush(p.ctx, p.event)
	}
}
