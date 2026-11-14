package reporting

import (
	"context"
	"errors"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultOwnerUserID is the stable owner created by the bootstrap migration.
const DefaultOwnerUserID = "00000000-0000-0000-0000-000000000001"

// PostgresStore resolves report settings through the generated sqlc queries.
type PostgresStore struct {
	q    *sqlc.Queries
	pool *pgxpool.Pool
}

// NewPostgresStore constructs the production report settings store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	if pool == nil {
		return &PostgresStore{}
	}
	return &PostgresStore{q: sqlc.New(pool), pool: pool}
}

func reportUUID(id string) (pgtype.UUID, error) {
	var out pgtype.UUID
	if err := out.Scan(id); err != nil || !out.Valid {
		return pgtype.UUID{}, errors.New("invalid report scope")
	}
	return out, nil
}

func optionalText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func uuidText(id pgtype.UUID) string { return uuid.UUID(id.Bytes).String() }

func prefsFromRow(row sqlc.ReportPreference) Prefs {
	return Prefs{
		Style: Style(row.Style), ShowReaction: row.ShowReaction, ShowAISummary: row.ShowAiSummary,
		ShowLinks: row.ShowLinks, ShowPRTitles: row.ShowPrTitles, ShowTraceHint: row.ShowTraceHint,
		Language: row.Language.String,
	}
}

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
	return prefsFromRow(row), nil
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

// ensureMember gives a workspace without any owner the stable bootstrap owner
// before an owner-scoped write, so a workspace created without a membership
// row can still save settings. Other users must already be members.
func (s *PostgresStore) ensureMember(ctx context.Context, user, workspace pgtype.UUID) error {
	if uuidText(user) != DefaultOwnerUserID {
		return nil
	}
	_, err := s.q.EnsureWorkspaceOwner(ctx, workspace)
	return err
}

// memberError maps the membership foreign-key violation to a clear error.
func memberError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23503" {
		return ErrNotWorkspaceMember
	}
	return err
}

// OwnerUserID resolves the workspace's P1 owner. The stable bootstrap ID is a
// safe fallback for databases created before the membership row was repaired.
func (s *PostgresStore) OwnerUserID(ctx context.Context, workspaceID string) (string, error) {
	if s == nil || s.q == nil {
		return DefaultOwnerUserID, nil
	}
	workspace, err := reportUUID(workspaceID)
	if err != nil {
		return "", err
	}
	var id pgtype.UUID
	err = s.pool.QueryRow(ctx, `SELECT user_id FROM public.workspace_members WHERE workspace_id=$1 AND role='owner' ORDER BY created_at,user_id LIMIT 1`, workspace).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultOwnerUserID, nil
	}
	if err != nil {
		return "", err
	}
	return uuidText(id), nil
}

// SetPrefs atomically replaces one exact preference scope.
func (s *PostgresStore) SetPrefs(ctx context.Context, userID, workspaceID, behavior string, p Prefs) (Prefs, error) {
	if s == nil || s.q == nil || !p.Valid() {
		return Prefs{}, errors.New("invalid report preferences")
	}
	user, err := reportUUID(userID)
	if err != nil {
		return Prefs{}, err
	}
	workspace, err := reportUUID(workspaceID)
	if err != nil {
		return Prefs{}, err
	}
	if err := s.ensureMember(ctx, user, workspace); err != nil {
		return Prefs{}, err
	}
	row, err := s.q.SetReportPreferences(ctx, sqlc.SetReportPreferencesParams{
		UserID: user, WorkspaceID: workspace, BehaviorName: optionalText(behavior), Style: string(p.Style),
		ShowReaction: p.ShowReaction, ShowAiSummary: p.ShowAISummary, ShowLinks: p.ShowLinks,
		ShowPrTitles: p.ShowPRTitles, ShowTraceHint: p.ShowTraceHint, Language: optionalText(p.Language),
	})
	if err != nil {
		return Prefs{}, memberError(err)
	}
	return prefsFromRow(row), nil
}

