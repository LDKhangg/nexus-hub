package relay

import (
	"context"
	"sync"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

type DebounceManage struct {
	registry map[domain.ChannelSource]EventDebounce
	mu       sync.Mutex
	duration time.Duration
	onFlush  func(ctx context.Context, event domain.NormalizedEvent)
}

func NewDebounceManage(duration time.Duration, fc func(ctx context.Context, event domain.NormalizedEvent)) *DebounceManage {
	return &DebounceManage{
		duration: duration,
		onFlush:  fc,
		registry: make(map[domain.ChannelSource]EventDebounce),
	}
}

func (m *DebounceManage) GetOrCreate(Channel domain.ChannelSource) EventDebounce {
	m.mu.Lock()
	defer m.mu.Unlock()

	if debounce, exists := m.registry[Channel]; exists {
		return debounce
	}
	debounce := NewMemoryDebouncer(m.duration, m.onFlush)
	m.registry[Channel] = debounce
	return debounce
}
