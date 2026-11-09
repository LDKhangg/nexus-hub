package tenant

import "time"

// IntegrationRole describes how a connected platform participates in the hub.
type IntegrationRole string

const (
	IntegrationRoleSource  IntegrationRole = "source"
	IntegrationRoleControl IntegrationRole = "control"
	IntegrationRoleBoth    IntegrationRole = "both"
)

// Integration connects a workspace to an external platform.
// Encrypted credentials belong to storage, not to this general-purpose model.
type Integration struct {
	// ID and WorkspaceID are UUID v7 strings.
	ID          string
	WorkspaceID string
	// Platform is adapter-defined and is not restricted to a fixed enum.
	Platform  string
	Role      IntegrationRole
	Config    map[string]any
	IsEnabled bool
	CreatedAt time.Time
	UpdatedAt time.Time
}