// SetTemplate validates and saves one exact template scope.
func (s *PostgresStore) SetTemplate(ctx context.Context, userID, workspaceID, behavior string, kind Kind, body string) error {
	if _, err := ParseKind(string(kind)); err != nil {
		return err
	}
	if err := Validate(body); err != nil {
		return err
	}
	user, err := reportUUID(userID)
	if err != nil {
		return err
	}
	workspace, err := reportUUID(workspaceID)
	if err != nil {
		return err
	}
	if s == nil || s.q == nil {
		return errors.New("report settings store is not configured")
	}
	if err := s.ensureMember(ctx, user, workspace); err != nil {
		return err
	}
	_, err = s.q.SetReportTemplate(ctx, sqlc.SetReportTemplateParams{UserID: user, WorkspaceID: workspace, BehaviorName: optionalText(behavior), Kind: string(kind), Body: body})
	return memberError(err)
}

// DeleteTemplate resets one exact template scope.
func (s *PostgresStore) DeleteTemplate(ctx context.Context, userID, workspaceID, behavior string, kind Kind) (bool, error) {
	user, err := reportUUID(userID)
	if err != nil {
		return false, err
	}
	workspace, err := reportUUID(workspaceID)
	if err != nil {
		return false, err
	}
	n, err := s.q.DeleteReportTemplate(ctx, sqlc.DeleteReportTemplateParams{UserID: user, WorkspaceID: workspace, BehaviorName: optionalText(behavior), Kind: string(kind)})
	return n > 0, err
}

// RememberTemplatePrompt records which template a ForceReply message edits.
func (s *PostgresStore) RememberTemplatePrompt(ctx context.Context, userID, workspaceID, behavior string, kind Kind, messageID int64, expiresAt time.Time) error {
	user, err := reportUUID(userID)
	if err != nil {
		return err
	}
	workspace, err := reportUUID(workspaceID)
	if err != nil {
		return err
	}
	if s == nil || s.q == nil {
		return errors.New("report settings store is not configured")
	}
	if err := s.ensureMember(ctx, user, workspace); err != nil {
		return err
	}
	return memberError(s.q.CreateReportTemplatePrompt(ctx, sqlc.CreateReportTemplatePromptParams{
		UserID: user, WorkspaceID: workspace, MessageID: messageID, BehaviorName: optionalText(behavior),
		Kind: string(kind), ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}))
}

// StageTemplateReply keeps template text out of the generic update queue.
func (s *PostgresStore) StageTemplateReply(ctx context.Context, workspaceID string, messageID int64, body string, at time.Time) (string, error) {
	workspace, err := reportUUID(workspaceID)
	if err != nil {
		return "", err
	}
	id, err := s.q.StageReportTemplateReply(ctx, sqlc.StageReportTemplateReplyParams{
		Body: pgtype.Text{String: body, Valid: true}, WorkspaceID: workspace, MessageID: messageID, AtTime: pgtype.Timestamptz{Time: at, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrTemplatePromptNotFound
	}
	return uuidText(id), err
}

// TemplateReply is a staged owner reply ready for validation and saving.
type TemplateReply struct {
	UserID, Behavior, Body string
	Kind                   Kind
}

// ConsumeTemplateReply removes and returns one staged reply.
func (s *PostgresStore) ConsumeTemplateReply(ctx context.Context, workspaceID, id string) (TemplateReply, error) {
	workspace, err := reportUUID(workspaceID)
	if err != nil {
		return TemplateReply{}, err
	}
	replyID, err := reportUUID(id)
	if err != nil {
		return TemplateReply{}, err
	}
	row, err := s.q.ConsumeReportTemplateReply(ctx, sqlc.ConsumeReportTemplateReplyParams{WorkspaceID: workspace, ID: replyID})
	if err != nil {
		return TemplateReply{}, err
	}
	return TemplateReply{UserID: uuidText(row.UserID), Behavior: row.BehaviorName.String, Body: row.PendingBody.String, Kind: Kind(row.Kind)}, nil
}
