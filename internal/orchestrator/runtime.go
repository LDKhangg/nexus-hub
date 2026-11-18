// Package orchestrator holds the provider-neutral AI orchestration domain.
// It imports no provider package (ARCH-004): models are reached only through
// AIRuntime, which wiring backs with a concrete provider.
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// DefaultMaxRepair is the default number of repair attempts after invalid
// output (setting orchestrator_max_repair, wired in a later task).
const DefaultMaxRepair = 2

// SettingMaxRepair names the setting that bounds repair attempts.
const SettingMaxRepair = "orchestrator_max_repair"

// ReasonInvalidOutput is the fixed reason code when output stays invalid.
const ReasonInvalidOutput = "invalid_output"

// ErrInvalidOutput means the model output did not match its schema within
// the repair bound. Nothing from the bad output is kept.
var ErrInvalidOutput = errors.New("orchestrator: invalid_output")

// StructuredRequest asks for one JSON value matching Schema. SchemaName and
// SchemaVersion identify the registered schema (and key the fake runtime).
type StructuredRequest struct {
	Model, System, Prompt string
	SchemaName            string
	SchemaVersion         int
	Schema                json.RawMessage
}

// StructuredResponse is the JSON value and the call's usage.
type StructuredResponse struct {
	JSON    json.RawMessage
	Usage   domain.Usage
	CostUSD float64
}

// AIRuntime is the only way orchestrator code reaches a model.
type AIRuntime interface {
	Generate(ctx context.Context, req StructuredRequest) (StructuredResponse, error)
}

// Decision values (FR-051); the same set as Decisions.
const (
	DecisionExecute              = "EXECUTE"
	DecisionDelegate             = "DELEGATE"
	DecisionRequestClarification = "REQUEST_CLARIFICATION"
	DecisionRequestApproval      = "REQUEST_APPROVAL"
	DecisionReplan               = "REPLAN"
	DecisionComplete             = "COMPLETE"
	DecisionFail                 = "FAIL"
)

// DefaultMinConfidence is the default Classify confidence threshold below
// which the caller should ask for clarification.
const DefaultMinConfidence = 0.6

// SettingMinConfidence names the setting that holds MinConfidence.
const SettingMinConfidence = "orchestrator_min_confidence"

// ErrInvalidInput means the caller passed input the orchestrator cannot use.
// No model call is made.
var ErrInvalidInput = errors.New("orchestrator: invalid input")

// Usage is what one Classify, Plan or Evaluate call spent, summed over every
// attempt including repair attempts. It is returned even on error.
type Usage struct {
	Tokens   domain.Usage
	CostUSD  float64
	Attempts int
}

func usageOf(v Validated) Usage {
	return Usage{Tokens: v.Usage, CostUSD: v.CostUSD, Attempts: v.Attempts}
}

// Orchestrator proposes classifications, plans and decisions. Its methods
// are pure: input structs in, validated output structs out. It has no DB
// access and causes no side effect other than model calls through Runtime.
// Build it with NewOrchestrator so defaults are set.
type Orchestrator struct {
	Runtime AIRuntime
	// Model names passed to the runtime per operation; the runtime maps
	// them to a provider. Empty means the runtime default.
	ClassifyModel, PlanModel, EvaluateModel string
	// MaxRepair bounds repair attempts after invalid output (negative is 0).
	MaxRepair int
	// MinConfidence is the Classify threshold; values outside (0, 1] use
	// DefaultMinConfidence.
	MinConfidence float64
	// Intents and TaskTypes are the Classify vocabularies; empty uses
	// DefaultIntents and DefaultTaskTypes.
	Intents, TaskTypes []string
	// BeforeAttempt, when set, runs before every repair attempt with the
	// usage spent so far in this call. A non-nil error stops the call and is
	// returned with that usage (for example a budget or wall-time guard).
	BeforeAttempt func(spent Usage) error
}

// NewOrchestrator returns an Orchestrator that uses model for every
// operation, DefaultMaxRepair and DefaultMinConfidence.
func NewOrchestrator(rt AIRuntime, model string) *Orchestrator {
	return &Orchestrator{
		Runtime:       rt,
		ClassifyModel: model,
		PlanModel:     model,
		EvaluateModel: model,
		MaxRepair:     DefaultMaxRepair,
		MinConfidence: DefaultMinConfidence,
	}
}

func (o *Orchestrator) ready() error {
	if o == nil || o.Runtime == nil {
		return errors.New("orchestrator: runtime is required")
	}
	return nil
}

func (o *Orchestrator) maxRepair() int {
	if o.MaxRepair < 0 {
		return 0
	}
	return o.MaxRepair
}

func (o *Orchestrator) minConfidence() float64 {
	c := o.MinConfidence
	if !(c > 0 && c <= 1) { // also rejects NaN
		return DefaultMinConfidence
	}
	return c
}
