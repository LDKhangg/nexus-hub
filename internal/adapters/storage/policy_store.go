package storage

import (
	"context"
	"errors"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/policy"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PolicyStore persists policy rule set versions and decisions (Phase 9 P1).
// Rule set writes are used only by the owner CLI; the engine only reads
// the active version and appends decisions.
type PolicyStore struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
}

func NewPolicyStore(pool *pgxpool.Pool) (*PolicyStore, error) {
	if pool == nil {
		return nil, errors.New("policy store requires pool")
	}
	return &PolicyStore{pool: pool, q: sqlc.New(pool)}, nil
}

var _ policy.Store = (*PolicyStore)(nil)

const maxPolicyListLimit = 500

func mapPolicyRuleSet(r sqlc.PolicyRuleSet) policy.StoredRuleSet {
	s := policy.StoredRuleSet{
		WorkspaceID: uuidString(r.WorkspaceID), Version: r.Version, Schema: r.Schema, Body: append([]byte(nil), r.Body...),
		BodySHA256: r.BodySha256, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt.Time, ActivatedAt: tsPtr(r.ActivatedAt), Active: r.IsActive,
	}
	if r.RestoredFromVersion.Valid {
		s.RestoredFrom = r.RestoredFromVersion.Int32
	}
	return s
}

func policyLimit(limit int) int32 {
	if limit <= 0 {
		return 20
	}
	if limit > maxPolicyListLimit {
		return maxPolicyListLimit
	}
	return int32(limit)
}

func (s *PolicyStore) ActiveRuleSet(ctx context.Context, workspaceID string) (policy.StoredRuleSet, bool, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return policy.StoredRuleSet{}, false, errors.New("policy: invalid workspace id")
	}
	row, err := s.q.GetActivePolicyRuleSet(ctx, ws)
	if isNoRows(err) {
		return policy.StoredRuleSet{}, false, nil
	}
	if err != nil {
		return policy.StoredRuleSet{}, false, err
	}
	return mapPolicyRuleSet(row), true, nil
}

func (s *PolicyStore) RuleSetVersion(ctx context.Context, workspaceID string, version int32) (policy.StoredRuleSet, bool, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return policy.StoredRuleSet{}, false, errors.New("policy: invalid workspace id")
	}
	row, err := s.q.GetPolicyRuleSetVersion(ctx, sqlc.GetPolicyRuleSetVersionParams{WorkspaceID: ws, Version: version})
	if isNoRows(err) {
		return policy.StoredRuleSet{}, false, nil
	}
	if err != nil {
		return policy.StoredRuleSet{}, false, err
	}
	return mapPolicyRuleSet(row), true, nil
}

func (s *PolicyStore) RuleSetHistory(ctx context.Context, workspaceID string, limit int) ([]policy.StoredRuleSet, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, errors.New("policy: invalid workspace id")
	}
	rows, err := s.q.ListPolicyRuleSets(ctx, sqlc.ListPolicyRuleSetsParams{WorkspaceID: ws, Limit: policyLimit(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]policy.StoredRuleSet, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapPolicyRuleSet(r))
	}
	return out, nil
}

