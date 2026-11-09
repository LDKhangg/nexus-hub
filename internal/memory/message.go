package memory

import "time"

// SenderRole identifies the author's role in a conversation.
type SenderRole string

const (
	SenderRoleExternalUser SenderRole = "external_user"
	SenderRoleHumanOwner   SenderRole = "human_owner"
	SenderRoleAIRouter     SenderRole = "ai_router"
	SenderRoleAIActor      SenderRole = "ai_actor"
	SenderRoleSystem       SenderRole = "system"
)

// Message preserves one original message, independently of debounce batches.
type Message struct {
	// ID is assigned by storage; it does not guarantee concurrent commit order.
	ID       int64
	ThreadID string
	// ExternalMessageID is a source message ID, not a webhook delivery ID.
	// Nil represents a message without a source identity, such as a local message.
	ExternalMessageID *string
	SenderID          *string
	SenderName        string
	SenderRole        SenderRole
	Content           string
	// OccurredAt is the source timestamp; CreatedAt is the ingestion timestamp.
	OccurredAt time.Time
	CreatedAt  time.Time
	Metadata   map[string]any
}
