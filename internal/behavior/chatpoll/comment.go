package chatpoll

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/sources/clickupchat"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/people"
)

// Comment and member reader defaults.
const (
	// DefaultCommentPages is how many pages of a task's comments (25 per
	// page, newest first) the CommentReader reads to find one comment.
	DefaultCommentPages = 5
	// DefaultMemberTTL is how long the MemberDirectory keeps a
	// workspace's member names.
	DefaultMemberTTL = 10 * time.Minute
	// DefaultMemberTeams caps the workspaces the MemberDirectory keeps.
	DefaultMemberTeams = 64
	// MaxMembersPerTeam caps the member names kept for one workspace.
	MaxMembersPerTeam = 10000
)

// CommentAPI is the ClickUp API v2 comment read (clickupchat.HTTPClient).
type CommentAPI interface {
	TaskComments(ctx context.Context, token, taskID string, startMS int64, startID string) ([]clickupchat.Comment, error)
}

// TeamAPI is the ClickUp API v2 workspace listing (clickupchat.HTTPClient).
type TeamAPI interface {
	Teams(ctx context.Context, token string) ([]clickupchat.Team, error)
}

// CommentReader implements behavior.CommentReader: it reads one comment of
// a task for a behavior run's model step. ClickUp v2 has no endpoint that
// returns one comment by ID, so it reads the task's comment pages (newest
// first) and picks the comment by ID. A run reads its comment soon after
// the webhook, so the first page almost always holds it. The text is
// returned to the run executor only; it is never logged or put in an
// error.
type CommentReader struct {
	API     CommentAPI
	Secrets Secrets
	People  people.Resolver
	// Pages caps the comment pages read; 0 means DefaultCommentPages.
	Pages int
}

var _ behavior.CommentReader = (*CommentReader)(nil)

// TaskComment implements behavior.CommentReader.
func (r *CommentReader) TaskComment(ctx context.Context, workspaceID, tokenRef, taskID, commentID string) (behavior.TaskComment, error) {
	if r == nil || r.API == nil || r.Secrets == nil {
		return behavior.TaskComment{}, &behavior.ChatReadError{Code: behavior.NoteNotImplemented}
	}
	token, err := resolveToken(ctx, r.Secrets, workspaceID, tokenRef)
	if err != nil {
		return behavior.TaskComment{}, err
	}
	pages := r.Pages
	if pages <= 0 {
		pages = DefaultCommentPages
	}
	var startMS int64
	startID := ""
	for page := 0; page < pages; page++ {
		list, err := r.API.TaskComments(ctx, token, taskID, startMS, startID)
		if err != nil {
			if ctx.Err() != nil {
				return behavior.TaskComment{}, ctx.Err()
			}
			return behavior.TaskComment{}, readError(err)
		}
		for _, c := range list {
			if c.ID == commentID {
				if r.People != nil && c.UserID != "" {
					_ = r.People.Sighting(ctx, people.Sighting{WorkspaceID: workspaceID, Platform: "clickup", ExternalID: c.UserID, Username: c.Username, Source: "auto"})
				}
				return behavior.TaskComment{Text: c.Text, SenderID: c.UserID, SenderName: c.Username}, nil
			}
		}
		if len(list) == 0 {
			break
		}
		last := list[len(list)-1]
		if last.ID == "" || last.DateMS <= 0 || (last.ID == startID && last.DateMS == startMS) {
			break
		}
		startMS, startID = last.DateMS, last.ID
	}
	return behavior.TaskComment{}, &behavior.ChatReadError{Code: clickupchat.CodeCommentNotFound}
}

// MemberDirectory implements behavior.MemberDirectory over the ClickUp
// workspace listing (GET /api/v2/team, which carries every member). It
// keeps each workspace's names in memory for TTL, per Nexus workspace and
// ClickUp workspace, so one tenant's token never answers for another's.
// At most MaxTeams workspaces are kept; the oldest goes first. A failed
// listing is not kept: the caller goes on without a name. Names are never
// logged.
type MemberDirectory struct {
	API     TeamAPI
	Secrets Secrets
	People  people.Resolver
	// TTL is how long names are kept; 0 means DefaultMemberTTL.
	TTL time.Duration
	// MaxTeams caps the kept workspaces; 0 means DefaultMemberTeams.
	MaxTeams int
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu    sync.Mutex
	teams map[memberKey]memberEntry
}

