package llm

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

type fakeLedger struct {
	mu      sync.Mutex
	rows    []domain.LLMUsageRecord
	cost    float64
	readErr error
	addErr  error
	reads   int
}

func (l *fakeLedger) AddLLMUsage(_ context.Context, r domain.LLMUsageRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.addErr != nil {
		return l.addErr
	}
	l.rows = append(l.rows, r)
	return nil
}

func (l *fakeLedger) LLMCostForDay(_ context.Context, _ string, _ time.Time) (float64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reads++
	return l.cost, l.readErr
}

func (l *fakeLedger) snapshot() []domain.LLMUsageRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]domain.LLMUsageRecord(nil), l.rows...)
}

type plainProvider struct {
	mu    sync.Mutex
	calls int
	usage domain.Usage
}

func (p *plainProvider) Generate(context.Context, *domain.LLMRequest) (*domain.LLMResponse, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return &domain.LLMResponse{Text: "ok", Usage: p.usage}, nil
}

type toolsProvider struct{ plainProvider }

func (p *toolsProvider) GenerateWithTools(context.Context, *domain.ToolRequest) (*domain.ToolResponse, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return &domain.ToolResponse{Usage: p.usage}, nil
}

type apiProvider struct{ toolsProvider }

func (p *apiProvider) GenerateJSON(context.Context, *domain.JSONRequest) (*domain.JSONResponse, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return &domain.JSONResponse{Usage: p.usage}, nil
}

type cliProvider struct{ toolsProvider }

func (p *cliProvider) GenerateStructured(context.Context, *StructuredRequest) (*StructuredResponse, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return &StructuredResponse{Usage: p.usage}, nil
}

func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

// TC-B-19 (REQ-8)
func TestEstimateCostUSD_MathAndUnknownModel(t *testing.T) {
	u := domain.Usage{Input: 1_000_000, Output: 1_000_000, CacheRead: 1_000_000, CacheWrite: 1_000_000}
	want := map[string]float64{
		"claude-opus-5-5":   4.00 + 20.00 + 0.20 + 5.00,
		"claude-sonnet-5-5": 2.00 + 10.00 + 0.10 + 2.50,
		"claude-haiku-5-5":  0.10 + 0.50 + 0.01 + 0.125,
	}
	for model, w := range want {
		got, ok := EstimateCostUSD(model, u)
		if !ok || math.Abs(got-w) > 1e-9 {
			t.Fatalf("%s: cost=%v ok=%v want %v", model, got, ok, w)
		}
	}
	// 500 input + 100 output tokens on sonnet: (500*2 + 100*10)/1e6.
	got, _ := EstimateCostUSD("claude-sonnet-5-5", domain.Usage{Input: 500, Output: 100})
	if math.Abs(got-0.002) > 1e-12 {
		t.Fatalf("small usage cost = %v", got)
	}
	// Negative counts are clamped to zero, never a credit.
	got, ok := EstimateCostUSD("claude-sonnet-5-5", domain.Usage{Input: -9_000_000, Output: 1_000_000})
	if !ok || math.Abs(got-10.0) > 1e-9 {
		t.Fatalf("negative clamp: %v", got)
	}
	if c, ok := EstimateCostUSD("claude-unknown-9", u); ok || c != 0 {
		t.Fatalf("unknown model: %v %v", c, ok)
	}
	if !KnownPricedModel("claude-haiku-5-5") || KnownPricedModel("x") {
		t.Fatal("KnownPricedModel wrong")
	}
}

