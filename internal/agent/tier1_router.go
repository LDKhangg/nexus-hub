package agent

import (
	"context"
	"errors"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/memory"
	"github.com/LDKhangg/nexus-hub/internal/orchestrator"
)

// Tier1Router classifies a context packet with a small model. Built with
// NewClassifyTier1Router it goes through the orchestrator's Classify over an
// AIRuntime (REQ-12); built with NewTier1Router it uses the plain text
// prompt, for providers without structured output.
type Tier1Router struct {
	llm   domain.LLMProvider
	rt    orchestrator.AIRuntime
	model string
}

func NewTier1Router(llm domain.LLMProvider, model string) *Tier1Router {
	return &Tier1Router{llm: llm, model: model}
}

// NewClassifyTier1Router triages through orchestrator Classify on rt with
// model (the triage_model setting).
func NewClassifyTier1Router(rt orchestrator.AIRuntime, model string) *Tier1Router {
	return &Tier1Router{rt: rt, model: model}
}

// Triage returns the validated decision and the usage of the call (usage is
// returned even when the decision is invalid).
func (r *Tier1Router) Triage(ctx context.Context, packet memory.ContextPacket, persona string, toolsAvailable bool) (TriageDecision, domain.Usage, error) {
	if r != nil && r.rt != nil {
		return r.triageClassify(ctx, packet, persona, toolsAvailable)
	}
	if r == nil || r.llm == nil {
		return TriageDecision{}, domain.Usage{}, &TriageError{Code: CodeLLMUnconfigured}
	}
	resp, err := r.llm.Generate(ctx, &domain.LLMRequest{
		Model: r.model, MaxTokens: TriageMaxTokens, System: triageSystem(persona, toolsAvailable),
		Messages: []domain.Message{{Role: domain.UserRole, Text: packet.Render()}},
	})
	if err != nil {
		return TriageDecision{}, domain.Usage{}, triageProviderError(ctx, err)
	}
	d, err := ParseTriageDecision(resp.Text)
	if err != nil {
		return TriageDecision{}, resp.Usage, &TriageError{Code: CodeInvalidDecision, Retryable: true}
	}
	return d, resp.Usage, nil
}

// triageProviderError maps a provider error to a safe triage error.
func triageProviderError(ctx context.Context, err error) error {
	if errors.Is(err, domain.ErrLLMRefused) {
		return &TriageError{Code: CodeLLMRefused}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if le, ok := domain.AsLLMError(err); ok {
		// A CLI provider's safe code (llm_unavailable, llm_unconfigured,
		// llm_timeout, llm_error).
		return &TriageError{Code: le.Code, Retryable: le.Retryable}
	}
	return &TriageError{Code: CodeLLMError, Retryable: true}
}

// triageClassify runs Tier-1 triage as one Classify call. The packet (the
// untrusted comment text) is only the Classify request, which Classify puts
// in its data block; the intent guide is a code constant and the persona
// owner configuration. Repair is off: one model call per job attempt, as
// before, and the job retry handles invalid output.
func (r *Tier1Router) triageClassify(ctx context.Context, packet memory.ContextPacket, persona string, toolsAvailable bool) (TriageDecision, domain.Usage, error) {
	o := orchestrator.NewOrchestrator(r.rt, r.model)
	o.MaxRepair = 0
	intents, guide := triageIntentsNoTools, triageGuideNoTools
	if toolsAvailable {
		intents, guide = triageIntents, triageGuide
	}
	c, u, err := o.Classify(ctx, orchestrator.ClassifyInput{
		Request: packet.Render(), Source: "clickup_comment",
		Intents: intents, Guide: guide, Context: persona, Omitted: packet.Omitted, Notes: true,
	})
	if err != nil {
		if errors.Is(err, orchestrator.ErrInvalidOutput) || errors.Is(err, orchestrator.ErrInvalidInput) {
			return TriageDecision{}, u.Tokens, &TriageError{Code: CodeInvalidDecision, Retryable: true}
		}
		return TriageDecision{}, u.Tokens, triageProviderError(ctx, err)
	}
	d, err := decisionFromClassification(c)
	if err != nil {
		return TriageDecision{}, u.Tokens, &TriageError{Code: CodeInvalidDecision, Retryable: true}
	}
	return d, u.Tokens, nil
}

// Tier-1 intent vocabularies and guides for Classify. The intents are the
// triage categories, so routing, report and dedupe see the same decision.
var (
	triageIntents        = []string{string(CategoryIgnore), string(CategoryNotify), string(CategoryInvestigate)}
	triageIntentsNoTools = []string{string(CategoryIgnore), string(CategoryNotify)}
)

const (
	triageGuideBase = "The request is the new ClickUp comments of an engineering team's task. " +
		"ignore: chit-chat or nothing to do. notify: a human should look. "
	triageGuideTail = "summary: what the new comments ask or report. " +
		"priority is the urgency: urgent or high = high, normal = normal, low = low."
	triageGuide = triageGuideBase +
		"investigate: answering needs reading code, PRs or the task; goal is then required and states a concrete goal. " +
		triageGuideTail
	triageGuideNoTools = triageGuideBase + triageGuideTail
)

// decisionFromClassification maps a Classify result to the TriageDecision
// contract with the checks ParseTriageDecision applies.
func decisionFromClassification(c orchestrator.Classification) (TriageDecision, error) {
	d := TriageDecision{
		Category:   Category(c.Intent),
		Urgency:    urgencyFromPriority(c.Priority),
		Confidence: c.Confidence,
		Summary:    c.Summary,
		Goal:       c.Goal,
	}
	switch d.Category {
	case CategoryIgnore, CategoryNotify, CategoryInvestigate:
	default:
		return TriageDecision{}, ErrInvalidDecision
	}
	if !(d.Confidence >= 0 && d.Confidence <= 1) {
		return TriageDecision{}, ErrInvalidDecision
	}
	if utf8.RuneCountInString(d.Summary) > maxSummaryRunes || utf8.RuneCountInString(d.Goal) > maxGoalRunes {
		return TriageDecision{}, ErrInvalidDecision
	}
	if d.Category == CategoryInvestigate && d.Goal == "" {
		return TriageDecision{}, ErrInvalidDecision
	}
	return d, nil
}

func urgencyFromPriority(p string) Urgency {
	switch p {
	case "urgent", "high":
		return UrgencyHigh
	case "low":
		return UrgencyLow
	default:
		return UrgencyNormal
	}
}
