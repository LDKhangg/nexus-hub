package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/memory"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

type fakeStore struct {
	mu       sync.Mutex
	job      *AgentJob
	ctx      JobContext
	usage    domain.Usage
	outcome  *JobResult
	fallback *struct {
		code, text string
		through    int64
	}
	retryAt   time.Time
	retryCode string
	staleOn   string
}

func (s *fakeStore) Claim(context.Context, time.Time) (*AgentJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.job
	s.job = nil
	return j, nil
}
func (s *fakeStore) Load(context.Context, AgentJob) (JobContext, error) { return s.ctx, nil }
func (s *fakeStore) AddUsage(_ context.Context, _ AgentJob, u domain.Usage) error {
	s.usage.Add(u)
	return nil
}
func (s *fakeStore) CommitOutcome(_ context.Context, _ AgentJob, r JobResult) error {
	if s.staleOn == "commit" {
		return ErrStaleAgentClaim
	}
	s.outcome = &r
	return nil
}
func (s *fakeStore) CommitFallback(_ context.Context, _ AgentJob, code, text string, through int64) error {
	s.fallback = &struct {
		code, text string
		through    int64
	}{code, text, through}
	return nil
}
func (s *fakeStore) Retry(_ context.Context, _ AgentJob, at time.Time, code string) error {
	s.retryAt, s.retryCode = at, code
	return nil
}

type fakeTriager struct {
	d     TriageDecision
	u     domain.Usage
	errs  []error
	calls int
}

func (f *fakeTriager) Triage(context.Context, memory.ContextPacket, string, bool) (TriageDecision, domain.Usage, error) {
	f.calls++
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return TriageDecision{}, f.u, err
		}
	}
	return f.d, f.u, nil
}

type fakeInvestigator struct {
	budget TokenBudget
	call   string
	report ActorReport
	err    error
}

func (f *fakeInvestigator) Investigate(ctx context.Context, _ memory.ContextPacket, _ TriageDecision, _ string, specs []tools.ToolSpec, exec *tools.Executor, b TokenBudget) (ActorReport, error) {
	f.budget = b
	r := f.report
	if f.call != "" {
		res := exec.Call(ctx, 1, f.call, json.RawMessage(`{}`))
		r.Calls = append(r.Calls, CallRecord{Step: 1, Name: f.call, Status: res.Status, Tier: res.Tier, IsError: res.IsError})
	}
	return r, f.err
}

type readTool struct{ runs int }

