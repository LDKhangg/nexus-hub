package agent

import (
	"context"
	"errors"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/memory"
)

// Tier1Router classifies a context packet with a small model.
type Tier1Router struct {
	llm   domain.LLMProvider
	model string
}

func NewTier1Router(llm domain.LLMProvider, model string) *Tier1Router {
	return &Tier1Router{llm: llm, model: model}
}

// Triage returns the validated decision and the usage of the call (usage is
// returned even when the decision is invalid).
func (r *Tier1Router) Triage(ctx context.Context, packet memory.ContextPacket, persona string, toolsAvailable bool) (TriageDecision, domain.Usage, error) {
	if r == nil || r.llm == nil {
		return TriageDecision{}, domain.Usage{}, &TriageError{Code: CodeLLMUnconfigured}
	}
	resp, err := r.llm.Generate(ctx, &domain.LLMRequest{
		Model: r.model, MaxTokens: TriageMaxTokens, System: triageSystem(persona, toolsAvailable),
		Messages: []domain.Message{{Role: domain.UserRole, Text: packet.Render()}},
	})
	if err != nil {
		if errors.Is(err, domain.ErrLLMRefused) {
			return TriageDecision{}, domain.Usage{}, &TriageError{Code: CodeLLMRefused}
		}
		if ctx.Err() != nil {
			return TriageDecision{}, domain.Usage{}, ctx.Err()
		}
		return TriageDecision{}, domain.Usage{}, &TriageError{Code: CodeLLMError, Retryable: true}
	}
	d, err := ParseTriageDecision(resp.Text)
	if err != nil {
		return TriageDecision{}, resp.Usage, &TriageError{Code: CodeInvalidDecision, Retryable: true}
	}
	return d, resp.Usage, nil
}
