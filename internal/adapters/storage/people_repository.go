package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
var _ people.Directory = (*PeopleRepository)(nil)

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

// List returns cleaned contacts in stable display-name order.
func (r *PeopleRepository) List(ctx context.Context, workspaceID, platform, search string, limit int) ([]people.Contact, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list contacts: %w", err)
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := r.queries.ListContacts(ctx, sqlc.ListContactsParams{
		WorkspaceID: ws, Platform: textOrNull(strings.TrimSpace(platform)), Search: textOrNull(strings.TrimSpace(search)), MaxRows: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("list contacts: %w", err)
	}
	contacts := make([]people.Contact, 0, len(rows))
	for _, row := range rows {
		contact, err := r.contact(ctx, ws, row.ID)
		if err != nil {
			return nil, err
		}
		contacts = append(contacts, contact)
	}
	return contacts, nil
}

// Show returns one workspace-scoped contact and its accounts.
func (r *PeopleRepository) Show(ctx context.Context, workspaceID, contactID string) (people.Contact, error) {
	ws, id, err := parseScopedIDs(workspaceID, contactID)
	if err != nil {
		return people.Contact{}, fmt.Errorf("show contact: %w", err)
	}
	return r.contact(ctx, ws, id)
}

// SetAlias cleans and stores a contact alias. Ref may be a contact UUID or
// platform:external-id.
func (r *PeopleRepository) SetAlias(ctx context.Context, workspaceID, ref, name string) (people.Contact, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return people.Contact{}, fmt.Errorf("set contact alias: %w", err)
	}
	cleaned := people.CleanDisplayName(name)
	if cleaned == "" {
		return people.Contact{}, errors.New("contact alias must not be empty")
	}
	id, err := r.contactID(ctx, ws, ref)
	if err != nil {
		return people.Contact{}, err
	}
	row, err := r.queries.SetContactAlias(ctx, sqlc.SetContactAliasParams{DisplayName: textOrNull(cleaned), WorkspaceID: ws, ContactID: id})
	if err != nil {
		return people.Contact{}, fmt.Errorf("set contact alias: %w", err)
	}
	r.invalidate(workspaceID)
	return r.contact(ctx, ws, row.ID)
}

// Merge moves every account from source into target and removes source.
func (r *PeopleRepository) Merge(ctx context.Context, workspaceID, sourceID, targetID string) (people.Contact, error) {
	ws, source, err := parseScopedIDs(workspaceID, sourceID)
	if err != nil {
		return people.Contact{}, fmt.Errorf("merge contacts: %w", err)
	}
	target, err := parseDBID(targetID)
	if err != nil {
		return people.Contact{}, fmt.Errorf("merge contacts: %w", err)
	}
	row, err := r.queries.MergeContacts(ctx, sqlc.MergeContactsParams{WorkspaceID: ws, SourceID: source, TargetID: target})
	if err != nil {
		return people.Contact{}, fmt.Errorf("merge contacts: %w", err)
	}
	r.invalidate(workspaceID)
	return r.contact(ctx, ws, row.ID)
}

// Split moves one account to a new contact.
func (r *PeopleRepository) Split(ctx context.Context, workspaceID, accountID string) (people.Contact, error) {
	ws, account, err := parseScopedIDs(workspaceID, accountID)
	if err != nil {
		return people.Contact{}, fmt.Errorf("split contact: %w", err)
	}
	_, contactID, err := newPeopleIDs()
	if err != nil {
		return people.Contact{}, err
	}
	row, err := r.queries.SplitContactAccount(ctx, sqlc.SplitContactAccountParams{WorkspaceID: ws, AccountID: account, NewContactID: contactID})
	if err != nil {
		return people.Contact{}, fmt.Errorf("split contact: %w", err)
	}
	r.invalidate(workspaceID)
	return r.contact(ctx, ws, row.ContactID)
}

// Forget deletes a contact and all of its accounts.
func (r *PeopleRepository) Forget(ctx context.Context, workspaceID, contactID string) error {
	ws, id, err := parseScopedIDs(workspaceID, contactID)
	if err != nil {
		return fmt.Errorf("forget contact: %w", err)
	}
	if _, err := r.queries.ForgetContact(ctx, sqlc.ForgetContactParams{WorkspaceID: ws, ContactID: id}); err != nil {
		return fmt.Errorf("forget contact: %w", err)
	}
	r.invalidate(workspaceID)
	return nil
}

// HasAlias reports whether an account's contact has an owner-set alias.
func (r *PeopleRepository) HasAlias(ctx context.Context, workspaceID, platform, externalID string) (bool, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return false, fmt.Errorf("read contact alias: %w", err)
	}
	row, err := r.queries.GetContactByAccount(ctx, sqlc.GetContactByAccountParams{WorkspaceID: ws, Platform: platform, ExternalID: externalID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read contact alias: %w", err)
	}
	return people.CleanDisplayName(row.DisplayName.String) != "", nil
}

func (r *PeopleRepository) contactID(ctx context.Context, ws pgtype.UUID, ref string) (pgtype.UUID, error) {
	if id, err := parseDBID(ref); err == nil {
		return id, nil
	}
	platform, externalID, ok := strings.Cut(ref, ":")
	if !ok || strings.TrimSpace(platform) == "" || strings.TrimSpace(externalID) == "" {
		return pgtype.UUID{}, errors.New("contact reference must be a UUID or platform:external-id")
	}
	row, err := r.queries.GetContactByAccount(ctx, sqlc.GetContactByAccountParams{WorkspaceID: ws, Platform: platform, ExternalID: externalID})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("find contact account: %w", err)
	}
	return row.ID, nil
}

func (r *PeopleRepository) contact(ctx context.Context, ws, id pgtype.UUID) (people.Contact, error) {
	row, err := r.queries.GetContact(ctx, sqlc.GetContactParams{WorkspaceID: ws, ContactID: id})
	if err != nil {
		return people.Contact{}, fmt.Errorf("read contact: %w", err)
	}
	accounts, err := r.queries.ListContactAccounts(ctx, sqlc.ListContactAccountsParams{WorkspaceID: ws, ContactID: id})
	if err != nil {
		return people.Contact{}, fmt.Errorf("read contact accounts: %w", err)
	}
	result := people.Contact{ID: dbUUID(row.ID), Alias: people.CleanDisplayName(row.DisplayName.String), UpdatedAt: row.UpdatedAt.Time}
	for _, account := range accounts {
		result.Accounts = append(result.Accounts, people.Account{
			ID: dbUUID(account.ID), Platform: account.Platform, ExternalID: account.ExternalID,
			PlatformName: people.CleanDisplayName(account.PlatformName.String), Username: people.CleanDisplayName(account.Username.String),
		})
	}
	result.Name = result.Alias
	if result.Name == "" && len(accounts) > 0 {
		result.Name = firstCleanName(accounts[0].PlatformName, accounts[0].Username)
		if result.Name == "" {
			result.Name = people.CleanDisplayName(accounts[0].Platform + " " + accounts[0].ExternalID)
		}
	}
	return result, nil
}

func dbUUID(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	return uuid.UUID(id.Bytes).String()
}

func (r *PeopleRepository) invalidate(workspaceID string) {
	r.mu.Lock()
	delete(r.cache, workspaceID)
	r.mu.Unlock()
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
