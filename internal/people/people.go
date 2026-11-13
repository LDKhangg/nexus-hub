// Package people defines the workspace-scoped people directory contract.
package people

import (
	"context"
	"time"
)

// Sighting is one observation of a platform account. Names are untrusted
// labels; ExternalID is the stable platform identifier used for resolution.
type Sighting struct {
	WorkspaceID  string
	Platform     string
	ExternalID   string
	PlatformName string
	Username     string
	Source       string
	IsSelf       bool
	SeenAt       time.Time
}

// Resolver records account sightings and resolves their current display name.
type Resolver interface {
	// DisplayName returns the best current name for a workspace account.
	DisplayName(ctx context.Context, workspaceID, platform, externalID string) (name string, ok bool)
	// Sighting records the latest observed labels for a platform account.
	Sighting(ctx context.Context, sighting Sighting) error
}
