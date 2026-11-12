package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BehaviorStore persists the behavior engine's definitions, runs, items,
// approvals, inbound updates, profile and decision memory.
type BehaviorStore struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
}

func NewBehaviorStore(pool *pgxpool.Pool) (*BehaviorStore, error) {
	if pool == nil {
		return nil, errors.New("behavior store requires pool")
	}
	return &BehaviorStore{pool: pool, q: sqlc.New(pool)}, nil
}

var (
	_ behavior.DefinitionStore = (*BehaviorStore)(nil)
	_ behavior.RunStore        = (*BehaviorStore)(nil)
	_ behavior.ApprovalStore   = (*BehaviorStore)(nil)
	_ behavior.InboxStore      = (*BehaviorStore)(nil)
	_ behavior.ProfileStore    = (*BehaviorStore)(nil)
	_ behavior.MemoryStore     = (*BehaviorStore)(nil)
)

// --- helpers ---

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// notFound maps pgx.ErrNoRows to behavior.ErrNotFound.
func notFound(err error) error {
	if isNoRows(err) {
		return behavior.ErrNotFound
	}
	return err
}

func invalid(field string) error { return fmt.Errorf("%w: %s", behavior.ErrInvalid, field) }

func tsPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func optInt8(v int64) pgtype.Int8 { return pgtype.Int8{Int64: v, Valid: v != 0} }

func optInt4(v *int) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*v), Valid: true}
}

func intPtr(v pgtype.Int4) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int32)
	return &n
}

