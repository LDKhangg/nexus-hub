package reporting

import (
	"context"
	"errors"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultOwnerUserID is the stable owner created by the bootstrap migration.
const DefaultOwnerUserID = "00000000-0000-0000-0000-000000000001"

// PostgresStore resolves report settings through the generated sqlc queries.
type PostgresStore struct{ q *sqlc.Queries }

// NewPostgresStore constructs the production report settings store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	if pool == nil {
		return &PostgresStore{}
	}
	return &PostgresStore{q: sqlc.New(pool)}
}

func reportUUID(id string) (pgtype.UUID, error) {
	var out pgtype.UUID
	if err := out.Scan(id); err != nil || !out.Valid {
		return pgtype.UUID{}, errors.New("invalid report scope")
	}
	return out, nil
}

func optionalText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

// Prefs resolves a behavior override before the workspace default. A missing
// row has the explicit owner default, rather than database zero values.
func (s *PostgresStore) Prefs(ctx context.Context, userID, workspaceID, behavior string) (Prefs, error) {
	if s == nil || s.q == nil {
		return DefaultOwnerPrefs(), nil
	}
	user, err := reportUUID(userID)
	if err != nil {
		return Prefs{}, err
	}
	workspace, err := reportUUID(workspaceID)
	if err != nil {
		return Prefs{}, err
	}
	row, err := s.q.ResolveReportPreferences(ctx, sqlc.ResolveReportPreferencesParams{
		UserID: user, WorkspaceID: workspace, BehaviorName: optionalText(behavior),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultOwnerPrefs(), nil
	}
	if err != nil {
		return Prefs{}, err
	}
	return Prefs{
		Style: Style(row.Style), ShowReaction: row.ShowReaction, ShowAISummary: row.ShowAiSummary,
		ShowLinks: row.ShowLinks, ShowPRTitles: row.ShowPrTitles, ShowTraceHint: row.ShowTraceHint,
		Language: row.Language.String,
	}, nil
}

// Template resolves a behavior override before the workspace default.
func (s *PostgresStore) Template(ctx context.Context, userID, workspaceID, behavior string, kind Kind) (string, bool, error) {
	if s == nil || s.q == nil {
		return "", false, nil
	}
	user, err := reportUUID(userID)
	if err != nil {
		return "", false, err
	}
	workspace, err := reportUUID(workspaceID)
	if err != nil {
		return "", false, err
	}
	row, err := s.q.ResolveReportTemplate(ctx, sqlc.ResolveReportTemplateParams{
		UserID: user, WorkspaceID: workspace, Kind: string(kind), BehaviorName: optionalText(behavior),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return row.Body, true, nil
}

var _ Store = (*PostgresStore)(nil)
