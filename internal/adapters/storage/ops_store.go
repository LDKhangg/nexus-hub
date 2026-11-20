package storage

import (
	"context"
	"errors"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OpsStore serves the Phase 12 operations reads (/metrics, nh status) and
// the LLM spend ledger. Every value is a count, a state, an age or a code.
type OpsStore struct {
	q *sqlc.Queries
}

// NewOpsStore wraps pool.
func NewOpsStore(pool *pgxpool.Pool) (*OpsStore, error) {
	if pool == nil {
		return nil, errors.New("ops store requires pool")
	}
	return &OpsStore{q: sqlc.New(pool)}, nil
}

// LLMUsage is one provider and model's tokens and estimated cost of a day.
type LLMUsage struct {
	Provider     string
	Model        string
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
}

// OpsSnapshot is the operations view of the hub (or of one workspace).
type OpsSnapshot struct {
	// ExecutionsByState counts workflow executions per state.
	ExecutionsByState map[string]int64
	// QueuePending counts workflow jobs waiting for a claim.
	QueuePending int64
	// OldestDueAge is the wait of the oldest due pending job (0: none).
	OldestDueAge time.Duration
	// WorkerDevices counts enrolled, not revoked devices.
	WorkerDevices int64
	// WorkersOnline counts devices in state online or busy.
	WorkersOnline int64
	// NewestHeartbeatAge is the age of the newest device heartbeat; -1
	// when no device ever reported one.
	NewestHeartbeatAge time.Duration
	// Day is the UTC day of LLMUsage.
	Day time.Time
	// LLMUsage is the day's ledger by provider and model.
	LLMUsage []LLMUsage
}

// optionalWorkspace parses a workspace id; "" means hub-wide.
func optionalWorkspace(workspaceID string) (pgtype.UUID, error) {
	if workspaceID == "" {
		return pgtype.UUID{}, nil
	}
	return parseDBID(workspaceID)
}

// utcDay is t's UTC calendar date.
func utcDay(t time.Time) pgtype.Date {
	y, m, d := t.UTC().Date()
	return pgtype.Date{Time: time.Date(y, m, d, 0, 0, 0, 0, time.UTC), Valid: true}
}

func secondsDuration(s float64) time.Duration {
	if s < 0 {
		return -1
	}
	return time.Duration(s * float64(time.Second))
}

// Snapshot reads the operations numbers, hub-wide when workspaceID is "".
// day selects the LLM ledger day (UTC).
func (s *OpsStore) Snapshot(ctx context.Context, workspaceID string, day time.Time) (OpsSnapshot, error) {
	ws, err := optionalWorkspace(workspaceID)
	if err != nil {
		return OpsSnapshot{}, err
	}
	out := OpsSnapshot{ExecutionsByState: map[string]int64{}, Day: utcDay(day).Time}
	states, err := s.q.OpsExecutionStateCounts(ctx, ws)
	if err != nil {
		return OpsSnapshot{}, err
	}
	for _, r := range states {
		out.ExecutionsByState[r.State] = r.N
	}
	queue, err := s.q.OpsQueueStats(ctx, ws)
	if err != nil {
		return OpsSnapshot{}, err
	}
	out.QueuePending, out.OldestDueAge = queue.Pending, secondsDuration(queue.OldestDueAgeS)
	workers, err := s.q.OpsWorkerStats(ctx, ws)
	if err != nil {
		return OpsSnapshot{}, err
	}
	out.WorkerDevices, out.WorkersOnline, out.NewestHeartbeatAge = workers.Devices, workers.Online, secondsDuration(workers.NewestHeartbeatAgeS)
	usage, err := s.LLMUsageForDay(ctx, workspaceID, day)
	if err != nil {
		return OpsSnapshot{}, err
	}
	out.LLMUsage = usage
	return out, nil
}

// LLMUsageForDay lists one UTC day's tokens and cost by provider and model,
// hub-wide when workspaceID is "".
func (s *OpsStore) LLMUsageForDay(ctx context.Context, workspaceID string, day time.Time) ([]LLMUsage, error) {
	ws, err := optionalWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListLLMUsageForDay(ctx, sqlc.ListLLMUsageForDayParams{Day: utcDay(day), WorkspaceID: ws})
	if err != nil {
		return nil, err
	}
	out := make([]LLMUsage, 0, len(rows))
	for _, r := range rows {
		out = append(out, LLMUsage{Provider: r.Provider, Model: r.Model, InputTokens: r.InputTokens, OutputTokens: r.OutputTokens, CostUSD: r.CostUsd})
	}
	return out, nil
}

// AddLLMUsage adds one call to the workspace's ledger row of its day.
func (s *OpsStore) AddLLMUsage(ctx context.Context, r domain.LLMUsageRecord) error {
	ws, err := parseDBID(r.WorkspaceID)
	if err != nil {
		return err
	}
	if r.InputTokens < 0 || r.OutputTokens < 0 || r.CostUSD < 0 {
		return errors.New("llm usage must not be negative")
	}
	return s.q.UpsertLLMUsageDaily(ctx, sqlc.UpsertLLMUsageDailyParams{
		WorkspaceID: ws, Day: utcDay(r.Day), Provider: r.Provider, Model: r.Model,
		InputTokens: r.InputTokens, OutputTokens: r.OutputTokens, CostUsd: r.CostUSD,
	})
}

// LLMCostForDay is the workspace's estimated model spend of one UTC day.
func (s *OpsStore) LLMCostForDay(ctx context.Context, workspaceID string, day time.Time) (float64, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return 0, err
	}
	return s.q.SumLLMCostForDay(ctx, sqlc.SumLLMCostForDayParams{WorkspaceID: ws, Day: utcDay(day)})
}