// jsonKind returns raw, or def when raw is empty, after checking that it is
// a JSON value of the wanted kind ('{' object or '[' array).
func jsonKind(raw json.RawMessage, def string, open byte, field string) ([]byte, error) {
	if len(raw) == 0 {
		return []byte(def), nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if !json.Valid(raw) || trimmed == "" || trimmed[0] != open {
		return nil, invalid(field)
	}
	return raw, nil
}

func objectJSON(raw json.RawMessage, field string) ([]byte, error) {
	return jsonKind(raw, "{}", '{', field)
}

func claimToken(token string) (pgtype.UUID, error) {
	t, err := parseDBID(token)
	if err != nil {
		return t, behavior.ErrStaleClaim
	}
	return t, nil
}

func newToken() (string, pgtype.UUID, error) {
	tok, err := uuid.NewRandom()
	if err != nil {
		return "", pgtype.UUID{}, err
	}
	db, err := parseDBID(tok.String())
	return tok.String(), db, err
}

func clamp(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

// --- definitions ---

func mapBehavior(b sqlc.Behavior, latest int) behavior.Behavior {
	return behavior.Behavior{
		ID: b.ID, Name: b.Name, Status: behavior.Status(b.Status), Draft: b.Draft, DraftVars: b.DraftVars,
		DraftSource: b.DraftSource, LatestVersion: latest, CreatedAt: b.CreatedAt.Time, UpdatedAt: b.UpdatedAt.Time,
	}
}

func mapVersion(v sqlc.BehaviorVersion, name string) behavior.Version {
	return behavior.Version{
		ID: v.ID, BehaviorID: v.BehaviorID, Name: name, Version: int(v.Version), Document: v.Document,
		Vars: v.Vars, Source: v.Source, PublishedAt: v.PublishedAt.Time,
	}
}

func (s *BehaviorStore) SaveDraft(ctx context.Context, workspaceID, name string, draft, vars json.RawMessage, source string) (behavior.Behavior, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.Behavior{}, err
	}
	if len(draft) == 0 {
		return behavior.Behavior{}, invalid("draft")
	}
	d, err := objectJSON(draft, "draft")
	if err != nil {
		return behavior.Behavior{}, err
	}
	v, err := objectJSON(vars, "vars")
	if err != nil {
		return behavior.Behavior{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return behavior.Behavior{}, err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	row, err := q.UpsertBehaviorDraft(ctx, sqlc.UpsertBehaviorDraftParams{WorkspaceID: ws, Name: name, Draft: d, DraftVars: v, DraftSource: source})
	if err != nil {
		return behavior.Behavior{}, err
	}
	next, err := q.NextBehaviorVersion(ctx, row.ID)
	if err != nil {
		return behavior.Behavior{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return behavior.Behavior{}, err
	}
	return mapBehavior(row, int(next)-1), nil
}

func (s *BehaviorStore) GetBehavior(ctx context.Context, workspaceID, name string) (behavior.Behavior, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.Behavior{}, err
	}
	row, err := s.q.GetBehaviorByName(ctx, sqlc.GetBehaviorByNameParams{WorkspaceID: ws, Name: name})
	if err != nil {
		return behavior.Behavior{}, notFound(err)
	}
	next, err := s.q.NextBehaviorVersion(ctx, row.ID)
	if err != nil {
		return behavior.Behavior{}, err
	}
	return mapBehavior(row, int(next)-1), nil
}

// ListBehaviors returns every behavior without its draft.
func (s *BehaviorStore) ListBehaviors(ctx context.Context, workspaceID string) ([]behavior.Behavior, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListBehaviors(ctx, ws)
	if err != nil {
		return nil, err
	}
	out := make([]behavior.Behavior, 0, len(rows))
	for _, r := range rows {
		out = append(out, behavior.Behavior{
			ID: r.ID, Name: r.Name, Status: behavior.Status(r.Status), LatestVersion: int(r.LatestVersion),
			CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
		})
	}
	return out, nil
}

func (s *BehaviorStore) Publish(ctx context.Context, workspaceID, name string, check func(behavior.Behavior) error) (behavior.Version, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.Version{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return behavior.Version{}, err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	row, err := q.LockBehaviorByName(ctx, sqlc.LockBehaviorByNameParams{WorkspaceID: ws, Name: name})
	if err != nil {
		return behavior.Version{}, notFound(err)
	}
	next, err := q.NextBehaviorVersion(ctx, row.ID)
	if err != nil {
		return behavior.Version{}, err
	}
	if check != nil {
		if err := check(mapBehavior(row, int(next)-1)); err != nil {
			return behavior.Version{}, err
		}
	}
	v, err := q.InsertBehaviorVersion(ctx, sqlc.InsertBehaviorVersionParams{
		WorkspaceID: ws, BehaviorID: row.ID, Version: next, Document: row.Draft, Vars: row.DraftVars, Source: row.DraftSource,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return behavior.Version{}, behavior.ErrConflict
		}
		return behavior.Version{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return behavior.Version{}, err
	}
	return mapVersion(v, row.Name), nil
}

func (s *BehaviorStore) SetStatus(ctx context.Context, workspaceID, name string, status behavior.Status) error {
	if status != behavior.StatusEnabled && status != behavior.StatusDisabled {
		return invalid("status")
	}
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	row, err := q.LockBehaviorByName(ctx, sqlc.LockBehaviorByNameParams{WorkspaceID: ws, Name: name})
	if err != nil {
		return notFound(err)
	}
	if status == behavior.StatusEnabled {
		next, err := q.NextBehaviorVersion(ctx, row.ID)
		if err != nil {
			return err
		}
		if next <= 1 {
			return behavior.ErrNotPublished
		}
	}
	if _, err := q.SetBehaviorStatus(ctx, sqlc.SetBehaviorStatusParams{Status: string(status), WorkspaceID: ws, ID: row.ID}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *BehaviorStore) GetVersion(ctx context.Context, workspaceID, name string, version int) (behavior.Version, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.Version{}, err
	}
	if version < 0 {
		return behavior.Version{}, invalid("version")
	}
	b, err := s.q.GetBehaviorByName(ctx, sqlc.GetBehaviorByNameParams{WorkspaceID: ws, Name: name})
	if err != nil {
		return behavior.Version{}, notFound(err)
	}
	var v sqlc.BehaviorVersion
	if version == 0 {
		v, err = s.q.GetLatestBehaviorVersion(ctx, sqlc.GetLatestBehaviorVersionParams{WorkspaceID: ws, BehaviorID: b.ID})
	} else {
		v, err = s.q.GetBehaviorVersion(ctx, sqlc.GetBehaviorVersionParams{WorkspaceID: ws, BehaviorID: b.ID, Version: int32(version)})
	}
	if err != nil {
		return behavior.Version{}, notFound(err)
	}
	return mapVersion(v, b.Name), nil
}

func (s *BehaviorStore) GetVersionByID(ctx context.Context, workspaceID string, id int64) (behavior.Version, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.Version{}, err
	}
	v, err := s.q.GetBehaviorVersionByID(ctx, sqlc.GetBehaviorVersionByIDParams{WorkspaceID: ws, ID: id})
	if err != nil {
		return behavior.Version{}, notFound(err)
	}
	b, err := s.q.GetBehaviorByID(ctx, sqlc.GetBehaviorByIDParams{WorkspaceID: ws, ID: v.BehaviorID})
	if err != nil {
		return behavior.Version{}, notFound(err)
	}
	return mapVersion(v, b.Name), nil
}

// ListVersions returns every version without its document.
func (s *BehaviorStore) ListVersions(ctx context.Context, workspaceID, name string) ([]behavior.Version, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, err
	}
	b, err := s.q.GetBehaviorByName(ctx, sqlc.GetBehaviorByNameParams{WorkspaceID: ws, Name: name})
	if err != nil {
		return nil, notFound(err)
	}
	rows, err := s.q.ListBehaviorVersions(ctx, sqlc.ListBehaviorVersionsParams{WorkspaceID: ws, BehaviorID: b.ID})
	if err != nil {
		return nil, err
	}
	out := make([]behavior.Version, 0, len(rows))
	for _, r := range rows {
		out = append(out, behavior.Version{ID: r.ID, BehaviorID: r.BehaviorID, Name: b.Name, Version: int(r.Version), PublishedAt: r.PublishedAt.Time})
	}
	return out, nil
}

func (s *BehaviorStore) EnabledVersions(ctx context.Context, workspaceID string) ([]behavior.Version, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListEnabledBehaviorVersions(ctx, ws)
	if err != nil {
		return nil, err
	}
	out := make([]behavior.Version, 0, len(rows))
	for _, r := range rows {
		out = append(out, behavior.Version{
			ID: r.ID, BehaviorID: r.BehaviorID, Name: r.Name, Version: int(r.Version), Document: r.Document,
			Vars: r.Vars, PublishedAt: r.PublishedAt.Time,
		})
	}
	return out, nil
}

func (s *BehaviorStore) Stats(ctx context.Context, workspaceID, name string) ([]behavior.VersionStats, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, err
	}
	b, err := s.q.GetBehaviorByName(ctx, sqlc.GetBehaviorByNameParams{WorkspaceID: ws, Name: name})
	if err != nil {
		return nil, notFound(err)
	}
	rows, err := s.q.BehaviorVersionStats(ctx, sqlc.BehaviorVersionStatsParams{WorkspaceID: ws, BehaviorID: b.ID})
	if err != nil {
		return nil, err
	}
	out := make([]behavior.VersionStats, 0, len(rows))
	for _, r := range rows {
		out = append(out, behavior.VersionStats{
			Version: int(r.Version), Runs: r.Runs, Undos: r.Undos, AutoItems: r.AutoItems, NotifyItems: r.NotifyItems,
			AskItems: r.AskItems, Conflicts: r.Conflicts, RatedDown: r.RatedDown,
		})
	}
	return out, nil
}

// --- runs ---

func mapRun(r sqlc.BehaviorRun, name string) behavior.Run {
	return behavior.Run{
		ID: r.ID, WorkspaceID: uuidString(r.WorkspaceID), BehaviorID: r.BehaviorID, BehaviorName: name,
		VersionID: r.BehaviorVersionID, Version: int(r.Version), TriggerKind: r.TriggerKind, Trigger: r.Trigger,
		Subject: r.Subject, Revision: r.Revision, Status: behavior.RunStatus(r.Status),
		CorrelationID: uuidString(r.CorrelationID), Attempts: int(r.Attempts),
		Usage: behavior.Usage{
			InputTokens: int(r.InputTokens), OutputTokens: int(r.OutputTokens), CacheReadTokens: int(r.CacheReadTokens),
			CacheWriteTokens: int(r.CacheWriteTokens), Steps: int(r.Steps),
		},
		ErrorCode: r.ErrorCode.String, Snapshot: r.Snapshot, UndoneAt: tsPtr(r.UndoneAt), RunAt: r.RunAt.Time,
		StartedAt: tsPtr(r.StartedAt), FinishedAt: tsPtr(r.FinishedAt), CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}
}

func (s *BehaviorStore) CreateRun(ctx context.Context, workspaceID string, r behavior.NewRun) (behavior.Run, bool, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.Run{}, false, err
	}
	switch {
	case r.BehaviorID == 0 || r.VersionID == 0 || r.Version < 1:
		return behavior.Run{}, false, invalid("version")
	case strings.TrimSpace(r.TriggerKind) == "":
		return behavior.Run{}, false, invalid("trigger_kind")
	case strings.TrimSpace(r.Subject) == "" || len(r.Subject) > 512:
		return behavior.Run{}, false, invalid("subject")
	case strings.TrimSpace(r.Revision) == "" || len(r.Revision) > 512:
		return behavior.Run{}, false, invalid("revision")
	}
	trigger, err := objectJSON(r.Trigger, "trigger")
	if err != nil {
		return behavior.Run{}, false, err
	}
	corr := r.CorrelationID
	if corr == "" {
		id, err := uuid.NewV7()
		if err != nil {
			return behavior.Run{}, false, err
		}
		corr = id.String()
	}
	corrID, err := parseDBID(corr)
	if err != nil {
		return behavior.Run{}, false, invalid("correlation_id")
	}
	runAt := r.RunAt
	if runAt.IsZero() {
		runAt = time.Now()
	}
	row, err := s.q.CreateBehaviorRun(ctx, sqlc.CreateBehaviorRunParams{
		WorkspaceID: ws, BehaviorID: r.BehaviorID, BehaviorVersionID: r.VersionID, Version: int32(r.Version),
		TriggerKind: r.TriggerKind, Trigger: trigger, Subject: r.Subject, Revision: r.Revision, CorrelationID: corrID, RunAt: ts(runAt),
	})
	if err == nil {
		return mapRun(row, ""), true, nil
	}
	if !isNoRows(err) {
		return behavior.Run{}, false, err
	}
	existing, err := s.q.GetBehaviorRunByDedupe(ctx, sqlc.GetBehaviorRunByDedupeParams{
		WorkspaceID: ws, BehaviorVersionID: r.VersionID, Subject: r.Subject, Revision: r.Revision,
	})
	if err != nil {
		// The existing run is outside this workspace: the version id
		// belongs to another workspace.
		if isNoRows(err) {
			return behavior.Run{}, false, behavior.ErrConflict
		}
		return behavior.Run{}, false, err
	}
	return mapRun(existing, ""), false, nil
}

func (s *BehaviorStore) ClaimRun(ctx context.Context, now time.Time, lease time.Duration, maxAttempts int) (*behavior.Run, error) {
	if lease <= 0 || maxAttempts < 1 {
		return nil, invalid("lease")
	}
	tok, token, err := newToken()
	if err != nil {
		return nil, err
	}
	at := ts(now)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	if _, err := q.FailExhaustedBehaviorRuns(ctx, sqlc.FailExhaustedBehaviorRunsParams{AtTime: at, MaxAttempts: int32(maxAttempts)}); err != nil {
		return nil, err
	}
	id, err := q.SelectBehaviorRunClaimCandidate(ctx, sqlc.SelectBehaviorRunClaimCandidateParams{AtTime: at, MaxAttempts: int32(maxAttempts)})
	if isNoRows(err) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	row, err := q.ClaimBehaviorRunByID(ctx, sqlc.ClaimBehaviorRunByIDParams{Token: token, AtTime: at, LeaseSecs: lease.Seconds(), ID: id})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	run := mapRun(row, "")
	run.ClaimToken = tok
	return &run, nil
}

func (s *BehaviorStore) AddUsage(ctx context.Context, runID int64, token string, u behavior.Usage) error {
	t, err := claimToken(token)
	if err != nil {
		return err
	}
	n, err := s.q.AddBehaviorRunUsage(ctx, sqlc.AddBehaviorRunUsageParams{
		Input: int32(u.InputTokens), Output: int32(u.OutputTokens), CacheRead: int32(u.CacheReadTokens),
		CacheWrite: int32(u.CacheWriteTokens), Steps: int32(u.Steps), ID: runID, Token: t,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return behavior.ErrStaleClaim
	}
	return nil
}

func (s *BehaviorStore) FinishRun(ctx context.Context, runID int64, token string, status behavior.RunStatus, code string) error {
	switch status {
	case behavior.RunDone, behavior.RunFailed, behavior.RunCanceled:
	default:
		return invalid("status")
	}
	t, err := claimToken(token)
	if err != nil {
		return err
	}
	n, err := s.q.FinishBehaviorRun(ctx, sqlc.FinishBehaviorRunParams{Status: string(status), Code: optText(code), ID: runID, Token: t})
	if err != nil {
		return err
	}
	if n == 0 {
		return behavior.ErrStaleClaim
	}
	return nil
}

func (s *BehaviorStore) RetryRun(ctx context.Context, runID int64, token string, next time.Time, code string) error {
	t, err := claimToken(token)
	if err != nil {
		return err
	}
	n, err := s.q.RetryBehaviorRun(ctx, sqlc.RetryBehaviorRunParams{NextAt: ts(next), Code: code, ID: runID, Token: t})
	if err != nil {
		return err
	}
	if n == 0 {
		return behavior.ErrStaleClaim
	}
	return nil
}

func (s *BehaviorStore) CancelPendingRun(ctx context.Context, workspaceID string, runID int64) error {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return err
	}
	n, err := s.q.CancelPendingBehaviorRun(ctx, sqlc.CancelPendingBehaviorRunParams{WorkspaceID: ws, ID: runID})
	if err != nil {
		return err
	}
	if n == 0 {
		return behavior.ErrConflict
	}
	return nil
}

func (s *BehaviorStore) SetSnapshot(ctx context.Context, workspaceID string, runID int64, snapshot json.RawMessage) (bool, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return false, err
	}
	if len(snapshot) == 0 {
		return false, invalid("snapshot")
	}
	snap, err := objectJSON(snapshot, "snapshot")
	if err != nil {
		return false, err
	}
	n, err := s.q.SetBehaviorRunSnapshot(ctx, sqlc.SetBehaviorRunSnapshotParams{Snapshot: snap, WorkspaceID: ws, ID: runID})
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (s *BehaviorStore) MarkUndone(ctx context.Context, workspaceID string, runID int64) error {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return err
	}
	n, err := s.q.MarkBehaviorRunUndone(ctx, sqlc.MarkBehaviorRunUndoneParams{WorkspaceID: ws, ID: runID})
	if err != nil {
		return err
	}
	if n == 0 {
		return behavior.ErrConflict
	}
	return nil
}