// ApplyRuleSet stores rs as a new active version in one transaction: the
// workspace row is locked (serializing concurrent applies), the previous
// active version is deactivated and the new one inserted active. A rule set
// equal to the active one stores nothing.
func (s *PolicyStore) ApplyRuleSet(ctx context.Context, workspaceID string, rs policy.RuleSet, createdBy string, restoredFrom int32) (policy.StoredRuleSet, bool, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return policy.StoredRuleSet{}, false, errors.New("policy: invalid workspace id")
	}
	switch createdBy {
	case policy.CreatedByCLI, policy.CreatedByInit:
	default:
		return policy.StoredRuleSet{}, false, errors.New("policy: invalid created_by")
	}
	if restoredFrom < 0 {
		return policy.StoredRuleSet{}, false, errors.New("policy: invalid restored version")
	}
	body, sum, err := policy.Canonical(rs)
	if err != nil {
		return policy.StoredRuleSet{}, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return policy.StoredRuleSet{}, false, err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	if _, err := q.LockPolicyWorkspace(ctx, ws); err != nil {
		if isNoRows(err) {
			return policy.StoredRuleSet{}, false, policy.ErrWorkspaceNotFound
		}
		return policy.StoredRuleSet{}, false, err
	}
	active, err := q.GetActivePolicyRuleSet(ctx, ws)
	switch {
	case err == nil && active.BodySha256 == sum && active.Schema == rs.Schema:
		return mapPolicyRuleSet(active), false, nil
	case err != nil && !isNoRows(err):
		return policy.StoredRuleSet{}, false, err
	}
	next, err := q.NextPolicyRuleSetVersion(ctx, ws)
	if err != nil {
		return policy.StoredRuleSet{}, false, err
	}
	if err := q.DeactivatePolicyRuleSets(ctx, ws); err != nil {
		return policy.StoredRuleSet{}, false, err
	}
	restored := pgtype.Int4{}
	if restoredFrom > 0 {
		restored = pgtype.Int4{Int32: restoredFrom, Valid: true}
	}
	row, err := q.InsertPolicyRuleSet(ctx, sqlc.InsertPolicyRuleSetParams{
		WorkspaceID: ws, Version: next, Schema: rs.Schema, Body: body, BodySha256: sum, CreatedBy: createdBy, RestoredFromVersion: restored,
	})
	if err != nil {
		return policy.StoredRuleSet{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return policy.StoredRuleSet{}, false, err
	}
	return mapPolicyRuleSet(row), true, nil
}

// EnsureDefaultPolicyRuleSet gives a workspace without any rule set
// version 1 (policy.DefaultV1), inside the caller's transaction (nh init).
// It reports whether a row was inserted.
func EnsureDefaultPolicyRuleSet(ctx context.Context, q *sqlc.Queries, workspace pgtype.UUID) (bool, error) {
	rs := policy.DefaultV1()
	body, sum, err := policy.Canonical(rs)
	if err != nil {
		return false, err
	}
	n, err := q.EnsureDefaultPolicyRuleSet(ctx, sqlc.EnsureDefaultPolicyRuleSetParams{WorkspaceID: workspace, Schema: rs.Schema, Body: body, BodySha256: sum})
	return n > 0, err
}

func (s *PolicyStore) RecordDecision(ctx context.Context, rec policy.DecisionRecord) error {
	ws, err := parseDBID(rec.WorkspaceID)
	if err != nil {
		return errors.New("policy: invalid workspace id")
	}
	version := pgtype.Int4{}
	if rec.RuleSetVersion > 0 {
		version = pgtype.Int4{Int32: rec.RuleSetVersion, Valid: true}
	}
	at := pgtype.Timestamptz{Time: rec.CreatedAt, Valid: !rec.CreatedAt.IsZero()}
	if !at.Valid {
		return errors.New("policy: decision time required")
	}
	_, err = s.q.InsertPolicyDecision(ctx, sqlc.InsertPolicyDecisionParams{
		WorkspaceID: ws, Action: rec.Action, Tier: string(rec.Tier), ResourceKey: rec.ResourceKey, Effect: string(rec.Effect),
		Reason: rec.Reason, RuleSetVersion: version, RuleKey: rec.RuleKey, ActorKind: rec.ActorKind,
		ExecutionID: optInt8(rec.ExecutionID), ToolRunID: optInt8(rec.ToolRunID), ApprovalID: optInt8(rec.ApprovalID),
		InputSha256: rec.InputSHA256, CreatedAt: at,
	})
	return err
}

func (s *PolicyStore) ListDecisions(ctx context.Context, workspaceID string, limit int) ([]policy.DecisionRecord, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, errors.New("policy: invalid workspace id")
	}
	rows, err := s.q.ListPolicyDecisions(ctx, sqlc.ListPolicyDecisionsParams{WorkspaceID: ws, Limit: policyLimit(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]policy.DecisionRecord, 0, len(rows))
	for _, r := range rows {
		d := policy.DecisionRecord{
			ID: r.ID, WorkspaceID: uuidString(r.WorkspaceID), Action: r.Action, Tier: policy.Tier(r.Tier), ResourceKey: r.ResourceKey,
			Effect: policy.Effect(r.Effect), Reason: r.Reason, RuleKey: r.RuleKey, ActorKind: r.ActorKind, InputSHA256: r.InputSha256,
			CreatedAt: r.CreatedAt.Time,
		}
		if r.RuleSetVersion.Valid {
			d.RuleSetVersion = r.RuleSetVersion.Int32
		}
		if r.ExecutionID.Valid {
			d.ExecutionID = r.ExecutionID.Int64
		}
		if r.ToolRunID.Valid {
			d.ToolRunID = r.ToolRunID.Int64
		}
		if r.ApprovalID.Valid {
			d.ApprovalID = r.ApprovalID.Int64
		}
		out = append(out, d)
	}
	return out, nil
}
