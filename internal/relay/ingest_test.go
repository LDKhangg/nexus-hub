package relay

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// Nếu Ingestor quên chuyển tiếp event, callback sẽ không nhận được comment.
func TestIngestorForwardsNormalEvent(t *testing.T) {
	// Arrange: dùng manager và debouncer thật; recorder ghi lại event được flush.
	rec := &recorder{}
	manager := NewDebounceManage(time.Hour, 0, rec.flush)
	t.Cleanup(manager.FlushAll)
	ingestor := NewIngestor(manager)
	event := domain.NormalizedEvent{
		WorkspaceID: "workspace-1",
		Source:      domain.SourceClickup,
		ExternalID:  "task-1",
		SenderID:    "user-1",
		Content:     "Please review this task",
	}

	// Act: đi qua ingest rồi flush ngay, không cần chờ timer.
	ingestor.Push(context.Background(), event)
	manager.FlushAll()

	// Assert: comment đi đến callback đúng một lần và không bị thay đổi.
	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 flushed event, got %d: %+v", len(got), got)
	}
	if !reflect.DeepEqual(got[0], event) {
		t.Errorf("flushed event = %+v, want %+v", got[0], event)
	}
}

// Nếu Ingestor bỏ qua filter, comment của bot sẽ lọt tới callback.
func TestIngestorDropsOwnBotEvent(t *testing.T) {
	rec := &recorder{}
	manager := NewDebounceManage(time.Hour, 0, rec.flush)
	t.Cleanup(manager.FlushAll)
	ingestor := NewIngestor(manager)
	event := domain.NormalizedEvent{
		WorkspaceID: "workspace-1",
		Source:      domain.SourceClickup,
		ExternalID:  "task-1",
		SenderID:    "bot-1",
		Content:     "🤖 Already handled",
		FromOwnBot:  true,
	}

	ingestor.Push(context.Background(), event)
	manager.FlushAll()

	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("own bot event should be dropped, got flushed events: %+v", got)
	}
}
