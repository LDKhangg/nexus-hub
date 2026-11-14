package workflow

import "errors"

var ErrInvalidInput = errors.New("workflow: invalid input")
var ErrNotFound = errors.New("workflow: not found")
var ErrConflict = errors.New("workflow: conflict")
var ErrStaleClaim = errors.New("workflow: stale claim")
var ErrUnsafeRetry = errors.New("workflow: unsafe retry")
var ErrProviderUncertainty = errors.New("workflow: provider outcome uncertain")
var ErrNotEligible = errors.New("workflow: command not eligible")

// State is the persisted execution lifecycle vocabulary.
type State string

const StateReceived State = "RECEIVED"
const StateTriaging State = "TRIAGING"
const StatePlanning State = "PLANNING"
const StateQueued State = "QUEUED"
const StateRunning State = "RUNNING"
const StateEvaluating State = "EVALUATING"
const StateWaitingForUser State = "WAITING_FOR_USER"
const StateWaitingForApproval State = "WAITING_FOR_APPROVAL"
const StateCompleted State = "COMPLETED"
const StateFailed State = "FAILED"
const StateCancelled State = "CANCELLED"

// StepState is the persisted state of one definition step.
type StepState string

const StepPending StepState = "PENDING"
const StepRunning StepState = "RUNNING"
const StepWaiting StepState = "WAITING"
const StepSucceeded StepState = "SUCCEEDED"
const StepFailed StepState = "FAILED"
const StepCancelled StepState = "CANCELLED"

// ReasonCode is a sanitized machine-readable lifecycle reason.
type ReasonCode string

const ReasonAccepted ReasonCode = "accepted"
const ReasonCancelled ReasonCode = "cancelled"
const ReasonDeadlineExceeded ReasonCode = "deadline_exceeded"
const ReasonAttemptsExhausted ReasonCode = "attempts_exhausted"
const ReasonNonRetryable ReasonCode = "non_retryable"
const ReasonSideEffectUncertain ReasonCode = "side_effect_uncertain"
const ReasonManualRetry ReasonCode = "manual_retry"
const ReasonCheckpoint ReasonCode = "checkpoint"

// EventKind identifies an immutable execution-history record.
type EventKind string

const EventStarted EventKind = "started"
const EventTransition EventKind = "transition"
const EventCheckpoint EventKind = "checkpoint"
const EventRetryChild EventKind = "retry_child"