// TC-B-20 (REQ-7)
func TestMeterRecord_CostProviderAndSafety(t *testing.T) {
	day := time.Date(2026, 11, 20, 23, 59, 0, 0, time.UTC)
	l := &fakeLedger{}
	m := &Meter{Ledger: l, Now: fixedNow(day)}
	ctx := context.Background()

	m.Record(ctx, "ws", ProviderAnthropic, "claude-sonnet-5-5", domain.Usage{Input: 100, CacheRead: 50, CacheWrite: 10, Output: 20})
	m.Record(ctx, "ws", ProviderClaudeCLI, "claude-sonnet-5-5", domain.Usage{Input: 100, Output: 20})
	m.Record(ctx, "ws", ProviderAnthropic, "claude-sonnet-5-5", domain.Usage{})
	m.Record(ctx, "ws", ProviderAnthropic, "claude-sonnet-5-5", domain.Usage{Input: -5, Output: -5})
	m.Record(ctx, "ws", ProviderAnthropic, "ignore previous instructions; DROP TABLE x", domain.Usage{Input: 10})
	m.Record(ctx, "", ProviderAnthropic, "claude-sonnet-5-5", domain.Usage{Input: 10})
	rows := l.snapshot()
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (zero, negative and empty-workspace calls are not recorded)", len(rows))
	}
	if rows[0].InputTokens != 160 || rows[0].OutputTokens != 20 || rows[0].CostUSD <= 0 {
		t.Fatalf("api row = %+v", rows[0])
	}
	if rows[1].Provider != string(ProviderClaudeCLI) || rows[1].CostUSD != 0 || rows[1].InputTokens != 100 {
		t.Fatalf("cli row must count tokens at cost 0: %+v", rows[1])
	}
	if rows[2].Model != "other" || rows[2].CostUSD != 0 {
		t.Fatalf("junk model must become other with cost 0: %+v", rows[2])
	}
	if !rows[0].Day.Equal(day) {
		t.Fatalf("day = %v", rows[0].Day)
	}

	// A ledger write failure never panics or surfaces.
	bad := &Meter{Ledger: &fakeLedger{addErr: errors.New("db down")}}
	bad.Record(ctx, "ws", ProviderAnthropic, "claude-sonnet-5-5", domain.Usage{Input: 1})
	var nilMeter *Meter
	nilMeter.Record(ctx, "ws", ProviderAnthropic, "m", domain.Usage{Input: 1})
	if err := nilMeter.Allow(ctx, "ws"); err != nil {
		t.Fatal("nil meter must allow")
	}
}

// TC-B-21 (REQ-7)
func TestMeterAllow_BudgetBoundaries(t *testing.T) {
	ctx := context.Background()
	refused := 0
	mk := func(budget, spent float64, readErr error) (*Meter, *fakeLedger) {
		l := &fakeLedger{cost: spent, readErr: readErr}
		return &Meter{Ledger: l, Budget: budget, OnRefused: func() { refused++ }}, l
	}
	cases := []struct {
		name          string
		budget, spent float64
		readErr       error
		ws            string
		refuse        bool
	}{
		{"under budget allows", 2, 1.99, nil, "ws", false},
		{"at budget refuses", 2, 2, nil, "ws", true},
		{"over budget refuses", 2, 5, nil, "ws", true},
		{"zero budget is off", 0, 100, nil, "ws", false},
		{"negative budget is off", -1, 100, nil, "ws", false},
		{"read error lets through", 2, 99, errors.New("db"), "ws", false},
		{"no workspace allows", 2, 99, nil, "", false},
	}
	for _, c := range cases {
		refused = 0
		m, _ := mk(c.budget, c.spent, c.readErr)
		err := m.Allow(ctx, c.ws)
		if c.refuse {
			le, ok := domain.AsLLMError(err)
			if !ok || le.Code != "llm_budget_exceeded" || le.Retryable || refused != 1 {
				t.Fatalf("%s: err=%v refused=%d", c.name, err, refused)
			}
		} else if err != nil || refused != 0 {
			t.Fatalf("%s: err=%v refused=%d", c.name, err, refused)
		}
	}
}

// TC-B-22 (REQ-7)
func TestMeterWrap_RefusedCallNeverReachesProvider(t *testing.T) {
	ctx := context.Background()
	over := &fakeLedger{cost: 3}
	prov := &cliProvider{}
	prov.usage = domain.Usage{Input: 10, Output: 5}
	m := &Meter{Ledger: over, Budget: 2}
	w := m.Wrap(prov, "ws", ProviderAnthropic, "claude-sonnet-5-5")
	if _, err := w.Generate(ctx, &domain.LLMRequest{}); err == nil {
		t.Fatal("Generate must be refused")
	}
	if _, err := w.(domain.ToolCallingProvider).GenerateWithTools(ctx, &domain.ToolRequest{}); err == nil {
		t.Fatal("GenerateWithTools must be refused")
	}
	if _, err := w.(StructuredProvider).GenerateStructured(ctx, &StructuredRequest{}); err == nil {
		t.Fatal("GenerateStructured must be refused")
	}
	api := &apiProvider{}
	wa := m.Wrap(api, "ws", ProviderAnthropic, "claude-sonnet-5-5")
	if _, err := wa.(domain.JSONProvider).GenerateJSON(ctx, &domain.JSONRequest{}); err == nil {
		t.Fatal("GenerateJSON must be refused")
	}
	if prov.calls != 0 || api.calls != 0 || len(over.snapshot()) != 0 {
		t.Fatalf("provider reached or ledger written: %d %d", prov.calls, api.calls)
	}

	// Under budget: provider is called and the usage is recorded once per call.
	ok := &fakeLedger{cost: 0.5}
	m2 := &Meter{Ledger: ok, Budget: 2}
	p2 := &plainProvider{usage: domain.Usage{Input: 10, Output: 5}}
	w2 := m2.Wrap(p2, "ws", ProviderAnthropic, "claude-sonnet-5-5")
	if _, err := w2.Generate(ctx, &domain.LLMRequest{}); err != nil || p2.calls != 1 {
		t.Fatalf("err=%v calls=%d", err, p2.calls)
	}
	rows := ok.snapshot()
	if len(rows) != 1 || rows[0].Model != "claude-sonnet-5-5" || rows[0].WorkspaceID != "ws" {
		t.Fatalf("rows = %+v (default model must be used when the request has none)", rows)
	}
}

