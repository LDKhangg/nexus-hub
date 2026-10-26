package relay

import (
	"context"
	"time"
)

type ChannelSource string

const (
	SourceClickup ChannelSource = "clickup"
	SourceGithub  ChannelSource = "github"
)

type EventType string

const (
	EventCommandPosted EventType = "command_posted"
	EventStatusChanged EventType = "status_changed"
	Eventmentioned     EventType = "mentioned"
	EventPRUpdated     EventType = "pr_updated"
)

type NormalizedEvent struct {
	WorkspaceID   string            `json:"workspace_id"`
	IntegrationID string            `json:"integration_id"`
	Source        ChannelSource     `json:"source"`
	EventType     EventType         `json:"event_type"`
	ExternalID    string            `json:"external_id"`
	Title         string            `json:"title"`
	SenderID      string            `json:"sender_id"`
	SenderName    string            `json:"sender_name"`
	Content       string            `json:"content"`
	Metadata      map[string]string `json:"metadata"`
	OccurredAt    time.Time         `json:"occurred_at"`
}

func (e NormalizedEvent) DebounceKey() string {
	return e.WorkspaceID + ": " + string(e.Source) + ": " + e.ExternalID
}

type SourcePort interface {
	Platform() ChannelSource
	Acknowledge(ctx context.Context, externalID string) error
	PostReply(ctx context.Context, externalID string, message string) error
	UpdateState(ctx context.Context, externalID string, newState string) error
}

type ControlPlanePort interface {
	EnsureThread(ctx context.Context, event NormalizedEvent) (controlThreadID string, err error)
	NotifyHuman(ctx context.Context, controlThreadID string, summary string, drafter string) error
}

type ThreadRepository interface {
	GetControlThreadID(ctx context.Context, workspaceID string, source ChannelSource, externalID string) (string, error)
	SaveMapping(ctx context.Context, workspaceID string, source ChannelSource, externalID string, controlThreadID string) error
}
