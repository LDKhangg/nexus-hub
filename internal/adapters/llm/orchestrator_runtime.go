package llm

import (
	"context"
	"errors"

	"github.com/LDKhangg/nexus-hub/internal/orchestrator"
)

// providerRuntime backs orchestrator.AIRuntime with a StructuredProvider, so
// the orchestrator domain never imports a provider package.
type providerRuntime struct{ p StructuredProvider }

var _ orchestrator.AIRuntime = providerRuntime{}

// FromProvider adapts a structured provider to orchestrator.AIRuntime.
func FromProvider(p StructuredProvider) orchestrator.AIRuntime { return providerRuntime{p: p} }

func (r providerRuntime) Generate(ctx context.Context, req orchestrator.StructuredRequest) (orchestrator.StructuredResponse, error) {
	if r.p == nil {
		return orchestrator.StructuredResponse{}, errors.New("llm: no structured provider")
	}
	resp, err := r.p.GenerateStructured(ctx, &StructuredRequest{
		Model: req.Model, System: req.System, Prompt: req.Prompt, Schema: req.Schema,
	})
	if err != nil {
		return orchestrator.StructuredResponse{}, err
	}
	if resp == nil {
		return orchestrator.StructuredResponse{}, errors.New("llm: empty structured response")
	}
	return orchestrator.StructuredResponse{JSON: resp.JSON, Usage: resp.Usage, CostUSD: resp.CostUSD}, nil
}
