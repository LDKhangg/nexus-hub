package relay

import (
	"context"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"testing"
)

type noticeAcceptFunc func(context.Context, []domain.NormalizedEvent) (AcceptResult, error)

func (f noticeAcceptFunc) Accept(ctx context.Context, events []domain.NormalizedEvent) (AcceptResult, error) {
	return f(ctx, events)
}
func TestNoticeServiceOwnBotFilter(t *testing.T) {
	store := noticeAcceptFunc(func(_ context.Context, events []domain.NormalizedEvent) (AcceptResult, error) {
		if len(events) != 1 || events[0].Content != "human" {
			t.Fatalf("bad originals: %+v", events)
		}
		return AcceptResult{Accepted: 1}, nil
	})
	service := NewNoticeService(store)
	result, err := service.Accept(context.Background(), []domain.NormalizedEvent{{Content: "bot", FromOwnBot: true}, {Content: "human"}})
	if err != nil || result.Accepted != 1 || result.Ignored != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	result, err = service.Accept(context.Background(), []domain.NormalizedEvent{{FromOwnBot: true}})
	if err != nil || result.Ignored != 1 {
		t.Fatalf("%+v %v", result, err)
	}
}
