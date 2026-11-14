package workflow

import (
	"context"
	"time"
)

// Usage is cumulative provider usage. Values never decrease.
type Usage struct {
	InputTokens int64

	OutputTokens int64

	CacheReadTokens int64

	CacheWriteTokens int64
}

// References are trusted identifiers for the source domain. They must not
// contain message bodies, prompts, model output, tool input or tool output.
type References struct {
	SourceKind string

	SourceID string

	Subject string

	Revision string

	OperationKey string

	LegacyRunID int64

	LegacyJobID int64
}

// CheckpointReference points to an existing result or side-effect receipt.
type CheckpointReference struct {
	Kind string `json:"kind"`

	ReferenceID string `json:"reference_id"`

	OperationKey string `json:"operation_key,omitempty"`
}

// Checkpoint is an allowlisted, bounded collection of durable references.
// Reusable must be true before a succeeded step can be copied to a retry child.
type Checkpoint struct {
	References []CheckpointReference `json:"references"`

	Reusable bool `json:"reusable"`
}

// Execution is one run pinned to an immutable published version.
type Execution struct {
	ID int64

	WorkspaceID string

	VersionID int64

	TriggerKey string

	CorrelationID string

	State State

	Reason ReasonCode

	Definition Definition

	References References

	ParentExecutionID int64

	Deadline *time.Time

	CancelledAt *time.Time

	StartedAt *time.Time

	FinishedAt *time.Time

	CreatedAt time.Time

	UpdatedAt time.Time
}

// Step is a persisted definition step and its cumulative progress.
type Step struct {
	ID int64

	ExecutionID int64

	Key string

	Position int

	Kind string

	DependsOn []string

	State StepState

	Attempts int

	Usage Usage

	Checkpoint *Checkpoint

	CreatedAt time.Time

	UpdatedAt time.Time
}

// Event is one ordered immutable history item.
type Event struct {
	ID int64

	ExecutionID int64

	Sequence int64

	Kind EventKind

	FromState State

	ToState State

	StepKey string

	Attempt int

	Reason ReasonCode

	IdempotencyKey string

	Usage Usage

	CreatedAt time.Time
}

// StartRequest contains only immutable configuration and trusted references.
type StartRequest struct {
	WorkspaceID string

	VersionID int64

	TriggerKey string

	CorrelationID string

	Definition Definition

	References References

	Deadline *time.Time
}

// Command identifies an idempotent operator command.
type Command struct {
	WorkspaceID string

	ExecutionID int64

	IdempotencyKey string
}

// Signal is an already-authorized durable decision or input reference.
type Signal struct {
	WorkspaceID string

	ExecutionID int64

	SignalID string

	BlockedStepKey string

	ReferenceKind string

	ReferenceID string

	IdempotencyKey string
}

// StepContext is the complete pinned input to a registered executor.
type StepContext struct {
	Execution Execution

	Definition Definition

	Step Step

	ClaimToken string

	Checkpoint *Checkpoint
}

// StepResult is either a successful checkpoint or a durable blocking wait.
type StepResult struct {
	Checkpoint *Checkpoint

	WaitState State

	WaitReferences []CheckpointReference
}

// TransitionRequest atomically changes state and appends history.
type TransitionRequest struct {
	WorkspaceID string

	ExecutionID int64

	From State

	To State

	StepKey string

	Attempt int

	Reason ReasonCode

	IdempotencyKey string
}

// CheckpointRequest atomically records one bounded checkpoint, usage and event.
type CheckpointRequest struct {
	WorkspaceID string

	ExecutionID int64

	StepKey string

	ExpectedAttempt int

	Checkpoint Checkpoint

	Usage Usage

	IdempotencyKey string
}

// Engine is the stable workflow command seam.
type Engine interface {
	Start(context.Context, StartRequest) (Execution, error)
	HandleSignal(context.Context, Signal) error
	Cancel(context.Context, Command) (Execution, error)
	Resume(context.Context, Command) (Execution, error)
	Retry(context.Context, Command) (Execution, error)
}

// StepExecutor runs one registered step without importing provider SDKs here.
type StepExecutor interface {
	Execute(context.Context, StepContext) (StepResult, error)
}
