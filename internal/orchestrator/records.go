package orchestrator

import (
	"context"
	"encoding/json"
	"time"
)

// Plan statuses; a plan row is immutable except for its status.
const (
	PlanActive     = "ACTIVE"
	PlanSuperseded = "SUPERSEDED"
	PlanCompleted  = "COMPLETED"
	PlanFailed     = "FAILED"
	PlanCancelled  = "CANCELLED"
)

// Plan task states.
const (
	TaskPending   = "PENDING"
	TaskReady     = "READY"
	TaskRunning   = "RUNNING"
	TaskWaiting   = "WAITING"
	TaskSucceeded = "SUCCEEDED"
	TaskFailed    = "FAILED"
	TaskBlocked   = "BLOCKED"
	TaskSkipped   = "SKIPPED"
)

// PlanRecord is a stored plan version: enums, counters and usage only.
type PlanRecord struct {
	ID            int64
	ExecutionID   int64
	Version       int
	SchemaVersion int
	Intent        string
	TaskType      string
	Priority      string
	Complexity    string
	Confidence    float64
	Budget        json.RawMessage
	Status        string
	TokensIn      int64
	TokensOut     int64
	CostUSD       float64
	CreatedAt     time.Time
}

// PlanTaskRecord is a stored plan task. Title is a sanitized short title.
type PlanTaskRecord struct {
	PlanID       int64
	Key          string
	Title        string
	Capabilities []string
	DependsOn    []string
	Priority     string
	Complexity   string
	Executor     string
	State        string
	ResultStepID string
	Result       json.RawMessage
	EvidenceRefs []string
	UpdatedAt    time.Time
}

// DecisionRecord audits one decision, including Go conversions.
type DecisionRecord struct {
	ID            int64
	ExecutionID   int64
	PlanVersion   int
	TaskKey       string
	Decision      string
	ReasonCode    string
	ConvertedFrom string
	EvidenceRefs  []string
	CreatedAt     time.Time
}

// PlanStore persists plans, plan tasks and decisions (workspace-scoped).
type PlanStore interface {
	// InsertPlan stores a plan and its tasks in one transaction.
	InsertPlan(ctx context.Context, workspaceID string, plan PlanRecord, tasks []PlanTaskRecord) (PlanRecord, error)
	SetPlanStatus(ctx context.Context, workspaceID string, executionID int64, version int, status string) error
	UpdateTask(ctx context.Context, workspaceID string, task PlanTaskRecord) error
	InsertDecision(ctx context.Context, workspaceID string, d DecisionRecord) (DecisionRecord, error)
	ListPlans(ctx context.Context, workspaceID string, executionID int64) ([]PlanRecord, error)
	ListTasks(ctx context.Context, workspaceID string, executionID int64) ([]PlanTaskRecord, error)
	ListDecisions(ctx context.Context, workspaceID string, executionID int64) ([]DecisionRecord, error)
}
