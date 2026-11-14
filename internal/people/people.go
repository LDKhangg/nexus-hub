// Package people defines the workspace-scoped people directory contract.
package people

import (
	"context"
	"strings"
	"time"
	"unicode"
)

// MaxDisplayNameRunes caps a display name used in model or report output.
const MaxDisplayNameRunes = 100

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

// Account is one platform identity attached to a contact.
type Account struct {
	ID           string `json:"id"`
	Platform     string `json:"platform"`
	ExternalID   string `json:"external_id"`
	PlatformName string `json:"platform_name,omitempty"`
	Username     string `json:"username,omitempty"`
}

// Contact is one person and all platform identities currently joined to it.
type Contact struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Alias     string    `json:"alias,omitempty"`
	Accounts  []Account `json:"accounts,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Directory is the writable people-directory contract used by owner tools.
type Directory interface {
	Resolver
	List(ctx context.Context, workspaceID, platform, search string, limit int) ([]Contact, error)
	Show(ctx context.Context, workspaceID, contactID string) (Contact, error)
	SetAlias(ctx context.Context, workspaceID, ref, name string) (Contact, error)
	Merge(ctx context.Context, workspaceID, sourceID, targetID string) (Contact, error)
	Split(ctx context.Context, workspaceID, accountID string) (Contact, error)
	Forget(ctx context.Context, workspaceID, contactID string) error
	HasAlias(ctx context.Context, workspaceID, platform, externalID string) (bool, error)
}

// CleanDisplayName makes an untrusted platform label safe for one model or
// report line. It removes control and invisible format characters, collapses
// whitespace, neutralizes @ mentions and caps the result by rune count.
func CleanDisplayName(name string) string {
	var b strings.Builder
	n := 0
	space := false
	for _, r := range name {
		if n >= MaxDisplayNameRunes {
			break
		}
		if unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == unicode.ReplacementChar {
			continue
		}
		if space {
			if n+1 >= MaxDisplayNameRunes {
				break
			}
			b.WriteByte(' ')
			n++
			space = false
		}
		if r == '@' {
			r = '＠'
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