func (s *BehaviorStore) GetRun(ctx context.Context, workspaceID string, runID int64) (behavior.Run, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.Run{}, err
	}
	row, err := s.q.GetBehaviorRun(ctx, sqlc.GetBehaviorRunParams{WorkspaceID: ws, ID: runID})
	if err != nil {
		return behavior.Run{}, notFound(err)
	}
	return mapRun(row.BehaviorRun, row.BehaviorName), nil
}

func (s *BehaviorStore) LatestRunBySubject(ctx context.Context, workspaceID, subject string) (behavior.Run, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.Run{}, err
	}
	row, err := s.q.LatestBehaviorRunBySubject(ctx, sqlc.LatestBehaviorRunBySubjectParams{WorkspaceID: ws, Subject: subject})
	if err != nil {
		return behavior.Run{}, notFound(err)
	}
	return mapRun(row.BehaviorRun, row.BehaviorName), nil
}

// --- items ---

func mapItem(r sqlc.RunItem) (behavior.Item, error) {
	var cites []behavior.Citation
	if len(r.Citations) > 0 {
		if err := json.Unmarshal(r.Citations, &cites); err != nil {
			return behavior.Item{}, errors.New("behavior store: stored citations are not valid")
		}
	}
	return behavior.Item{
		ID: r.ID, RunID: r.RunID, Key: r.ItemKey, Label: r.Label, Text: r.Text, Proposal: r.Proposal, Citations: cites,
		Topic: r.Topic, Confidence: r.Confidence, Informational: r.Informational, Flags: r.Flags,
		Class: behavior.Class(r.Class), Rule: r.Rule, Status: behavior.ItemStatus(r.Status), ActionResult: r.ActionResult,
		Rating: behavior.Rating(r.Rating.String), RatingReason: r.RatingReason.String, RatedAt: tsPtr(r.RatedAt),
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}, nil
}

