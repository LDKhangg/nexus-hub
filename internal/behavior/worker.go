package behavior

import (
	"context"
	"time"
)

// WorkerOnlineWindow is how long after its last contact a device still counts
// as online.
const WorkerOnlineWindow = 90 * time.Second

// WorkerEnrollCodeTTL is how long a one-time enrollment code stays valid.
const WorkerEnrollCodeTTL = 10 * time.Minute

// WorkerDevice is an enrolled local worker. The token is never part of it.
type WorkerDevice struct {
	ID           string
	WorkspaceID  string
	Name         string
	Capabilities []string
	State        string
	LastSeenAt   *time.Time
	CreatedAt    time.Time
	RevokedAt    *time.Time
}

// DisplayState is "revoked", "online" or "offline" at the given time.
func (d WorkerDevice) DisplayState(now time.Time) string {
	switch {
	case d.RevokedAt != nil:
		return "revoked"
	case d.LastSeenAt != nil && now.Sub(*d.LastSeenAt) <= WorkerOnlineWindow:
		if d.State == "busy" {
			return "busy"
		}
		return "online"
	}
	return "offline"
}

// WorkerAdmin is the owner's side of worker management.
type WorkerAdmin interface {
	// CreateEnrollCode returns a one-time code (shown once) and its expiry.
	CreateEnrollCode(ctx context.Context, workspaceID, createdBy string) (string, time.Time, error)
	ListDevices(ctx context.Context, workspaceID string) ([]WorkerDevice, error)
	// RevokeDevice revokes an active device by name; ErrNotFound if none.
	RevokeDevice(ctx context.Context, workspaceID, name string) error
}

// WorkerEnroller redeems an enrollment code. The plaintext token is returned
// once. A bad, used or expired code is ErrNotFound.
type WorkerEnroller interface {
	Enroll(ctx context.Context, code, name string, capabilities []string) (WorkerDevice, string, error)
}

// WorkerAuthenticator maps a device token to its active device and records
// contact. Unknown or revoked tokens are ErrNotFound.
type WorkerAuthenticator interface {
	Authenticate(ctx context.Context, token string) (WorkerDevice, error)
}
