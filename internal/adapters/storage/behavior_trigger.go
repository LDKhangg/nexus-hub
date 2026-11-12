package storage

import (
	"context"
	"encoding/json"
	"errors"
	"log"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/settings"
	"github.com/jackc/pgx/v5/pgtype"
)

const ownerClickUpSetting = "owner_clickup_user_id"

// ingestTriggers creates behavior runs for ClickUp events inside the ingest
// transaction, so a stored callback and the runs it starts commit together:
// a crash or a failed commit loses neither, and ClickUp's retry is deduped
// per (version, subject, revision). Enabled versions and the owner's ClickUp
// user ID are read once per batch, inside the same transaction.
type ingestTriggers struct {
	q        *sqlc.Queries
	ws       pgtype.UUID
	loaded   bool
	versions []behavior.Version
	owner    string
}

func newIngestTriggers(q *sqlc.Queries, ws pgtype.UUID) *ingestTriggers {
	return &ingestTriggers{q: q, ws: ws}
}

func (t *ingestTriggers) load(ctx context.Context) error {
	if t.loaded {
		return nil
	}
	rows, err := t.q.ListEnabledBehaviorVersions(ctx, t.ws)
	if err != nil {
		return err
	}
	for _, r := range rows {
		t.versions = append(t.versions, behavior.Version{
			ID: r.ID, BehaviorID: r.BehaviorID, Name: r.Name, Version: int(r.Version), Document: r.Document,
			Vars: r.Vars, PublishedAt: r.PublishedAt.Time,
		})
	}
	if len(t.versions) > 0 {
		owner, err := t.ownerClickUpID(ctx)
		if err != nil {
			return err
		}
		t.owner = owner
	}
	t.loaded = true
	return nil
}

// ownerClickUpID reads the setting; an invalid stored value counts as unset
// (filters that need it then never match) and is logged without the value.
func (t *ingestTriggers) ownerClickUpID(ctx context.Context) (string, error) {
	raw, err := t.q.GetSetting(ctx, ownerClickUpSetting)
	if isNoRows(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var v string
	if settings.Validate(ownerClickUpSetting, raw) != nil || json.Unmarshal(raw, &v) != nil {
		log.Printf("behavior ingest: setting %s is invalid; owner filters do not match", ownerClickUpSetting)
		return "", nil
	}
	return v, nil
}

// start creates the runs one event starts and returns how many were new.
// Only database errors are returned; an event or behavior that cannot make
// a run is skipped.
func (t *ingestTriggers) start(ctx context.Context, e domain.NormalizedEvent) (int, error) {
	ev, ok := behavior.EventFromNormalized(e)
	if !ok {
		return 0, nil
	}
	if err := t.load(ctx); err != nil {
		return 0, err
	}
	if len(t.versions) == 0 {
		return 0, nil
	}
	created := 0
	for _, m := range behavior.MatchEvent(ev, t.versions, t.owner) {
		nr, ok := m.RunFor(ev)
		if !ok {
			log.Printf("behavior ingest: event for version %d has no valid subject or revision; skipped", m.Version.ID)
			continue
		}
		run, isNew, err := createRun(ctx, t.q, t.ws, nr)
		if errors.Is(err, behavior.ErrInvalid) || errors.Is(err, behavior.ErrConflict) {
			log.Printf("behavior ingest: run for version %d cannot be stored; skipped", m.Version.ID)
			continue
		}
		if err != nil {
			return created, err
		}
		if isNew {
			created++
			log.Printf("behavior ingest: run %d created for version %d corr=%s", run.ID, m.Version.ID, run.CorrelationID)
		}
	}
	return created, nil
}
