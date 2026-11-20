package storage

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
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

var _ behavior.WorkflowAlertStore = (*OpsStore)(nil)

// ListAlertCandidates returns workflow executions in an alert state that
// have no alert row yet, oldest first (Phase 12 REQ-10).
func (s *OpsStore) ListAlertCandidates(ctx context.Context, q behavior.AlertQuery) ([]behavior.WorkflowAlert, error) {
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.q.ListWorkflowAlertCandidates(ctx, sqlc.ListWorkflowAlertCandidatesParams{
		RunningBefore: ts(q.RunningBefore), QueuedBefore: ts(q.QueuedBefore),
		ApprovalBefore: ts(q.ApprovalBefore), FailedSince: ts(q.FailedSince), RowLimit: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]behavior.WorkflowAlert, 0, len(rows))
	for _, r := range rows {
		out = append(out, behavior.WorkflowAlert{
			WorkspaceID: uuidString(r.WorkspaceID), ExecutionID: r.ExecutionID, State: behavior.AlertState(r.AlertState),
			BehaviorName: r.BehaviorName, Since: r.SinceAt.Time, ErrorCode: r.ErrorCode,
		})
	}
	return out, nil
}

// ClaimAlert inserts the pending row of (execution, state), due again at
// at+AlertClaimLease; claimed is false when the alert was claimed before.
func (s *OpsStore) ClaimAlert(ctx context.Context, a behavior.WorkflowAlert, at time.Time) (bool, error) {
	return s.claimAlert(ctx, a, "pending", at)
}

// SuppressAlert inserts the row of (execution, state) as suppressed by the
// hourly cap; claimed is false when the alert was claimed before.
func (s *OpsStore) SuppressAlert(ctx context.Context, a behavior.WorkflowAlert, at time.Time) (bool, error) {
	return s.claimAlert(ctx, a, "suppressed", at)
}

func (s *OpsStore) claimAlert(ctx context.Context, a behavior.WorkflowAlert, status string, at time.Time) (bool, error) {
	ws, err := parseDBID(a.WorkspaceID)
	if err != nil {
		return false, err
	}
	var since pgtype.Timestamptz
	if !a.Since.IsZero() {
		since = ts(a.Since)
	}
	var code pgtype.Text
	if alertCodeShape.MatchString(a.ErrorCode) {
		code = pgtype.Text{String: a.ErrorCode, Valid: true}
	}
	n, err := s.q.ClaimWorkflowAlert(ctx, sqlc.ClaimWorkflowAlertParams{
		WorkspaceID: ws, ExecutionID: a.ExecutionID, State: string(a.State), Status: status,
		NextAttemptAt: ts(at.Add(behavior.AlertClaimLease)), SinceAt: since, ErrorCode: code, CreatedAt: ts(at),
	})
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

var alertCodeShape = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// MarkAlertSent marks a pending alert delivered at at.
func (s *OpsStore) MarkAlertSent(ctx context.Context, a behavior.WorkflowAlert, at time.Time) error {
	ws, err := parseDBID(a.WorkspaceID)
	if err != nil {
		return err
	}
	_, err = s.q.MarkWorkflowAlertSent(ctx, sqlc.MarkWorkflowAlertSentParams{WorkspaceID: ws, ExecutionID: a.ExecutionID, State: string(a.State), SentAt: ts(at)})
	return err
}

// MarkAlertAttemptFailed counts one failed attempt of a pending alert made
// after a.Attempts attempts: due again at next, or failed when next is
// zero. A row already moved on (another attempt recorded) is left as is.
func (s *OpsStore) MarkAlertAttemptFailed(ctx context.Context, a behavior.WorkflowAlert, code string, next time.Time) error {
	ws, err := parseDBID(a.WorkspaceID)
	if err != nil {
		return err
	}
	if !alertCodeShape.MatchString(code) {
		code = "send_failed"
	}
	var nextAt pgtype.Timestamptz
	if !next.IsZero() {
		nextAt = ts(next)
	}
	_, err = s.q.MarkWorkflowAlertAttemptFailed(ctx, sqlc.MarkWorkflowAlertAttemptFailedParams{
		NextAttemptAt: nextAt, LastErrorCode: pgtype.Text{String: code, Valid: true},
		WorkspaceID: ws, ExecutionID: a.ExecutionID, State: string(a.State), PrevAttempts: int32(a.Attempts),
	})
	return err
}

// ListAlertRetries returns pending alerts due at or before due, oldest
// first, at most limit (1..100).
func (s *OpsStore) ListAlertRetries(ctx context.Context, due time.Time, limit int) ([]behavior.WorkflowAlert, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.q.ListDueWorkflowAlertRetries(ctx, sqlc.ListDueWorkflowAlertRetriesParams{DueAt: ts(due), RowLimit: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]behavior.WorkflowAlert, 0, len(rows))
	for _, r := range rows {
		out = append(out, behavior.WorkflowAlert{
			WorkspaceID: uuidString(r.WorkspaceID), ExecutionID: r.ExecutionID, State: behavior.AlertState(r.State),
			BehaviorName: r.BehaviorName, Since: r.SinceAt.Time, ErrorCode: r.ErrorCode, Attempts: int(r.Attempts), Obsolete: r.Obsolete,
		})
	}
	return out, nil
}

// MarkAlertObsolete closes a pending stuck or overdue alert whose
// condition ended, without sending it.
func (s *OpsStore) MarkAlertObsolete(ctx context.Context, a behavior.WorkflowAlert) error {
	ws, err := parseDBID(a.WorkspaceID)
	if err != nil {
		return err
	}
	_, err = s.q.MarkWorkflowAlertObsolete(ctx, sqlc.MarkWorkflowAlertObsoleteParams{WorkspaceID: ws, ExecutionID: a.ExecutionID, State: string(a.State)})
	return err
}

// CountAlertsSentSince counts alerts delivered since since, hub-wide.
func (s *OpsStore) CountAlertsSentSince(ctx context.Context, since time.Time) (int, error) {
	n, err := s.q.CountWorkflowAlertsSentSince(ctx, ts(since))
	return int(n), err
}

// ListAlertSummaries returns, per workspace, the suppressed alerts claimed
// before before and not yet summarised.
func (s *OpsStore) ListAlertSummaries(ctx context.Context, before time.Time) ([]behavior.AlertSummary, error) {
	rows, err := s.q.ListUnsummarisedWorkflowAlerts(ctx, ts(before))
	if err != nil {
		return nil, err
	}
	out := make([]behavior.AlertSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, behavior.AlertSummary{WorkspaceID: uuidString(r.WorkspaceID), Count: int(r.N), OldestAt: r.OldestAt.Time})
	}
	return out, nil
}

