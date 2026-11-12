package storage

import (
	"context"
	"errors"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ToolRunStore is the tool_runs audit trail with write idempotency.
type ToolRunStore struct{ pool *pgxpool.Pool }

func NewToolRunStore(pool *pgxpool.Pool) (*ToolRunStore, error) {
	if pool == nil {
		return nil, errors.New("tool run store requires pool")
	}
	return &ToolRunStore{pool: pool}, nil
}

var (
	_ tools.ToolRunStore     = (*ToolRunStore)(nil)
	_ tools.JobBudgetCounter = (*ToolRunStore)(nil)
)

func optText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func (s *ToolRunStore) insert(ctx context.Context, q *sqlc.Queries, r tools.RunStart, status, code string) (int64, error) {
	ws, err := parseDBID(r.WorkspaceID)
	if err != nil {
		return 0, err
	}
	var input []byte
	if len(r.InputRedacted) > 0 {
		input = r.InputRedacted
	}
	return q.InsertToolRun(ctx, sqlc.InsertToolRunParams{
		WorkspaceID: ws, AgentJobID: r.JobID, Step: int32(r.Step), Tool: r.Tool, Tier: string(r.Tier),
		Repo: optText(r.Repo), Account: optText(r.Account), IdempotencyKey: r.Key, InputRedacted: input, Status: status, ErrorCode: optText(code),
	})
}

// Begin records a started run. For non-read tiers it returns the prior run
// instead when the same key already succeeded (replay) or may have taken
// effect (started, timeout, unknown_outcome → unknown_outcome).
func (s *ToolRunStore) Begin(ctx context.Context, r tools.RunStart) (int64, *tools.PriorRun, error) {
	if r.Tier == tenant.TierRead {
		id, err := s.insert(ctx, sqlc.New(s.pool), r, tools.StatusStarted, "")
		return id, nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	if err := q.LockToolRunKey(ctx, r.Key); err != nil {
		return 0, nil, err
	}
	prior, err := q.FindPriorToolRun(ctx, r.Key)
	switch {
	case err == nil:
		if prior.Status == tools.StatusOK {
			return 0, &tools.PriorRun{Status: tools.StatusOK, Summary: prior.ResultSummary}, nil
		}
		return 0, &tools.PriorRun{Status: tools.StatusUnknownOutcome}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, nil, err
	}
	id, err := s.insert(ctx, q, r, tools.StatusStarted, "")
	if err != nil {
		return 0, nil, err
	}
	return id, nil, tx.Commit(ctx)
}

// Finish completes a started run; rows already finished are left alone.
func (s *ToolRunStore) Finish(ctx context.Context, f tools.RunFinish) error {
	_, err := sqlc.New(s.pool).FinishToolRun(ctx, sqlc.FinishToolRunParams{
		ID: f.RunID, Status: f.Status, ErrorCode: optText(f.ErrorCode),
		DurationMs: pgtype.Int8{Int64: f.DurationMS, Valid: true}, OutputBytes: pgtype.Int4{Int32: int32(f.OutputBytes), Valid: true},
		ResultSummary: optText(f.Summary),
	})
	return err
}

// Deny records a policy denial.
func (s *ToolRunStore) Deny(ctx context.Context, r tools.RunStart, code string) error {
	_, err := s.insert(ctx, sqlc.New(s.pool), r, tools.StatusDenied, code)
	return err
}

// JobCounts returns the write and dangerous budget a job has used across
// all of its attempts.
func (s *ToolRunStore) JobCounts(ctx context.Context, jobID int64) (writes, dangerous int, err error) {
	row, err := sqlc.New(s.pool).CountJobBudgetRuns(ctx, jobID)
	if err != nil {
		return 0, 0, err
	}
	return int(row.Writes), int(row.Dangerous), nil
}
