package relay

import (
	"sync"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// DebounceManage giữ một debouncer cho mỗi source, để sau này mỗi source
// có thể dùng window riêng.
type DebounceManage struct {
	mu       sync.Mutex
	registry map[domain.ChannelSource]EventDebounce
	duration time.Duration
	maxWait  time.Duration
	onFlush  FlushFunc
}

func NewDebounceManage(duration, maxWait time.Duration, onFlush FlushFunc) *DebounceManage {
	return &DebounceManage{
		duration: duration,
		maxWait:  maxWait,
		onFlush:  onFlush,
		registry: make(map[domain.ChannelSource]EventDebounce),
	}
}

func (m *DebounceManage) GetOrCreate(channel domain.ChannelSource) EventDebounce {
	m.mu.Lock()
	defer m.mu.Unlock()

	if d, ok := m.registry[channel]; ok {
		return d
	}
	d := NewMemoryDebouncer(m.duration, m.maxWait, m.onFlush)
	m.registry[channel] = d
	return d
}

func (m *DebounceManage) FlushAll() {
	m.mu.Lock()
	all := make([]EventDebounce, 0, len(m.registry))
	for _, d := range m.registry {
		all = append(all, d)
	}
	m.mu.Unlock()

	for _, d := range all {
		d.FlushAll()
	}
}
