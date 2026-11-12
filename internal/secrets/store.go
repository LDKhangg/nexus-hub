package secrets

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by a Store for an unknown credential name.
var ErrNotFound = errors.New("credential not found")

// Meta is non-secret credential metadata.
type Meta struct {
	Name, Kind string
	UpdatedAt  time.Time
}

// Store persists encrypted credentials (implemented by storage.CredentialStore).
type Store interface {
	Put(ctx context.Context, workspaceID, name, kind string, nonce, ct []byte) error
	Get(ctx context.Context, workspaceID, name string) (kind string, nonce, ct []byte, err error)
	List(ctx context.Context, workspaceID string) ([]Meta, error)
	Delete(ctx context.Context, workspaceID, name string) error
}

// Kinds are the allowed credential kinds.
var Kinds = map[string]bool{
	"telegram_bot": true, "clickup_token": true, "clickup_webhook": true,
	"github_pat": true, "github_app_key": true, "anthropic_key": true,
}