func validItemStatus(s behavior.ItemStatus) bool {
	switch s {
	case behavior.ItemDrafted, behavior.ItemDone, behavior.ItemAsked, behavior.ItemAnswered, behavior.ItemExpired,
		behavior.ItemSkipped, behavior.ItemConflict, behavior.ItemFailed:
		return true
	}
	return false
}

func (s *BehaviorStore) AddItem(ctx context.Context, workspaceID string, it behavior.NewItem) (behavior.Item, bool, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.Item{}, false, err
	}
	switch it.Class {
	case behavior.ClassAuto, behavior.ClassNotify, behavior.ClassAsk:
	default:
		return behavior.Item{}, false, invalid("class")
	}
	status := it.Status
	if status == "" {
		status = behavior.ItemDrafted
	}
	switch {
	case !validItemStatus(status):
		return behavior.Item{}, false, invalid("status")
	case strings.TrimSpace(it.Key) == "" || len(it.Key) > 200:
		return behavior.Item{}, false, invalid("key")
	case strings.TrimSpace(it.Rule) == "":
		return behavior.Item{}, false, invalid("rule")
	case !(it.Confidence >= 0 && it.Confidence <= 1):
		return behavior.Item{}, false, invalid("confidence")
	}
	cites := it.Citations
	if cites == nil {
		cites = []behavior.Citation{}
	}
	citeJSON, err := json.Marshal(cites)
	if err != nil {
		return behavior.Item{}, false, err
	}
	flags := it.Flags
	if flags == nil {
		flags = []string{}
	}
	row, err := s.q.InsertRunItem(ctx, sqlc.InsertRunItemParams{
		WorkspaceID: ws, RunID: it.RunID, ItemKey: it.Key, Label: it.Label, Text: it.Text, Proposal: it.Proposal,
		Citations: citeJSON, Topic: it.Topic, Confidence: it.Confidence, Informational: it.Informational, Flags: flags,
		Class: string(it.Class), Rule: it.Rule, Status: string(status),
	})
	if err == nil {
		item, err := mapItem(row)
		return item, true, err
	}
	if !isNoRows(err) {
		return behavior.Item{}, false, err
	}
	existing, err := s.q.GetRunItemByKey(ctx, sqlc.GetRunItemByKeyParams{WorkspaceID: ws, RunID: it.RunID, ItemKey: it.Key})
	if err != nil {
		return behavior.Item{}, false, notFound(err)
	}
	item, err := mapItem(existing)
	return item, false, err
}

