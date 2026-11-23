package llm

import (
	"context"
	"log"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// UsageLedger stores the daily LLM spend ledger (storage.OpsStore).
type UsageLedger interface {
	AddLLMUsage(ctx context.Context, r domain.LLMUsageRecord) error
	LLMCostForDay(ctx context.Context, workspaceID string, day time.Time) (float64, error)
}

// ledgerWriteTimeout bounds one ledger write. The write runs on a context
// detached from the caller's, so a call that finished is still counted
// when its caller was cancelled right after.
const ledgerWriteTimeout = 5 * time.Second

// ledgerLogEvery rate-limits the ledger failure logs.
const ledgerLogEvery = time.Minute

var ledgerModelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/#@-]{0,127}$`)

// Meter counts every model call of a workspace into the spend ledger and
// enforces llm_daily_budget_usd. A nil *Meter meters nothing.
//
// Budget: when Budget > 0 and the workspace's estimated cost of the
// current UTC day is at or above it, a new call returns a non-retryable
// domain.LLMError with code llm_budget_exceeded before reaching the
// provider. Calls already running finish and are counted. A ledger read
// failure lets the call through (logged, rate-limited): the budget is a
// spend guard, not an availability dependency.
//
// Cost: estimated from the price table (pricing.go) for the API provider
// only. CLI providers run on a host sign-in (subscription) and have no
// per-call spend, so they count tokens with cost 0 and never hit the
// budget.
type Meter struct {
	Ledger UsageLedger
	Budget float64
	Now    func() time.Time
	// OnRefused, when set, is called once per call refused by the budget
	// (metrics). It receives nothing about the call.
	OnRefused func()

	unknownModels sync.Map // model -> struct{}: logged once
	lastLog       atomic.Int64
}

func (m *Meter) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func (m *Meter) logLimited(msg string) {
	now := time.Now().UnixNano()
	last := m.lastLog.Load()
	if now-last < int64(ledgerLogEvery) || !m.lastLog.CompareAndSwap(last, now) {
		return
	}
	log.Print(msg)
}

// Allow returns llm_budget_exceeded when the workspace spent its budget
// for the current UTC day; nil otherwise.
func (m *Meter) Allow(ctx context.Context, workspaceID string) error {
	if m == nil || m.Ledger == nil || m.Budget <= 0 || workspaceID == "" {
		return nil
	}
	spent, err := m.Ledger.LLMCostForDay(ctx, workspaceID, m.now())
	if err != nil {
		m.logLimited("llm budget: spend ledger read failed; call allowed")
		return nil
	}
	if spent >= m.Budget {
		if m.OnRefused != nil {
			m.OnRefused()
		}
		return &domain.LLMError{Code: domain.LLMCodeBudgetExceeded}
	}
	return nil
}

// Record adds one call's usage to the ledger. A call without tokens is
// not recorded.
func (m *Meter) Record(ctx context.Context, workspaceID string, provider ProviderType, model string, u domain.Usage) {
	if m == nil || m.Ledger == nil || workspaceID == "" {
		return
	}
	in := max(u.Input, 0) + max(u.CacheRead, 0) + max(u.CacheWrite, 0)
	out := max(u.Output, 0)
	if in == 0 && out == 0 {
		return
	}
	if !ledgerModelRe.MatchString(model) {
		model = "other"
	}
	var cost float64
	if provider == ProviderAnthropic {
		c, ok := EstimateCostUSD(model, u)
		if !ok {
			if _, seen := m.unknownModels.LoadOrStore(model, struct{}{}); !seen {
				log.Printf("llm usage: model %s has no price; tokens counted, cost 0", model)
			}
		}
		cost = c
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ledgerWriteTimeout)
	defer cancel()
	if err := m.Ledger.AddLLMUsage(wctx, domain.LLMUsageRecord{
		WorkspaceID: workspaceID, Day: m.now(), Provider: string(provider), Model: model,
		InputTokens: in, OutputTokens: out, CostUSD: cost,
	}); err != nil {
		m.logLimited("llm usage: spend ledger write failed")
	}
}

// Wrap returns p metered for workspaceID. The wrapper keeps the optional
// interfaces p has (tool calling, native JSON or CLI structured output),
// so callers' type checks see the same capabilities.
func (m *Meter) Wrap(p domain.LLMProvider, workspaceID string, provider ProviderType, defaultModel string) domain.LLMProvider {
	if m == nil || p == nil {
		return p
	}
	b := meteredBase{inner: p, m: m, ws: workspaceID, provider: provider, def: defaultModel}
	tc, hasTools := p.(domain.ToolCallingProvider)
	sp, hasStructured := p.(StructuredProvider)
	jp, hasJSON := p.(domain.JSONProvider)
	switch {
	case hasTools && hasStructured:
		return &meteredCLI{meteredTools: meteredTools{meteredBase: b, tools: tc}, structured: sp}
	case hasTools && hasJSON:
		return &meteredAPI{meteredTools: meteredTools{meteredBase: b, tools: tc}, json: jp}
	case hasTools:
		return &meteredTools{meteredBase: b, tools: tc}
	default:
		return &b
	}
}

type meteredBase struct {
	inner    domain.LLMProvider
	m        *Meter
	ws       string
	provider ProviderType
	def      string
}

func (b *meteredBase) model(m string) string {
	if m == "" {
		return b.def
	}
	return m
}

func (b *meteredBase) Generate(ctx context.Context, req *domain.LLMRequest) (*domain.LLMResponse, error) {
	if err := b.m.Allow(ctx, b.ws); err != nil {
		return nil, err
	}
	resp, err := b.inner.Generate(ctx, req)
	if resp != nil && req != nil {
		b.m.Record(ctx, b.ws, b.provider, b.model(req.Model), resp.Usage)
	}
	return resp, err
}

type meteredTools struct {
	meteredBase
	tools domain.ToolCallingProvider
}

func (t *meteredTools) GenerateWithTools(ctx context.Context, req *domain.ToolRequest) (*domain.ToolResponse, error) {
	if err := t.m.Allow(ctx, t.ws); err != nil {
		return nil, err
	}
	resp, err := t.tools.GenerateWithTools(ctx, req)
	if resp != nil && req != nil {
		t.m.Record(ctx, t.ws, t.provider, t.model(req.Model), resp.Usage)
	}
	return resp, err
}

type meteredAPI struct {
	meteredTools
	json domain.JSONProvider
}

func (a *meteredAPI) GenerateJSON(ctx context.Context, req *domain.JSONRequest) (*domain.JSONResponse, error) {
	if err := a.m.Allow(ctx, a.ws); err != nil {
		return nil, err
	}
	resp, err := a.json.GenerateJSON(ctx, req)
	if resp != nil && req != nil {
		a.m.Record(ctx, a.ws, a.provider, a.model(req.Model), resp.Usage)
	}
	return resp, err
}

type meteredCLI struct {
	meteredTools
	structured StructuredProvider
}

func (c *meteredCLI) GenerateStructured(ctx context.Context, req *StructuredRequest) (*StructuredResponse, error) {
	if err := c.m.Allow(ctx, c.ws); err != nil {
		return nil, err
	}
	resp, err := c.structured.GenerateStructured(ctx, req)
	if resp != nil && req != nil {
		c.m.Record(ctx, c.ws, c.provider, c.model(req.Model), resp.Usage)
	}
	return resp, err
}

var (
	_ domain.LLMProvider         = (*meteredBase)(nil)
	_ domain.ToolCallingProvider = (*meteredTools)(nil)
	_ domain.JSONProvider        = (*meteredAPI)(nil)
	_ StructuredProvider         = (*meteredCLI)(nil)
)
