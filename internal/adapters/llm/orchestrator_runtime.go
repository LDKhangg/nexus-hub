package llm

import (
	"context"
	"errors"

	"github.com/LDKhangg/nexus-hub/internal/domain"
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

// orchestratorMaxTokens bounds one orchestrator answer of an API provider.
const orchestratorMaxTokens = 4096

// jsonRuntime backs orchestrator.AIRuntime with an API provider's native
// structured-output call.
type jsonRuntime struct{ p domain.JSONProvider }

func (r jsonRuntime) Generate(ctx context.Context, req orchestrator.StructuredRequest) (orchestrator.StructuredResponse, error) {
	resp, err := r.p.GenerateJSON(ctx, &domain.JSONRequest{
		Model: req.Model, System: req.System, MaxTokens: orchestratorMaxTokens, Schema: req.Schema,
		Messages: []domain.Message{{Role: domain.UserRole, Text: req.Prompt}},
	})
	if err != nil {
		return orchestrator.StructuredResponse{}, err
	}
	if resp == nil {
		return orchestrator.StructuredResponse{}, errors.New("llm: empty structured response")
	}
	return orchestrator.StructuredResponse{JSON: resp.JSON, Usage: resp.Usage}, nil
}

// RuntimeFor adapts a workspace provider to orchestrator.AIRuntime: the
// CLI providers' structured call, or an API provider's native JSON call.
func RuntimeFor(p domain.LLMProvider) (orchestrator.AIRuntime, error) {
	if sp, ok := p.(StructuredProvider); ok {
		return FromProvider(sp), nil
	}
	if jp, ok := p.(domain.JSONProvider); ok {
		return jsonRuntime{p: jp}, nil
	}
	return nil, errors.New("llm: provider has no structured output")
}