func (s *BehaviorStore) GetItem(ctx context.Context, workspaceID string, itemID int64) (behavior.Item, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.Item{}, err
	}
	row, err := s.q.GetRunItem(ctx, sqlc.GetRunItemParams{WorkspaceID: ws, ID: itemID})
	if err != nil {
		return behavior.Item{}, notFound(err)
	}
	return mapItem(row)
}

func (s *BehaviorStore) GetItemByKey(ctx context.Context, workspaceID string, runID int64, key string) (behavior.Item, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.Item{}, err
	}
	row, err := s.q.GetRunItemByKey(ctx, sqlc.GetRunItemByKeyParams{WorkspaceID: ws, RunID: runID, ItemKey: key})
	if err != nil {
		return behavior.Item{}, notFound(err)
	}
	return mapItem(row)
}

func (s *BehaviorStore) ListItems(ctx context.Context, workspaceID string, runID int64) ([]behavior.Item, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListRunItems(ctx, sqlc.ListRunItemsParams{WorkspaceID: ws, RunID: runID})
	if err != nil {
		return nil, err
	}
	out := make([]behavior.Item, 0, len(rows))
	for _, r := range rows {
		it, err := mapItem(r)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, nil
}

func (s *BehaviorStore) TransitionItem(ctx context.Context, workspaceID string, itemID int64, from []behavior.ItemStatus, to behavior.ItemStatus, result json.RawMessage) (behavior.Item, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.Item{}, err
	}
	if len(from) == 0 || !validItemStatus(to) {
		return behavior.Item{}, invalid("status")
	}
	froms := make([]string, 0, len(from))
	for _, f := range from {
		if !validItemStatus(f) {
			return behavior.Item{}, invalid("status")
		}
		froms = append(froms, string(f))
	}
	var res []byte
	if len(result) > 0 {
		if res, err = objectJSON(result, "action_result"); err != nil {
			return behavior.Item{}, err
		}
	}
	row, err := s.q.TransitionRunItem(ctx, sqlc.TransitionRunItemParams{ToStatus: string(to), ActionResult: res, WorkspaceID: ws, ID: itemID, FromStatuses: froms})
	if err == nil {
		return mapItem(row)
	}
	if !isNoRows(err) {
		return behavior.Item{}, err
	}
	if _, err := s.q.GetRunItem(ctx, sqlc.GetRunItemParams{WorkspaceID: ws, ID: itemID}); err != nil {
		return behavior.Item{}, notFound(err)
	}
	return behavior.Item{}, behavior.ErrConflict
}

func (s *BehaviorStore) RateItem(ctx context.Context, workspaceID string, itemID int64, rating behavior.Rating, reason string) error {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return err
	}
	if rating != behavior.RatingUp && rating != behavior.RatingDown {
		return invalid("rating")
	}
	if utf8.RuneCountInString(reason) > 1000 {
		return invalid("rating_reason")
	}
	n, err := s.q.RateRunItem(ctx, sqlc.RateRunItemParams{Rating: string(rating), Reason: optText(reason), WorkspaceID: ws, ID: itemID})
	if err != nil {
		return err
	}
	if n == 0 {
		return behavior.ErrNotFound
	}
	return nil
}
