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
