package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/orchestrator"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var _ orchestrator.PlanStore = (*OrchestratorStore)(nil)

// OrchestratorStore keeps sanitized orchestrator plans, tasks and decisions.
type OrchestratorStore struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
}

func NewOrchestratorStore(pool *pgxpool.Pool) (*OrchestratorStore, error) {
	if pool == nil {
		return nil, errors.New("orchestrator store requires pool")
	}
	return &OrchestratorStore{pool: pool, q: sqlc.New(pool)}, nil
}

func (s *OrchestratorStore) rollback(tx pgx.Tx) {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(c)
}

func jsonOrDefault(raw json.RawMessage, def string) []byte {
	if len(raw) == 0 {
		return []byte(def)
	}
	return raw
}

func refsJSON(refs []string) []byte {
	if refs == nil {
		refs = []string{}
	}
	b, _ := json.Marshal(refs)
	return b
}

func refsFromJSON(raw []byte) []string {
	var refs []string
	_ = json.Unmarshal(raw, &refs)
	if refs == nil {
		refs = []string{}
	}
	return refs
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func mapOrchestratorPlan(r sqlc.OrchestratorPlan) orchestrator.PlanRecord {
	return orchestrator.PlanRecord{
		ID: r.ID, ExecutionID: r.ExecutionID, Version: int(r.Version), SchemaVersion: int(r.SchemaVersion),
		Intent: r.Intent, TaskType: r.TaskType, Priority: r.Priority, Complexity: r.Complexity,
		Confidence: r.Confidence, Budget: json.RawMessage(r.Budget), Status: r.Status,
		TokensIn: r.TokensIn, TokensOut: r.TokensOut, CostUSD: r.CostUsd, CreatedAt: r.CreatedAt.Time,
	}
}

func mapOrchestratorTask(r sqlc.OrchestratorPlanTask) orchestrator.PlanTaskRecord {
	return orchestrator.PlanTaskRecord{
		PlanID: r.PlanID, Key: r.Key, Title: r.Title, Capabilities: nonNil(r.Capabilities), DependsOn: nonNil(r.DependsOn),
		Priority: r.Priority, Complexity: r.Complexity, Executor: r.Executor, State: r.State,
		ResultStepID: r.ResultStepID, Result: json.RawMessage(r.Result), EvidenceRefs: refsFromJSON(r.EvidenceRefs),
		UpdatedAt: r.UpdatedAt.Time,
	}
}

func mapOrchestratorDecision(r sqlc.OrchestratorDecision) orchestrator.DecisionRecord {
	return orchestrator.DecisionRecord{
		ID: r.ID, ExecutionID: r.ExecutionID, PlanVersion: int(r.PlanVersion), TaskKey: r.TaskKey.String,
		Decision: r.Decision, ReasonCode: r.ReasonCode, ConvertedFrom: r.ConvertedFrom.String,
		EvidenceRefs: refsFromJSON(r.EvidenceRefs), CreatedAt: r.CreatedAt.Time,
	}
}

// InsertPlan stores the plan and all its tasks in one transaction.
func (s *OrchestratorStore) InsertPlan(ctx context.Context, workspaceID string, plan orchestrator.PlanRecord, tasks []orchestrator.PlanTaskRecord) (orchestrator.PlanRecord, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return orchestrator.PlanRecord{}, invalid("workspace_id")
	}
	if plan.ExecutionID <= 0 || plan.Version < 1 || len(tasks) == 0 || len(tasks) > orchestrator.MaxPlanTasks {
		return orchestrator.PlanRecord{}, invalid("plan")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return orchestrator.PlanRecord{}, err
	}
	defer s.rollback(tx)
	q := s.q.WithTx(tx)
	row, err := q.InsertOrchestratorPlan(ctx, sqlc.InsertOrchestratorPlanParams{
		WorkspaceID: ws, ExecutionID: plan.ExecutionID, Version: int32(plan.Version), SchemaVersion: int32(max(plan.SchemaVersion, 1)),
		Intent: plan.Intent, TaskType: plan.TaskType, Priority: defaultStr(plan.Priority, "normal"),
		Complexity: defaultStr(plan.Complexity, "m"), Confidence: plan.Confidence, Budget: jsonOrDefault(plan.Budget, "{}"),
		TokensIn: plan.TokensIn, TokensOut: plan.TokensOut, CostUsd: plan.CostUSD,
	})
	if err != nil {
		return orchestrator.PlanRecord{}, topicError(err)
	}
	for _, t := range tasks {
		if _, err := q.InsertOrchestratorPlanTask(ctx, sqlc.InsertOrchestratorPlanTaskParams{
			WorkspaceID: ws, PlanID: row.ID, Key: t.Key, Title: t.Title,
			Capabilities: nonNil(t.Capabilities), DependsOn: nonNil(t.DependsOn),
			Priority: defaultStr(t.Priority, "normal"), Complexity: defaultStr(t.Complexity, "m"), Executor: t.Executor,
		}); err != nil {
			return orchestrator.PlanRecord{}, topicError(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return orchestrator.PlanRecord{}, err
	}
	return mapOrchestratorPlan(row), nil
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// SetPlanStatus changes only the status of a plan version.
func (s *OrchestratorStore) SetPlanStatus(ctx context.Context, workspaceID string, executionID int64, version int, status string) error {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return invalid("workspace_id")
	}
	_, err = s.q.SetOrchestratorPlanStatus(ctx, sqlc.SetOrchestratorPlanStatusParams{
		WorkspaceID: ws, ExecutionID: executionID, Version: int32(version), Status: status,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return invalid("plan")
	}
	return topicError(err)
}

// UpdateTask sets a task's state, executor, result and evidence.
func (s *OrchestratorStore) UpdateTask(ctx context.Context, workspaceID string, t orchestrator.PlanTaskRecord) error {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return invalid("workspace_id")
	}
	_, err = s.q.UpdateOrchestratorPlanTask(ctx, sqlc.UpdateOrchestratorPlanTaskParams{
		WorkspaceID: ws, PlanID: t.PlanID, Key: t.Key, State: t.State, Executor: t.Executor,
		ResultStepID: t.ResultStepID, Result: []byte(t.Result), EvidenceRefs: refsJSON(t.EvidenceRefs),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return invalid("task")
	}
	return topicError(err)
}

// InsertDecision records one decision (including a Go conversion).
func (s *OrchestratorStore) InsertDecision(ctx context.Context, workspaceID string, d orchestrator.DecisionRecord) (orchestrator.DecisionRecord, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return orchestrator.DecisionRecord{}, invalid("workspace_id")
	}
	row, err := s.q.InsertOrchestratorDecision(ctx, sqlc.InsertOrchestratorDecisionParams{
		WorkspaceID: ws, ExecutionID: d.ExecutionID, PlanVersion: int32(d.PlanVersion), TaskKey: optText(d.TaskKey),
		Decision: d.Decision, ReasonCode: d.ReasonCode, ConvertedFrom: optText(d.ConvertedFrom), EvidenceRefs: refsJSON(d.EvidenceRefs),
	})
	if err != nil {
		return orchestrator.DecisionRecord{}, topicError(err)
	}
	return mapOrchestratorDecision(row), nil
}

func (s *OrchestratorStore) ListPlans(ctx context.Context, workspaceID string, executionID int64) ([]orchestrator.PlanRecord, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, invalid("workspace_id")
	}
	rows, err := s.q.ListOrchestratorPlans(ctx, sqlc.ListOrchestratorPlansParams{WorkspaceID: ws, ExecutionID: executionID})
	if err != nil {
		return nil, err
	}
	out := make([]orchestrator.PlanRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapOrchestratorPlan(r))
	}
	return out, nil
}

// ListTasks returns the tasks of every plan version of an execution, by version.
func (s *OrchestratorStore) ListTasks(ctx context.Context, workspaceID string, executionID int64) ([]orchestrator.PlanTaskRecord, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, invalid("workspace_id")
	}
	rows, err := s.q.ListOrchestratorPlanTasks(ctx, sqlc.ListOrchestratorPlanTasksParams{WorkspaceID: ws, ExecutionID: executionID})
	if err != nil {
		return nil, err
	}
	out := make([]orchestrator.PlanTaskRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapOrchestratorTask(r))
	}
	return out, nil
}

func (s *OrchestratorStore) ListDecisions(ctx context.Context, workspaceID string, executionID int64) ([]orchestrator.DecisionRecord, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, invalid("workspace_id")
	}
	rows, err := s.q.ListOrchestratorDecisions(ctx, sqlc.ListOrchestratorDecisionsParams{WorkspaceID: ws, ExecutionID: executionID})
	if err != nil {
		return nil, err
	}
	out := make([]orchestrator.DecisionRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapOrchestratorDecision(r))
	}
	return out, nil
}
