package relay

import (
	"context"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

type Ingestor struct {
	manager *DebounceManage
}

func NewIngestor(debounceManage *DebounceManage) *Ingestor {
	return &Ingestor{
		manager: debounceManage,
	}
}

func (i *Ingestor) Push(ctx context.Context, event domain.NormalizedEvent) {
	drop, _ := shouldDrop(event)
	if drop {
		return
	}
	debouncer := i.manager.GetOrCreate(event.Source)
	debouncer.Push(ctx, event)
}
