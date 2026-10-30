package domain

import "time"

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