type memberKey struct{ workspaceID, teamID string }

type memberEntry struct {
	at    time.Time
	names map[string]string // user ID -> username
}

var _ behavior.MemberDirectory = (*MemberDirectory)(nil)

// MemberName implements behavior.MemberDirectory.
func (d *MemberDirectory) MemberName(ctx context.Context, workspaceID, tokenRef, teamID, userID string) (string, bool) {
	if d == nil || d.API == nil || d.Secrets == nil || teamID == "" || userID == "" {
		return "", false
	}
	now := d.now()
	key := memberKey{workspaceID, teamID}
	d.mu.Lock()
	e, ok := d.teams[key]
	d.mu.Unlock()
	if ok && now.Sub(e.at) < d.ttl() {
		name, found := e.names[userID]
		return name, found && name != ""
	}
	token, err := resolveToken(ctx, d.Secrets, workspaceID, tokenRef)
	if err != nil {
		return "", false
	}
	teams, err := d.API.Teams(ctx, token)
	if err != nil {
		return "", false
	}
	found := map[memberKey]memberEntry{}
	for _, t := range teams {
		if t.ID == "" {
			continue
		}
		names := map[string]string{}
		for _, m := range t.Members {
			if len(names) == MaxMembersPerTeam {
				break
			}
			if m.UserID != "" {
				if d.People != nil {
					_ = d.People.Sighting(ctx, people.Sighting{WorkspaceID: workspaceID, Platform: "clickup", ExternalID: m.UserID, Username: m.Username, Source: "auto"})
				}
				if m.Username != "" {
					names[m.UserID] = m.Username
				}
			}
		}
		found[memberKey{workspaceID, t.ID}] = memberEntry{at: now, names: names}
	}
	if _, ok := found[key]; !ok {
		// The token does not see this workspace: remember that too.
		found[key] = memberEntry{at: now, names: map[string]string{}}
	}
	d.store(found)
	name, ok := found[key].names[userID]
	return name, ok && name != ""
}

// store keeps the entries, dropping expired ones and then the oldest
// beyond MaxTeams.
func (d *MemberDirectory) store(entries map[memberKey]memberEntry) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.teams == nil {
		d.teams = map[memberKey]memberEntry{}
	}
	now, ttl := d.now(), d.ttl()
	for k, e := range d.teams {
		if now.Sub(e.at) >= ttl {
			delete(d.teams, k)
		}
	}
	for k, e := range entries {
		d.teams[k] = e
	}
	limit := d.MaxTeams
	if limit <= 0 {
		limit = DefaultMemberTeams
	}
	for len(d.teams) > limit {
		var oldest memberKey
		var at time.Time
		first := true
		for k, e := range d.teams {
			if first || e.at.Before(at) {
				oldest, at, first = k, e.at, false
			}
		}
		delete(d.teams, oldest)
	}
}

func (d *MemberDirectory) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *MemberDirectory) ttl() time.Duration {
	if d.TTL > 0 {
		return d.TTL
	}
	return DefaultMemberTTL
}

// resolveToken resolves the run's ClickUp credential reference; a failure
// is a *behavior.ChatReadError (missing_credentials) or the context's
// error.
func resolveToken(ctx context.Context, s Secrets, workspaceID, tokenRef string) (string, error) {
	if tokenRef == "" {
		return "", &behavior.ChatReadError{Code: clickupchat.CodeMissingCredentials}
	}
	token, err := s.Resolve(ctx, workspaceID, tokenRef)
	if err != nil || token == "" {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", &behavior.ChatReadError{Code: clickupchat.CodeMissingCredentials}
	}
	return token, nil
}

// readError maps a ClickUp client error to a safe *behavior.ChatReadError.
func readError(err error) error {
	var ce *clickupchat.Error
	if errors.As(err, &ce) {
		return &behavior.ChatReadError{Code: ce.Code, Transient: ce.Retryable}
	}
	return &behavior.ChatReadError{Code: clickupchat.CodeUnavailable, Transient: true}
}
