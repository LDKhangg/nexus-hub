package workflow

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

// Operator read limits shared by the CLI and the Telegram assistant tools.
const (
	DefaultListLimit = 50
	MaxListLimit     = 200
)

// Job statuses of the generic queue.
const (
	JobPending    = "pending"
	JobProcessing = "processing"
	JobCompleted  = "completed"
	JobDeadLetter = "dead_letter"
)

// ValidJobStatus reports whether s is a queue status.
func ValidJobStatus(s string) bool {
	switch s {
	case JobPending, JobProcessing, JobCompleted, JobDeadLetter:
		return true
	}
	return false
}

// ExecutionView is an execution with the name and number of its pinned
// version. It carries identifiers, states and reason codes only.
type ExecutionView struct {
	Execution

	BehaviorName string

	BehaviorVersion int
}

// JobView is one queue row: identifiers, status, counters and codes.
type JobView struct {
	ID int64

	ExecutionID int64

	StepKey string

	Status string

	DueAt time.Time

	Attempts int

	MaxAttempts int

	LeaseExpiresAt *time.Time

	LastErrorCode string

	DeadLetterReason string

	CreatedAt time.Time

	UpdatedAt time.Time
}

// WaitView is one blocking wait of an execution step and the state of its
// bound approval (decision code only; never its payload or answer).
type WaitView struct {
	ID int64

	StepKey string

	ApprovalID int64

	Kind string

	Decision string

	DecidedAt *time.Time

	ExpiresAt *time.Time

	ClosedAt *time.Time

	Signalled bool

	SignalConsumedAt *time.Time

	CreatedAt time.Time
}

// ExecutionDetail is what an operator needs to judge one execution.
type ExecutionDetail struct {
	ExecutionView

	// RetryChildID is the direct retry child, 0 when there is none.
	RetryChildID int64

	// LiveJob is the pending or processing job, nil when there is none.
	LiveJob *JobView

	Waits []WaitView
}

// ExecutionQuery selects a page of executions, newest first.
type ExecutionQuery struct {
	WorkspaceID string

	// State filters by state when set.
	State State

	// BeforeID continues a page: only executions with a smaller ID.
	BeforeID int64

	Limit int
}

// JobQuery selects a page of queue rows, newest first.
type JobQuery struct {
	WorkspaceID string

	// Status filters by queue status when set.
	Status string

	// ExecutionID filters by execution when set.
	ExecutionID int64

	BeforeID int64

	Limit int
}

// Reader is the workspace-scoped read side of the engine. Every method
// returns ErrNotFound for an execution of another workspace. A page method
// returns the cursor value of the next page, 0 when there is none.
type Reader interface {
	ListExecutions(ctx context.Context, q ExecutionQuery) ([]ExecutionView, int64, error)
	GetExecutionDetail(ctx context.Context, workspaceID string, executionID int64) (ExecutionDetail, error)
	ListSteps(ctx context.Context, workspaceID string, executionID int64) ([]Step, error)
	ListEvents(ctx context.Context, workspaceID string, executionID, afterSequence int64, limit int) ([]Event, int64, error)
	ListJobs(ctx context.Context, q JobQuery) ([]JobView, int64, error)
}

// ValidListLimit reports whether n is an accepted page size.
func ValidListLimit(n int) bool { return n >= 1 && n <= MaxListLimit }

const cursorVersion = "c1"

// EncodeCursor returns an opaque page cursor bound to a list scope (for
// example "executions" or "events:42"). It holds no stored data.
func EncodeCursor(scope string, value int64) string {
	if value <= 0 {
		return ""
	}
	raw := cursorVersion + "|" + scope + "|" + strconv.FormatInt(value, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor returns the value of a cursor made by EncodeCursor for the
// same scope. An empty cursor is 0; a malformed or foreign one is
// ErrInvalidInput.
func DecodeCursor(scope, cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}
	if len(cursor) > 256 {
		return 0, ErrInvalidInput
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, ErrInvalidInput
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 || parts[0] != cursorVersion || parts[1] != scope {
		return 0, ErrInvalidInput
	}
	value, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || value <= 0 {
		return 0, ErrInvalidInput
	}
	return value, nil
}
