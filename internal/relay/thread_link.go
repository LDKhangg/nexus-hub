package relay

import "time"

// ThreadLink records an explicit, symmetric relationship within one workspace.
// It neither grants access nor automatically includes another thread in context.
type ThreadLink struct {
	// All IDs are UUID v7 strings. Storage must keep the smaller thread UUID first.
	WorkspaceID    string
	SourceThreadID string
	TargetThreadID string
	// Metadata may record the confirmed URL or human decision behind the link.
	Metadata  map[string]any
	CreatedAt time.Time
}
