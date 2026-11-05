package domain

import (
	"errors"
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
	EventMentioned     EventType = "mentioned"
	EventPRUpdated     EventType = "pr_updated"
)

// ErrIgnoredEvent được parser trả về khi webhook hợp lệ nhưng không cần xử lý
// (ví dụ: event không chứa comment). Handler sẽ trả 200 và không queue.
var ErrIgnoredEvent = errors.New("event ignored")

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
	return e.WorkspaceID + ":" + string(e.Source) + ":" + e.ExternalID
}
