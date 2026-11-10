package relay

import (
	"context"
	"github.com/LDKhangg/nexus-hub/internal/domain"
)

type NoticeService struct{ store NoticeAcceptor }

func NewNoticeService(store NoticeAcceptor) *NoticeService { return &NoticeService{store: store} }
func (s *NoticeService) Accept(ctx context.Context, events []domain.NormalizedEvent) (AcceptResult, error) {
	filtered := make([]domain.NormalizedEvent, 0, len(events))
	ignored := 0
	for _, event := range events {
		if drop, _ := shouldDrop(event); drop {
			ignored++
			continue
		}
		filtered = append(filtered, event)
	}
	if len(filtered) == 0 {
		return AcceptResult{Ignored: ignored}, nil
	}
	result, err := s.store.Accept(ctx, filtered)
	if err != nil {
		return AcceptResult{}, err
	}
	result.Ignored += ignored
	return result, nil
}
