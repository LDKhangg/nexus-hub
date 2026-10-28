package relay

import (
	"context"
	"sync"
	"time"
)

type DebounceManage struct {
	registry map[ChannelSource]EventDebounce
	mu       sync.Mutex
	duration time.Duration
	onFlush  func(ctx context.Context, event NormalizedEvent)
}

func NewDebounceManage(duration time.Duration, fc func(ctx context.Context, event NormalizedEvent)) *DebounceManage {
	return &DebounceManage{
		duration: duration,
		onFlush:  fc,
		registry: make(map[ChannelSource]EventDebounce),
	}
}

func (m *DebounceManage) GetOrCreate(Channel ChannelSource) EventDebounce {
	m.mu.Lock()
	defer m.mu.Unlock()

	if debounce, exists := m.registry[Channel]; exists {
		return debounce
	}
	debounce := NewMemoryDebouncer(m.duration, m.onFlush)
	m.registry[Channel] = debounce
	return debounce
}
