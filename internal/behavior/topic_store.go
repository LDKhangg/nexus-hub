package behavior

import (
	"context"
	"time"
)

// Topic subject kinds. A subject key is an identifier (for example a ClickUp
// task id or a fixed topic name from behavior YAML), never message text.
const (
	TopicSubjectClickUpTask = "clickup_task"
	TopicSubjectWorkflow    = "workflow"
	TopicSubjectFixed       = "fixed"
)

// Topic states.
const (
	TopicOpen   = "open"
	TopicClosed = "closed"
)

// TopicReservationLease is how long a reservation without a thread id stays
// owned by its creator before another caller may take it over.
const TopicReservationLease = 60 * time.Second

// TopicKey identifies one topic subject inside one chat of one workspace.
type TopicKey struct {
	WorkspaceID   string
	IntegrationID string
	ChatID        int64
	SubjectKind   string
	SubjectKey    string
}

// Topic is a stored binding. ThreadID is 0 while only reserved. Topic and
// task names are never stored.
type Topic struct {
	ID            int64
	WorkspaceID   string
	IntegrationID string
	ChatID        int64
	ThreadID      int64
	SubjectKind   string
	SubjectKey    string
	State         string
	ReservedAt    time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ClosedAt      *time.Time
}

// TopicStore persists topic bindings; every call is scoped by workspace.
type TopicStore interface {
	// EnsureTopic returns the subject's topic. claimed is true only for the
	// caller that must create the Telegram topic: it holds a reservation
	// (no thread id yet) and must finish with SetTopicThread, or give up
	// with ReleaseTopic. Concurrent callers get claimed=false, so one subject
	// never gets two Telegram topics. A reservation that is still unset after
	// TopicReservationLease can be taken over by the next caller.
	EnsureTopic(ctx context.Context, key TopicKey) (topic Topic, claimed bool, err error)
	// SetTopicThread records the created thread id once, for the holder whose
	// reservation (Topic.ReservedAt as EnsureTopic returned it) is still
	// current; ErrConflict when a stale holder calls it, when
	// the topic already has one or does not exist in the workspace.
	SetTopicThread(ctx context.Context, workspaceID string, id, threadID int64, reservedAt time.Time) (Topic, error)
	// ReleaseTopic drops a reservation that has no thread id yet.
	ReleaseTopic(ctx context.Context, workspaceID string, id int64) error
	TopicBySubject(ctx context.Context, key TopicKey) (Topic, error)
	TopicByThread(ctx context.Context, workspaceID, integrationID string, chatID, threadID int64) (Topic, error)
	// SetTopicState sets open or closed; ErrNotFound when missing.
	SetTopicState(ctx context.Context, workspaceID string, id int64, state string) (Topic, error)
	ListTopics(ctx context.Context, workspaceID string) ([]Topic, error)
}