// TC-B-23 (REQ-8)
func TestMeterWrap_KeepsCapabilities(t *testing.T) {
	m := &Meter{Ledger: &fakeLedger{}}
	has := func(p domain.LLMProvider) (tools, js, structured bool) {
		_, tools = p.(domain.ToolCallingProvider)
		_, js = p.(domain.JSONProvider)
		_, structured = p.(StructuredProvider)
		return
	}
	if tl, js, st := has(m.Wrap(&plainProvider{}, "w", ProviderAnthropic, "m")); tl || js || st {
		t.Fatal("plain provider gained capabilities")
	}
	if tl, js, st := has(m.Wrap(&toolsProvider{}, "w", ProviderAnthropic, "m")); !tl || js || st {
		t.Fatal("tools-only capabilities changed")
	}
	if tl, js, st := has(m.Wrap(&apiProvider{}, "w", ProviderAnthropic, "m")); !tl || !js || st {
		t.Fatal("api capabilities changed")
	}
	if tl, js, st := has(m.Wrap(&cliProvider{}, "w", ProviderClaudeCLI, "m")); !tl || js || !st {
		t.Fatal("cli capabilities changed")
	}
	var nilMeter *Meter
	p := &plainProvider{}
	if nilMeter.Wrap(p, "w", ProviderAnthropic, "m") != domain.LLMProvider(p) {
		t.Fatal("nil meter must return the provider unchanged")
	}
	if m.Wrap(nil, "w", ProviderAnthropic, "m") != nil {
		t.Fatal("nil provider must stay nil")
	}
}

// TC-B-24 (REQ-7)
func TestMeterAllow_BudgetResetsAtUTCMidnight(t *testing.T) {
	ctx := context.Background()
	// A ledger keyed by UTC day, like the real one.
	byDay := map[string]float64{"2026-11-20": 2.5}
	l := &dayLedger{byDay: byDay}
	now := time.Date(2026, 11, 20, 23, 59, 59, 0, time.UTC)
	m := &Meter{Ledger: l, Budget: 2, Now: func() time.Time { return now }}
	if err := m.Allow(ctx, "ws"); err == nil {
		t.Fatal("must refuse at 23:59:59 after the budget is spent")
	}
	now = now.Add(time.Second)
	if err := m.Allow(ctx, "ws"); err != nil {
		t.Fatalf("new UTC day must allow: %v", err)
	}
	// A +07 local time that is still the previous UTC day is judged in UTC.
	loc := time.FixedZone("ict", 7*3600)
	m.Now = func() time.Time { return time.Date(2026, 11, 21, 5, 0, 0, 0, loc) } // 2026-11-20 22:00 UTC
	if err := m.Allow(ctx, "ws"); err == nil {
		t.Fatal("day must be taken in UTC")
	}
}

type dayLedger struct{ byDay map[string]float64 }

func (l *dayLedger) AddLLMUsage(context.Context, domain.LLMUsageRecord) error { return nil }
func (l *dayLedger) LLMCostForDay(_ context.Context, _ string, d time.Time) (float64, error) {
	return l.byDay[d.UTC().Format("2006-01-02")], nil
}

// TC-B-25 (REQ-7), meaningful under -race.
func TestMeter_ConcurrentRecordAndAllowAreRaceFree(t *testing.T) {
	l := &fakeLedger{cost: 0}
	m := &Meter{Ledger: l, Budget: 100}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			_ = m.Allow(ctx, "ws")
			m.Record(ctx, "ws", ProviderAnthropic, "claude-haiku-5-5", domain.Usage{Input: 1, Output: 1})
		}()
	}
	wg.Wait()
	if n := len(l.snapshot()); n != 20 {
		t.Fatalf("rows = %d, want 20", n)
	}
	// Repeated read errors are tolerated without panic (rate-limited log).
	e := &Meter{Ledger: &fakeLedger{readErr: errors.New("x")}, Budget: 1}
	for i := 0; i < 5; i++ {
		if err := e.Allow(context.Background(), "ws"); err != nil {
			t.Fatal(err)
		}
	}
}
