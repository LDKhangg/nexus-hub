package tenant

import (
	"errors"
	"time"
)

// Workspace is the tenant boundary for integrations, threads and their data.
type Workspace struct {
	// ID is an application-generated UUID v7 in canonical string form.
	ID          string
	DisplayName string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

var ErrWorkspaceNotFound = errors.New(
	"workspace not found",
)
