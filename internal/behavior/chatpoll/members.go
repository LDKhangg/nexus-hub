package chatpoll

import (
	"context"
	"sync"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/sources/clickupchat"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/people"
)

// Channel member directory defaults.
const (
	// DefaultChannelMemberPages caps the member pages one lookup reads.
	DefaultChannelMemberPages = 5
	// ChannelMemberPageSize is the member page size asked of ClickUp.
	ChannelMemberPageSize = 100
	// DefaultMemberChannels caps the channels the ChannelMemberDirectory
	// keeps.
	DefaultMemberChannels = 256
	// MaxMembersPerChannel caps the member names kept for one channel.
	MaxMembersPerChannel = 10000
)

// ChannelMembersAPI is the ClickUp Chat v3 channel member listing
// (clickupchat.HTTPClient).
type ChannelMembersAPI interface {
	ChannelMembers(ctx context.Context, ws, token, channelID, cursor string, limit int) (clickupchat.Page[clickupchat.ChannelMember], error)
}

// ChannelMemberDirectory implements behavior.ChannelMemberDirectory over
// the channel member listing (GET
// /api/v3/workspaces/{ws}/chat/channels/{id}/members). It keeps the names
// read for each channel in memory for TTL, per Nexus workspace, ClickUp
// workspace and channel, so one tenant's token never answers for
// another's.
//
// A lookup that misses the cache reads member pages (ChannelMemberPageSize
// each) from where the channel's last read stopped, at most Pages pages,
// and stops as soon as the user is found. A channel whose listing has
// been read to the end (or to MaxMembersPerChannel names) answers misses
// from the cache until it expires. At most MaxChannels channels are kept;
// expired ones go first, then the oldest. A failed read keeps the names
// read before it; the caller goes on without a name. Names are never
// logged.
type ChannelMemberDirectory struct {
	API     ChannelMembersAPI
	Secrets Secrets
	People  people.Resolver
	// TTL is how long names are kept; 0 means DefaultMemberTTL.
	TTL time.Duration
	// Pages caps the pages one lookup reads; 0 means
	// DefaultChannelMemberPages.
	Pages int
	// MaxChannels caps the kept channels; 0 means DefaultMemberChannels.
	MaxChannels int
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu       sync.Mutex
	channels map[channelKey]*channelEntry
}

type channelKey struct{ workspaceID, teamID, channelID string }

type channelEntry struct {
	at       time.Time
	names    map[string]string // user ID -> cleaned display name
	cursor   string            // where the next page starts
	complete bool              // the listing was read to the end or to the cap
}

var _ behavior.ChannelMemberDirectory = (*ChannelMemberDirectory)(nil)

// ChannelMemberName implements behavior.ChannelMemberDirectory.
func (d *ChannelMemberDirectory) ChannelMemberName(ctx context.Context, workspaceID, tokenRef, teamID, channelID, userID string) (string, bool) {
	if d == nil || d.API == nil || d.Secrets == nil || teamID == "" || channelID == "" || userID == "" {
		return "", false
	}
	now := d.now()
	key := channelKey{workspaceID, teamID, channelID}

	d.mu.Lock()
	e, ok := d.channels[key]
	if ok && now.Sub(e.at) >= d.ttl() {
		delete(d.channels, key)
		ok = false
	}
	start := ""
	if ok {
		if name, found := e.names[userID]; found {
			d.mu.Unlock()
			return name, name != ""
		}
		if e.complete {
			d.mu.Unlock()
			return "", false
		}
		start = e.cursor
	}
	d.mu.Unlock()

	token, err := resolveToken(ctx, d.Secrets, workspaceID, tokenRef)
	if err != nil {
		return "", false
	}
	pages := d.Pages
	if pages <= 0 {
		pages = DefaultChannelMemberPages
	}
	names := map[string]string{}
	cursor, complete := start, false
	for page := 0; page < pages; page++ {
		p, err := d.API.ChannelMembers(ctx, teamID, token, channelID, cursor, ChannelMemberPageSize)
		if err != nil {
			break
		}
		for _, m := range p.Items {
			if m.ID == "" {
				continue
			}
			if d.People != nil {
				_ = d.People.Sighting(ctx, people.Sighting{WorkspaceID: workspaceID, Platform: "clickup", ExternalID: m.ID, PlatformName: m.Name, Username: m.Username, Source: "auto"})
			}
			name := behavior.CleanDisplayName(m.Name)
			if name == "" {
				name = behavior.CleanDisplayName(m.Username)
			}
			if name != "" {
				names[m.ID] = name
			}
		}
		if p.NextCursor == "" || p.NextCursor == cursor || len(p.Items) == 0 {
			complete = true
			break
		}
		cursor = p.NextCursor
		if _, found := names[userID]; found {
			break
		}
	}
	d.store(key, start, names, cursor, complete, now)
	name, found := names[userID]
	return name, found && name != ""
}

// store merges the names read from start into the channel's entry. The
// entry's cursor moves only when no other lookup moved it meanwhile.
func (d *ChannelMemberDirectory) store(key channelKey, start string, names map[string]string, cursor string, complete bool, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.channels == nil {
		d.channels = map[channelKey]*channelEntry{}
	}
	ttl := d.ttl()
	e, ok := d.channels[key]
	if !ok || now.Sub(e.at) >= ttl {
		if start != "" {
			// The entry this read resumed has expired or gone: its
			// names are partial, so keep them without a cursor that
			// skips the first pages.
			cursor, complete = "", false
		}
		e = &channelEntry{at: now, names: map[string]string{}}
		d.channels[key] = e
	} else if e.cursor != start || e.complete {
		cursor, complete = e.cursor, e.complete
	}
	for id, name := range names {
		if len(e.names) >= MaxMembersPerChannel {
			complete = true
			break
		}
		e.names[id] = name
	}
	if len(e.names) >= MaxMembersPerChannel {
		complete = true
	}
	e.cursor, e.complete = cursor, complete

	for k, c := range d.channels {
		if now.Sub(c.at) >= ttl {
			delete(d.channels, k)
		}
	}
	limit := d.MaxChannels
	if limit <= 0 {
		limit = DefaultMemberChannels
	}
	for len(d.channels) > limit {
		var oldest channelKey
		var at time.Time
		first := true
		for k, c := range d.channels {
			if first || c.at.Before(at) {
				oldest, at, first = k, c.at, false
			}
		}
		delete(d.channels, oldest)
	}
}

func (d *ChannelMemberDirectory) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *ChannelMemberDirectory) ttl() time.Duration {
	if d.TTL > 0 {
		return d.TTL
	}
	return DefaultMemberTTL
}
