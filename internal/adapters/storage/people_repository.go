package storage

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/people"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	peopleCacheTTL          = 5 * time.Minute
	peopleCachePerWorkspace = 256
	peopleCacheWorkspaces   = 64
)

type peopleCacheKey struct{ platform, externalID string }
type peopleCacheValue struct {
	name string
	ok   bool
	at   time.Time
}

// PeopleRepository is the PostgreSQL-backed workspace people directory.
type PeopleRepository struct {
	queries *sqlc.Queries
	now     func() time.Time

	mu    sync.Mutex
	cache map[string]map[peopleCacheKey]peopleCacheValue
}

// NewPeopleRepository constructs a workspace-scoped people directory.
func NewPeopleRepository(pool *pgxpool.Pool) (*PeopleRepository, error) {
	if pool == nil {
		return nil, errors.New("people repository requires pool")
	}
	return &PeopleRepository{queries: sqlc.New(pool), now: time.Now, cache: make(map[string]map[peopleCacheKey]peopleCacheValue)}, nil
}

var _ people.Resolver = (*PeopleRepository)(nil)

// DisplayName resolves an account's manual alias, platform name, username,
// then a stable platform/id fallback. Unknown accounts return ok=false.
func (r *PeopleRepository) DisplayName(ctx context.Context, workspaceID, platform, externalID string) (string, bool) {
	if r == nil || workspaceID == "" || platform == "" || externalID == "" {
		return "", false
	}
	key := peopleCacheKey{platform: platform, externalID: externalID}
	now := r.now()
	if v, ok := r.cached(workspaceID, key, now); ok {
		return v.name, v.ok
	}
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return "", false
	}
	row, err := r.queries.GetContactByAccount(ctx, sqlc.GetContactByAccountParams{WorkspaceID: ws, Platform: platform, ExternalID: externalID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			r.storeCache(workspaceID, key, peopleCacheValue{at: now})
		}
		return "", false
	}
	name := firstCleanName(row.DisplayName, row.PlatformName, row.Username)
	if name == "" {
		name = people.CleanDisplayName(platform + " " + externalID)
	}
	r.storeCache(workspaceID, key, peopleCacheValue{name: name, ok: name != "", at: now})
	return name, name != ""
}

// Sighting records labels observed without making an extra platform call.
func (r *PeopleRepository) Sighting(ctx context.Context, s people.Sighting) error {
	if r == nil {
		return errors.New("people repository is nil")
	}
	if s.WorkspaceID == "" || s.Platform == "" || s.ExternalID == "" || s.Source == "" {
		return errors.New("people sighting requires workspace, platform, external id and source")
	}
	ws, err := parseDBID(s.WorkspaceID)
	if err != nil {
		return fmt.Errorf("people sighting workspace: %w", err)
	}
	seenAt := s.SeenAt
	if seenAt.IsZero() {
		seenAt = r.now()
	}
	platformName := people.CleanDisplayName(s.PlatformName)
	username := people.CleanDisplayName(s.Username)
	existing, getErr := r.queries.GetContactByAccount(ctx, sqlc.GetContactByAccountParams{WorkspaceID: ws, Platform: s.Platform, ExternalID: s.ExternalID})
	if getErr == nil {
		stale := existing.LastSeen.Valid && seenAt.Before(existing.LastSeen.Time)
		if platformName == "" || stale {
			platformName = existing.PlatformName.String
		}
		if username == "" || stale {
			username = existing.Username.String
		}
	} else if !errors.Is(getErr, pgx.ErrNoRows) {
		return fmt.Errorf("read people sighting: %w", getErr)
	}
	accountID, contactID, err := newPeopleIDs()
	if err != nil {
		return err
	}
	_, err = r.queries.UpsertContactAccount(ctx, sqlc.UpsertContactAccountParams{
		AccountID: accountID, WorkspaceID: ws, Platform: s.Platform, ExternalID: s.ExternalID,
		PlatformName: textOrNull(platformName), Username: textOrNull(username), Source: s.Source,
		IsSelf: s.IsSelf, SeenAt: pgtype.Timestamptz{Time: seenAt, Valid: true}, ContactID: contactID,
	})
	if err != nil {
		return fmt.Errorf("record people sighting: %w", err)
	}
	r.mu.Lock()
	if workspace := r.cache[s.WorkspaceID]; workspace != nil {
		delete(workspace, peopleCacheKey{platform: s.Platform, externalID: s.ExternalID})
	}
	r.mu.Unlock()
	return nil
}

func firstCleanName(names ...pgtype.Text) string {
	for _, value := range names {
		if value.Valid {
			if name := people.CleanDisplayName(value.String); name != "" {
				return name
			}
		}
	}
	return ""
}

func textOrNull(value string) pgtype.Text { return pgtype.Text{String: value, Valid: value != ""} }

func newPeopleIDs() (pgtype.UUID, pgtype.UUID, error) {
	account, err := uuid.NewV7()
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("create people account id: %w", err)
	}
	contact, err := uuid.NewV7()
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, fmt.Errorf("create people contact id: %w", err)
	}
	return pgtype.UUID{Bytes: account, Valid: true}, pgtype.UUID{Bytes: contact, Valid: true}, nil
}

func (r *PeopleRepository) cached(workspaceID string, key peopleCacheKey, now time.Time) (peopleCacheValue, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.cache[workspaceID][key]
	if ok && now.Sub(v.at) < peopleCacheTTL {
		return v, true
	}
	if ok {
		delete(r.cache[workspaceID], key)
	}
	return peopleCacheValue{}, false
}

func (r *PeopleRepository) storeCache(workspaceID string, key peopleCacheKey, value peopleCacheValue) {
	r.mu.Lock()
	defer r.mu.Unlock()
	workspace := r.cache[workspaceID]
	if workspace == nil {
		if len(r.cache) >= peopleCacheWorkspaces {
			for id := range r.cache {
				delete(r.cache, id)
				break
			}
		}
		workspace = make(map[peopleCacheKey]peopleCacheValue)
		r.cache[workspaceID] = workspace
	}
	for len(workspace) >= peopleCachePerWorkspace {
		var oldest peopleCacheKey
		var at time.Time
		first := true
		for k, v := range workspace {
			if first || v.at.Before(at) {
				oldest, at, first = k, v.at, false
			}
		}
		delete(workspace, oldest)
	}
	workspace[key] = value
}