// MarkAlertsSummarised marks a workspace's suppressed alerts claimed
// before before as summarised at at.
func (s *OpsStore) MarkAlertsSummarised(ctx context.Context, workspaceID string, before, at time.Time) error {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return err
	}
	_, err = s.q.MarkWorkflowAlertsSummarised(ctx, sqlc.MarkWorkflowAlertsSummarisedParams{SummarisedAt: ts(at), WorkspaceID: ws, Before: ts(before)})
	return err
}

// ReleaseAlert deletes the row of an alert (operator recovery); the next
// scan may alert again. The watcher never calls it.
func (s *OpsStore) ReleaseAlert(ctx context.Context, a behavior.WorkflowAlert) error {
	ws, err := parseDBID(a.WorkspaceID)
	if err != nil {
		return err
	}
	_, err = s.q.ReleaseWorkflowAlert(ctx, sqlc.ReleaseWorkflowAlertParams{WorkspaceID: ws, ExecutionID: a.ExecutionID, State: string(a.State)})
	return err
}

// OpenAlertCount counts open workflow alerts, hub-wide when workspaceID is
// "": stuck or overdue alerts of executions that have not ended, and failed
// alerts claimed since failedSince.
func (s *OpsStore) OpenAlertCount(ctx context.Context, workspaceID string, failedSince time.Time) (int64, error) {
	ws, err := optionalWorkspace(workspaceID)
	if err != nil {
		return 0, err
	}
	return s.q.CountOpenWorkflowAlerts(ctx, sqlc.CountOpenWorkflowAlertsParams{WorkspaceID: ws, FailedSince: ts(failedSince)})
}

// CodeCount is one safe code and its count.
type CodeCount struct {
	Code string
	N    int64
}

// OpsErrors is the error view of nh status: codes and counts only.
type OpsErrors struct {
	// FailedExecutions counts executions FAILED since the given time by
	// reason code ("" when none).
	FailedExecutions []CodeCount
	// DeadLetterJobs counts dead-letter workflow jobs by reason code.
	DeadLetterJobs []CodeCount
	// FailedAlerts counts alerts whose send attempts ran out.
	FailedAlerts int64
}

// Errors reads the error view, hub-wide when workspaceID is "".
func (s *OpsStore) Errors(ctx context.Context, workspaceID string, failedSince time.Time) (OpsErrors, error) {
	ws, err := optionalWorkspace(workspaceID)
	if err != nil {
		return OpsErrors{}, err
	}
	var out OpsErrors
	failed, err := s.q.OpsFailedExecutionReasons(ctx, sqlc.OpsFailedExecutionReasonsParams{Since: ts(failedSince), WorkspaceID: ws})
	if err != nil {
		return OpsErrors{}, err
	}
	for _, r := range failed {
		out.FailedExecutions = append(out.FailedExecutions, CodeCount{Code: r.ReasonCode, N: r.N})
	}
	dead, err := s.q.OpsDeadLetterReasons(ctx, ws)
	if err != nil {
		return OpsErrors{}, err
	}
	for _, r := range dead {
		out.DeadLetterJobs = append(out.DeadLetterJobs, CodeCount{Code: r.ReasonCode, N: r.N})
	}
	if out.FailedAlerts, err = s.q.CountFailedWorkflowAlerts(ctx, ws); err != nil {
		return OpsErrors{}, err
	}
	return out, nil
}