func (t *readTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{Name: "fake_read", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`), Tier: tenant.TierRead}
}
func (t *readTool) Run(context.Context, tools.Call) (tools.ToolOutput, error) {
	t.runs++
	return tools.ToolOutput{Text: "found it"}, nil
}

func jobCtx(enabled []string) JobContext {
	return JobContext{
		Title: "Login broken", TaskURL: "https://app.clickup.com/t/abc",
		History: []memory.Message{
			{ID: 1, SenderRole: memory.SenderRoleExternalUser, SenderName: "Old", Content: "old"},
			{ID: 5, SenderRole: memory.SenderRoleExternalUser, SenderName: "Alice", Content: "login fails with 500, token ghp_" + strings.Repeat("a", 36)},
		},
		LastTriagedID: 1,
		Config:        tenant.AgentConfig{Enabled: true, ConfidenceThreshold: 0.85, Tools: tenant.ToolPolicyConfig{Enabled: enabled}},
	}
}

func newOrch(t *testing.T, s *fakeStore, tr *fakeTriager, inv *fakeInvestigator) *Orchestrator {
	t.Helper()
	reg, err := tools.NewRegistry(&readTool{})
	if err != nil {
		t.Fatal(err)
	}
	o := NewOrchestrator(s, func(context.Context, string) (Triager, Investigator, error) { return tr, inv, nil }, reg, &memRuns{}, NewRedactor(nil), nil,
		OrchestratorSettings{Limits: tools.DefaultLimits(), MaxInputTokens: 60000, MaxOutputTokens: 6000})
	o.now = func() time.Time { return time.Unix(1000, 0) }
	return o
}

func job(attempts int) *AgentJob {
	return &AgentJob{ID: 42, WorkspaceID: "ws", ThreadID: "th", ClaimToken: "c", Attempts: attempts}
}

func TestOrchestrator_Ignore(t *testing.T) {
	s := &fakeStore{job: job(1), ctx: jobCtx(nil)}
	tr := &fakeTriager{d: TriageDecision{Category: CategoryIgnore, Confidence: 0.95, Summary: "thanks"}}
	if err := newOrch(t, s, tr, &fakeInvestigator{}).processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.outcome == nil || s.outcome.Outcome != OutcomeIgnore || s.outcome.Text != "" || s.outcome.ThroughMessageID != 5 || s.outcome.RouterNote == "" {
		t.Fatalf("%+v", s.outcome)
	}
}

func TestOrchestrator_Notify(t *testing.T) {
	s := &fakeStore{job: job(1), ctx: jobCtx(nil)}
	tr := &fakeTriager{d: TriageDecision{Category: CategoryInvestigate, Urgency: UrgencyHigh, Confidence: 0.9, Summary: "login 500 for @bob", Goal: "g"}, u: domain.Usage{Input: 300, Output: 40}}
	if err := newOrch(t, s, tr, &fakeInvestigator{}).processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "🧠 Nexus · Login broken\nhigh · login 500 for @bob\nhttps://app.clickup.com/t/abc"
	if s.outcome == nil || s.outcome.Outcome != OutcomeNotify || s.outcome.Text != want {
		t.Fatalf("%+v", s.outcome)
	}
	if s.usage.Input != 300 {
		t.Fatalf("usage %+v", s.usage)
	}
}

func TestOrchestrator_Investigate(t *testing.T) {
	s := &fakeStore{job: job(1), ctx: jobCtx([]string{"fake_read"})}
	tr := &fakeTriager{d: TriageDecision{Category: CategoryInvestigate, Urgency: UrgencyNormal, Confidence: 0.9, Summary: "s", Goal: "find"}, u: domain.Usage{Input: 300, Output: 40}}
	inv := &fakeInvestigator{call: "fake_read", report: ActorReport{Text: "- root cause: nil map", Usage: domain.Usage{Input: 2000, Output: 200, CacheRead: 500}}}
	if err := newOrch(t, s, tr, inv).processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := s.outcome
	if r == nil || r.Outcome != OutcomeInvestigate || r.ActorNote != "- root cause: nil map" {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{"🔎 Nexus · Login broken\nnormal · s\n\n- root cause: nil map\n\n", "Tools: fake_read ✓", "Tokens: 2800/240", "https://app.clickup.com/t/abc"} {
		if !strings.Contains(r.Text, want) {
			t.Fatalf("missing %q:\n%s", want, r.Text)
		}
	}
}

func TestOrchestrator_RetryThenSuccess(t *testing.T) {
	s := &fakeStore{job: job(1), ctx: jobCtx(nil)}
	tr := &fakeTriager{d: TriageDecision{Category: CategoryNotify, Confidence: 0.9, Summary: "s"}, errs: []error{&TriageError{Code: CodeLLMError, Retryable: true}}}
	o := newOrch(t, s, tr, &fakeInvestigator{})
	if err := o.processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.retryCode != CodeLLMError || !s.retryAt.Equal(time.Unix(1005, 0)) || s.outcome != nil || s.fallback != nil {
		t.Fatalf("retry %v %s", s.retryAt, s.retryCode)
	}
	s.job = job(2)
	if err := o.processNext(context.Background()); err != nil || s.outcome == nil {
		t.Fatalf("%v %+v", err, s.outcome)
	}
}

func TestOrchestrator_ExhaustedFallsBack(t *testing.T) {
	s := &fakeStore{job: job(MaxAgentAttempts), ctx: jobCtx(nil)}
	tr := &fakeTriager{errs: []error{&TriageError{Code: CodeInvalidDecision, Retryable: true}}}
	if err := newOrch(t, s, tr, &fakeInvestigator{}).processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := s.fallback
	if f == nil || f.code != CodeInvalidDecision || f.through != 5 || !strings.HasPrefix(f.text, "⚠️ AI triage unavailable (invalid_decision)") || !strings.Contains(f.text, "Alice") || !strings.Contains(f.text, "login fails with 500") {
		t.Fatalf("%+v", f)
	}
	if strings.Contains(f.text, "ghp_aaaa") {
		t.Fatal("secret in fallback")
	}
	// A recovered lease past the limit falls back without calling the LLM.
	s = &fakeStore{job: job(MaxAgentAttempts + 1), ctx: jobCtx(nil)}
	tr = &fakeTriager{}
	if err := newOrch(t, s, tr, &fakeInvestigator{}).processNext(context.Background()); err != nil || tr.calls != 0 || s.fallback == nil || s.fallback.code != "attempts_exhausted" {
		t.Fatalf("%v calls=%d %+v", err, tr.calls, s.fallback)
	}
}

func TestOrchestrator_NonRetryableImmediate(t *testing.T) {
	for _, code := range []string{CodeLLMRefused, CodeLLMUnconfigured} {
		s := &fakeStore{job: job(1), ctx: jobCtx(nil)}
		tr := &fakeTriager{errs: []error{&TriageError{Code: code}}}
		if err := newOrch(t, s, tr, &fakeInvestigator{}).processNext(context.Background()); err != nil {
			t.Fatal(err)
		}
		if s.fallback == nil || s.fallback.code != code || s.retryCode != "" {
			t.Fatalf("%s: %+v", code, s.fallback)
		}
	}
	// No LLM credential at all.
	s := &fakeStore{job: job(1), ctx: jobCtx(nil)}
	reg, _ := tools.NewRegistry()
	o := NewOrchestrator(s, func(context.Context, string) (Triager, Investigator, error) {
		return nil, nil, errors.New("unconfigured")
	}, reg, &memRuns{}, NewRedactor(nil), nil, OrchestratorSettings{Limits: tools.DefaultLimits()})
	if err := o.processNext(context.Background()); err != nil || s.fallback == nil || s.fallback.code != CodeLLMUnconfigured {
		t.Fatalf("%v %+v", err, s.fallback)
	}
}

func TestOrchestrator_BudgetCarriesAcrossAttempts(t *testing.T) {
	j := job(2)
	j.Usage = domain.Usage{Input: 50000, Output: 1000}
	s := &fakeStore{job: j, ctx: jobCtx([]string{"fake_read"})}
	tr := &fakeTriager{d: TriageDecision{Category: CategoryInvestigate, Confidence: 0.9, Summary: "s", Goal: "g"}, u: domain.Usage{Input: 300, Output: 40}}
	inv := &fakeInvestigator{report: ActorReport{Text: "r", Usage: domain.Usage{Input: 100, Output: 10}}}
	if err := newOrch(t, s, tr, inv).processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if inv.budget.Used.Input != 50300 || inv.budget.Used.Output != 1040 || inv.budget.MaxInput != 60000 || inv.budget.MaxOutput != 6000 {
		t.Fatalf("budget %+v", inv.budget)
	}
	if !strings.Contains(s.outcome.Text, "Tokens: 50400/1050") {
		t.Fatalf("job total not shown:\n%s", s.outcome.Text)
	}
	// Usage of a failed investigation is still recorded.
	s = &fakeStore{job: job(1), ctx: jobCtx([]string{"fake_read"})}
	inv = &fakeInvestigator{report: ActorReport{Usage: domain.Usage{Input: 700}}, err: &TriageError{Code: CodeLLMError, Retryable: true}}
	if err := newOrch(t, s, tr, inv).processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.usage.Input != 1000 || s.retryCode != CodeLLMError {
		t.Fatalf("usage %+v retry %s", s.usage, s.retryCode)
	}
}

func TestOrchestrator_StaleClaimIgnored(t *testing.T) {
	s := &fakeStore{job: job(1), ctx: jobCtx(nil), staleOn: "commit"}
	tr := &fakeTriager{d: TriageDecision{Category: CategoryNotify, Confidence: 0.9, Summary: "s"}}
	if err := newOrch(t, s, tr, &fakeInvestigator{}).processNext(context.Background()); err != nil {
		t.Fatalf("stale claim must be dropped quietly: %v", err)
	}
	if s.fallback != nil || s.retryCode != "" {
		t.Fatal("stale job touched")
	}
}

func TestOrchestrator_EmptyBatchAndDisabled(t *testing.T) {
	c := jobCtx(nil)
	c.LastTriagedID = 5
	s := &fakeStore{job: job(1), ctx: c}
	tr := &fakeTriager{}
	if err := newOrch(t, s, tr, &fakeInvestigator{}).processNext(context.Background()); err != nil || tr.calls != 0 || s.outcome == nil || s.outcome.Text != "" {
		t.Fatalf("empty batch: %v %+v", err, s.outcome)
	}
	c = jobCtx(nil)
	c.Config.Enabled = false
	s = &fakeStore{job: job(1), ctx: c}
	if err := newOrch(t, s, tr, &fakeInvestigator{}).processNext(context.Background()); err != nil || tr.calls != 0 || s.fallback == nil || s.fallback.code != "agent_disabled" {
		t.Fatalf("disabled: %v %+v", err, s.fallback)
	}
}

type fakeCompressor struct {
	ws, thread string
	calls      int
}

func (c *fakeCompressor) Compress(_ context.Context, ws, thread string) (bool, domain.Usage, error) {
	c.calls++
	c.ws, c.thread = ws, thread
	return true, domain.Usage{}, nil
}

func TestOrchestrator_CompressAfterSuccess(t *testing.T) {
	comp := &fakeCompressor{}
	mk := func(s *fakeStore, tr *fakeTriager) *Orchestrator {
		o := newOrch(t, s, tr, &fakeInvestigator{})
		o.compressor = comp
		return o
	}
	s := &fakeStore{job: job(1), ctx: jobCtx(nil)}
	if err := mk(s, &fakeTriager{d: TriageDecision{Category: CategoryNotify, Confidence: 0.9, Summary: "s"}}).processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if comp.calls != 1 || comp.ws != "ws" || comp.thread != "th" {
		t.Fatalf("%+v", comp)
	}
	s = &fakeStore{job: job(1), ctx: jobCtx(nil)}
	_ = mk(s, &fakeTriager{errs: []error{&TriageError{Code: CodeLLMError, Retryable: true}}}).processNext(context.Background())
	s = &fakeStore{job: job(1), ctx: jobCtx(nil)}
	_ = mk(s, &fakeTriager{errs: []error{&TriageError{Code: CodeLLMRefused}}}).processNext(context.Background())
	if comp.calls != 1 {
		t.Fatalf("compressed after a failure: %d", comp.calls)
	}
}
