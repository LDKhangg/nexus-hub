package relay

import "time"

// Thread is an independent conversation within one workspace and integration.
// Linking it to another thread does not merge their histories or grant access.
type Thread struct {
	// ID, WorkspaceID and IntegrationID are UUID v7 strings.
	ID            string
	WorkspaceID   string
	IntegrationID string
	// ExternalScope disambiguates IDs within a source, e.g. repository or channel.
	ExternalScope string
	// ThreadType is adapter-defined, e.g. task, issue or conversation.
	ThreadType     string
	ExternalID     string
	Title          string
	RollingSummary string
	// Nil means no message has been covered by RollingSummary yet.
	// Summary and cursor must be updated atomically with per-thread serialization.
	SummaryThroughMessageID *int64
	Metadata                map[string]any
	CreatedAt               time.Time
	UpdatedAt               time.Time
}
