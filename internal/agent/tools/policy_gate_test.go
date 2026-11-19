package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	nhpolicy "github.com/LDKhangg/nexus-hub/internal/policy"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// Phase 9 Batch A: the executor asks the policy engine and honours
// approval grants (REQ-4, REQ-5, REQ-7). The fakes below stand in for the
// engine and the DB store; the DB-backed grant check is tested in the
// storage package.

type fnGate struct {
	mu   sync.Mutex
	fn   func(nhpolicy.Request) nhpolicy.Effect
	reqs []nhpolicy.Request
}

func (g *fnGate) Decide(_ context.Context, r nhpolicy.Request) (nhpolicy.Decision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reqs = append(g.reqs, r)
	return nhpolicy.Decision{Effect: g.fn(r)}, nil
}
func (g *fnGate) requests() []nhpolicy.Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]nhpolicy.Request(nil), g.reqs...)
}

// grantStore is a fakeStore that also verifies and consumes grants the way
// the DB store must: replay first, then verify every binding, then consume.
type grantStore struct {
	*fakeStore
	grants        map[int64]*fakeGrant
	grantedBegins int
}

type fakeGrant struct {
	check    GrantCheck
	approved bool
	expired  bool
	consumed int
}

func newGrantStore() *grantStore {
	return &grantStore{fakeStore: &fakeStore{}, grants: map[int64]*fakeGrant{}}
}

func (s *grantStore) BeginGranted(_ context.Context, r RunStart, g GrantCheck) (int64, *PriorRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grantedBegins++
	for _, row := range s.rows {
		if row.Start.Key == r.Key && row.Start.Tier != tenant.TierRead && row.Status != "denied" && row.Status != "error" && row.Status != "dry_run" {
			st := row.Status
			if st != "ok" {
				st = "unknown_outcome"
			}
			return 0, &PriorRun{Status: st, Summary: row.Summary}, nil
		}
	}
	gr := s.grants[g.ApprovalID]
	if gr == nil || !gr.approved || gr.expired || gr.consumed > 0 || gr.check != g {
		return 0, nil, ErrGrantInvalid
	}
	gr.consumed++
	s.nextID++
	s.rows = append(s.rows, &fakeRow{ID: s.nextID, Start: r, Status: "started"})
	return s.nextID, nil, nil
}

// grantEnv builds an executor over a grantStore whose gate says
// approval_required for dangerous tools and allow otherwise.
func grantEnv(t *testing.T, gate PolicyGate, store *grantStore, opts ...Option) (env, *Executor) {
	t.Helper()
	e := newEnvGate(t, policy(), gate, Limits{})
	ex := NewExecutor(e.exec.reg, policy(), DefaultLimits(), store, upperSan{}, 42, "ws-1", append(execOpts(gate), opts...)...)
	e.store = store.fakeStore
	return e, ex
}

func approvalGate() *fnGate {
	return &fnGate{fn: func(r nhpolicy.Request) nhpolicy.Effect {
		if r.Tier == nhpolicy.TierDangerous {
			return nhpolicy.EffectApprovalRequired
		}
		return nhpolicy.EffectAllow
	}}
}

func goodGrant() *fakeGrant {
	return &fakeGrant{approved: true, check: GrantCheck{ApprovalID: 7, WorkspaceID: "ws-1", Action: "danger_a", ResourceKey: "o/r", InputSHA256: InputSHA256(json.RawMessage(repoIn))}}
}

func withGrant(id int64) context.Context {
	return WithApprovalGrant(context.Background(), ApprovalGrant{ApprovalID: id})
}

// TC-X1 (REQ-4): a deny rule refuses the call; tool_runs says policy_denied; nothing runs.
func TestPolicyGate_DenyRecordsPolicyDenied(t *testing.T) {
	g := &ruleGate{tiers: map[nhpolicy.Tier]nhpolicy.Effect{nhpolicy.TierRead: nhpolicy.EffectAllow, nhpolicy.TierWrite: nhpolicy.EffectDeny, nhpolicy.TierDangerous: nhpolicy.EffectDeny}}
	e := newEnvGate(t, policy(), g, Limits{})
	r := e.exec.Call(context.Background(), 1, "write_a", json.RawMessage(repoIn))
	expectDenied(t, e, r, DenyPolicyDenied)
	if e.tools["write_a"].count() != 0 {
		t.Fatal("denied write ran")
	}
	r = e.exec.Call(context.Background(), 1, "danger_a", json.RawMessage(repoIn))
	expectDenied(t, e, r, DenyPolicyDenied)
	if r.Tier != tenant.TierDangerous {
		t.Fatalf("%+v", r)
	}
}

// TC-X2 (REQ-4): approval_required without a grant is denied with the model-facing text; nothing runs.
func TestPolicyGate_ApprovalRequiredWithoutGrant(t *testing.T) {
	e := newEnv(t, policy(), false, Limits{})
	r := e.exec.Call(context.Background(), 1, "danger_a", json.RawMessage(repoIn))
	expectDenied(t, e, r, DenyApprovalRequired)
	if r.Content != approvalRequiredText || !strings.Contains(r.Content, "owner must approve") || !strings.Contains(r.Content, "nothing was done") {
		t.Fatalf("model-facing text: %q", r.Content)
	}
	if e.tools["danger_a"].count() != 0 {
		t.Fatal("ran without approval")
	}
	// A grant id of zero or a missing grant is no grant.
	for _, ctx := range []context.Context{context.Background(), withGrant(0), withGrant(-3)} {
		if r := e.exec.Call(ctx, 1, "danger_a", json.RawMessage(repoIn)); r.Status != StatusDenied || r.Content != approvalRequiredText {
			t.Fatalf("%+v", r)
		}
	}
	if _, ok := ApprovalGrantFrom(context.Background()); ok {
		t.Fatal("grant from empty ctx")
	}
}

// TC-X3 (REQ-4): allow runs, and the engine got ids, names and a hash only.
func TestPolicyGate_AllowRunsAndRequestShape(t *testing.T) {
	g := v1Gate(false)
	e := newEnvGate(t, policy(), g, Limits{})
	e.exec = NewExecutor(e.exec.reg, policy(), DefaultLimits(), e.store, upperSan{}, 42, "ws-1", WithPolicy(g, nhpolicy.ActorBehavior), WithExecutionID(77))
	in := json.RawMessage(`{"repo":"o/r","number":12,"body":"SECRET text that must never reach the engine"}`)
	if r := e.exec.Call(context.Background(), 1, "write_a", in); r.IsError || r.Status != StatusOK {
		t.Fatalf("%+v", r)
	}
	reqs := g.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d decisions for one call", len(reqs))
	}
	q := reqs[0]
	if q.WorkspaceID != "ws-1" || q.ActorKind != nhpolicy.ActorBehavior || q.Action != "write_a" || q.Tier != nhpolicy.TierWrite ||
		q.ResourceKey != "o/r#12" || q.ExecutionID != 77 || q.InputSHA256 != InputSHA256(in) || q.Revalidate || q.GrantRejected {
		t.Fatalf("%+v", q)
	}
	if strings.Contains(strings.Join([]string{q.WorkspaceID, q.ActorKind, q.Action, q.ResourceKey, q.InputSHA256}, "|"), "SECRET") {
		t.Fatal("input text reached the engine")
	}
}

// TC-X4 (REQ-4): no gate means every non-read call is denied; reads still run.
func TestPolicyGate_NilGateDeniesNonRead(t *testing.T) {
	e := newEnv(t, policy(), true, Limits{})
	e.exec = NewExecutor(e.exec.reg, policy(), DefaultLimits(), e.store, upperSan{}, 42, "ws-1")
	for _, n := range []string{"write_a", "danger_a"} {
		expectDenied(t, e, e.exec.Call(context.Background(), 1, n, json.RawMessage(repoIn)), DenyPolicyDenied)
		if e.tools[n].count() != 0 {
			t.Fatalf("%s ran without a gate", n)
		}
	}
	if r := e.exec.Call(context.Background(), 1, "read_a", json.RawMessage(`{}`)); r.IsError || e.tools["read_a"].count() != 1 {
		t.Fatalf("read without a gate: %+v", r)
	}
	// A typed-nil option and a nil option are ignored, not a crash.
	ex := NewExecutor(e.exec.reg, policy(), DefaultLimits(), e.store, upperSan{}, 42, "ws-1", nil, WithPolicy(nil, nhpolicy.ActorAgent))
	if r := ex.Call(context.Background(), 1, "write_a", json.RawMessage(repoIn)); r.Status != StatusDenied {
		t.Fatalf("%+v", r)
	}
}

// TC-X5 (REQ-4): an unusable decision is a deny.
func TestPolicyGate_InvalidEffectDenied(t *testing.T) {
	g := &fnGate{fn: func(nhpolicy.Request) nhpolicy.Effect { return "" }}
	e := newEnvGate(t, policy(), g, Limits{})
	expectDenied(t, e, e.exec.Call(context.Background(), 1, "write_a", json.RawMessage(repoIn)), DenyPolicyDenied)
	g2 := &fnGate{fn: func(nhpolicy.Request) nhpolicy.Effect { return "maybe" }}
	e2 := newEnvGate(t, policy(), g2, Limits{})
	expectDenied(t, e2, e2.exec.Call(context.Background(), 1, "write_a", json.RawMessage(repoIn)), DenyPolicyDenied)
}

// TC-X6 (REQ-4): read tools never ask the engine; earlier denials never reach it.
func TestPolicyGate_ReadsAndEarlierDenialsSkipTheEngine(t *testing.T) {
	g := v1Gate(true)
	e := newEnvGate(t, policy(), g, Limits{})
	for _, n := range []string{"read_a"} {
		if r := e.exec.Call(context.Background(), 1, n, json.RawMessage(`{}`)); r.IsError {
			t.Fatalf("%+v", r)
		}
	}
	if r := e.exec.Call(context.Background(), 1, "read_repo", json.RawMessage(repoIn)); r.IsError {
		t.Fatalf("%+v", r)
	}
	if n := len(g.requests()); n != 0 {
		t.Fatalf("%d engine calls for read tools", n)
	}
	// Repo grant narrows first: tool_not_allowed, no engine call.
	p := policy()
	rg := p.Repos[repoName]
	rg.Allow = []string{"write_b"}
	p.Repos[repoName] = rg
	p.Enabled = append(p.Enabled, "write_a")
	e2 := newEnvGate(t, p, g, Limits{})
	expectDenied(t, e2, e2.exec.Call(context.Background(), 1, "write_a", json.RawMessage(repoIn)), "tool_not_allowed")
	expectDenied(t, e2, e2.exec.Call(context.Background(), 1, "rm_rf", json.RawMessage(`{}`)), "unknown_tool")
	if n := len(g.requests()); n != 0 {
		t.Fatalf("%d engine calls after earlier denials", n)
	}
	// An allow rule never widens the repo grant (policy only narrows).
	if e2.tools["write_a"].count() != 0 {
		t.Fatal("ran")
	}
}

// TC-X7 (REQ-4): every denial gives the write/dangerous budget back.
func TestPolicyGate_BudgetRefundedOnEachDenial(t *testing.T) {
	g := &ruleGate{tiers: map[nhpolicy.Tier]nhpolicy.Effect{nhpolicy.TierRead: nhpolicy.EffectAllow, nhpolicy.TierWrite: nhpolicy.EffectDeny, nhpolicy.TierDangerous: nhpolicy.EffectApprovalRequired}}
	e := newEnvGate(t, policy(), g, Limits{})
	// MaxWritesPerJob is 2 and MaxDangerous is 1: three denials in a row must not exhaust either.
	for i := 0; i < 3; i++ {
		expectDenied(t, e, e.exec.Call(context.Background(), 1, "write_a", json.RawMessage(`{"repo":"o/r","i":`+string(rune('0'+i))+`}`)), DenyPolicyDenied)
		expectDenied(t, e, e.exec.Call(context.Background(), 1, "danger_a", json.RawMessage(`{"repo":"o/r","i":`+string(rune('0'+i))+`}`)), DenyApprovalRequired)
	}
	g.tiers[nhpolicy.TierWrite], g.tiers[nhpolicy.TierDangerous] = nhpolicy.EffectAllow, nhpolicy.EffectAllow
	for i, n := range []string{"write_a", "danger_a"} {
		if r := e.exec.Call(context.Background(), 1, n, json.RawMessage(repoIn)); r.IsError {
			t.Fatalf("call %d (%s) after refunds: %+v", i, n, r)
		}
	}
	// And the real budgets still bind afterwards.
	expectDenied(t, e, e.exec.Call(context.Background(), 1, "write_c", json.RawMessage(repoIn)), "write_budget")
}

// TC-X8 (REQ-4): the escalated tier is what the engine is asked about.
func TestPolicyGate_EscalatedTierAsked(t *testing.T) {
	g := v1Gate(false)
	e := newEnvGate(t, policy(), g, Limits{})
	if r := e.exec.Call(context.Background(), 1, "push", json.RawMessage(`{"repo":"o/r","branch":"nexus/x"}`)); r.IsError {
		t.Fatalf("%+v", r)
	}
	r := e.exec.Call(context.Background(), 1, "push", json.RawMessage(`{"repo":"o/r","branch":"main"}`))
	expectDenied(t, e, r, DenyApprovalRequired)
	reqs := g.requests()
	if len(reqs) != 2 || reqs[0].Tier != nhpolicy.TierWrite || reqs[1].Tier != nhpolicy.TierDangerous || reqs[1].Action != "push" {
		t.Fatalf("%+v", reqs)
	}
	if e.tools["push"].count() != 1 {
		t.Fatal("escalated push ran without approval")
	}
}

// TC-X9 (REQ-4): dry run asks the engine too and is no bypass.
func TestPolicyGate_DryRunStillAsksEngine(t *testing.T) {
	p := policy()
	p.DryRun = true
	g := v1Gate(false)
	e := newEnvGate(t, p, g, Limits{})
	expectDenied(t, e, e.exec.Call(context.Background(), 1, "danger_a", json.RawMessage(repoIn)), DenyApprovalRequired)
	if r := e.exec.Call(context.Background(), 1, "write_a", json.RawMessage(repoIn)); r.Status != StatusDryRun {
		t.Fatalf("%+v", r)
	}
	if len(g.requests()) != 2 {
		t.Fatalf("%d", len(g.requests()))
	}
}

// TC-G1 (REQ-7): a valid grant runs the call once; the engine is asked again right before the effect.
func TestGrant_ValidRunsOnceAndRevalidates(t *testing.T) {
	g, store := approvalGate(), newGrantStore()
	gr := goodGrant()
	store.grants[7] = gr
	e, ex := grantEnv(t, g, store)
	r := ex.Call(withGrant(7), 1, "danger_a", json.RawMessage(repoIn))
	if r.IsError || r.Status != StatusOK || e.tools["danger_a"].count() != 1 || gr.consumed != 1 {
		t.Fatalf("%+v consumed=%d", r, gr.consumed)
	}
	reqs := g.requests()
	if len(reqs) != 2 || reqs[0].Revalidate || !reqs[1].Revalidate || reqs[1].ApprovalID != 7 || reqs[1].ToolRunID == 0 || reqs[1].GrantRejected {
		t.Fatalf("%+v", reqs)
	}
	// A second, different call with the same (used) grant is refused.
	again := NewExecutor(e.exec.reg, policy(), DefaultLimits(), store, upperSan{}, 42, "ws-1", execOpts(g)...)
	r = again.Call(withGrant(7), 2, "danger_a", json.RawMessage(`{"repo":"o/r","again":true}`))
	if r.Status != StatusDenied || !strings.Contains(r.Content, DenyRevalidateDenied) || e.tools["danger_a"].count() != 1 {
		t.Fatalf("%+v", r)
	}
}

// TC-G2 (REQ-7): a grant that does not match the exact action is refused; nothing runs; the audit says revalidate_denied.
func TestGrant_MismatchRefused(t *testing.T) {
	mods := map[string]func(g *fakeGrant){
		"other action":     func(g *fakeGrant) { g.check.Action = "danger_b" },
		"other resource":   func(g *fakeGrant) { g.check.ResourceKey = "x/y" },
		"other input hash": func(g *fakeGrant) { g.check.InputSHA256 = InputSHA256(json.RawMessage(`{"repo":"o/r","n":2}`)) },
		"other execution":  func(g *fakeGrant) { g.check.ExecutionID = 9 },
		"other workspace":  func(g *fakeGrant) { g.check.WorkspaceID = "ws-2" },
		"expired":          func(g *fakeGrant) { g.expired = true },
		"not approved":     func(g *fakeGrant) { g.approved = false },
		"already consumed": func(g *fakeGrant) { g.consumed = 1 },
		"empty hash":       func(g *fakeGrant) { g.check.InputSHA256 = "" },
	}
	for name, mod := range mods {
		t.Run(name, func(t *testing.T) {
			g, store := approvalGate(), newGrantStore()
			gr := goodGrant()
			mod(gr)
			store.grants[7] = gr
			before := gr.consumed
			e, ex := grantEnv(t, g, store)
			r := ex.Call(withGrant(7), 1, "danger_a", json.RawMessage(repoIn))
			if r.Status != StatusDenied || !r.IsError || !strings.Contains(r.Content, DenyRevalidateDenied) || e.tools["danger_a"].count() != 0 {
				t.Fatalf("%+v", r)
			}
			if gr.consumed != before {
				t.Fatal("a rejected grant was consumed")
			}
			if row := store.last(); row.Status != "denied" || row.Code != DenyRevalidateDenied {
				t.Fatalf("audit %+v", row)
			}
			reqs := g.requests()
			last := reqs[len(reqs)-1]
			if !last.GrantRejected || last.ApprovalID != 0 {
				t.Fatalf("the rejection must be recorded as GrantRejected with no approval id: %+v", last)
			}
			// The dangerous budget is back: an allowed call of the same job still fits.
			g2 := &ruleGate{tiers: map[nhpolicy.Tier]nhpolicy.Effect{nhpolicy.TierDangerous: nhpolicy.EffectAllow}}
			ex.gate = g2
			if r := ex.Call(context.Background(), 2, "danger_b", json.RawMessage(repoIn)); r.IsError {
				t.Fatalf("budget not refunded: %+v", r)
			}
		})
	}
	t.Run("unknown approval id", func(t *testing.T) {
		g, store := approvalGate(), newGrantStore()
		e, ex := grantEnv(t, g, store)
		r := ex.Call(withGrant(404), 1, "danger_a", json.RawMessage(repoIn))
		if !strings.Contains(r.Content, DenyRevalidateDenied) || e.tools["danger_a"].count() != 0 {
			t.Fatalf("%+v", r)
		}
	})
}

// TC-G3 (REQ-7): the same idempotency key replays the earlier result and never consumes a second grant.
func TestGrant_RetryReplaysWithoutConsuming(t *testing.T) {
	g, store := approvalGate(), newGrantStore()
	gr := goodGrant()
	store.grants[7] = gr
	spare := goodGrant()
	spare.check.ApprovalID = 8
	store.grants[8] = spare
	e, ex := grantEnv(t, g, store)
	first := ex.Call(withGrant(7), 1, "danger_a", json.RawMessage(repoIn))
	retry := NewExecutor(e.exec.reg, policy(), DefaultLimits(), store, upperSan{}, 42, "ws-1", execOpts(g)...)
	second := retry.Call(withGrant(8), 2, "danger_a", json.RawMessage(`{ "repo" : "o/r" }`))
	if first.IsError || second.IsError || second.Status != StatusOK || e.tools["danger_a"].count() != 1 {
		t.Fatalf("first %+v second %+v count %d", first, second, e.tools["danger_a"].count())
	}
	if gr.consumed != 1 || spare.consumed != 0 {
		t.Fatalf("grants consumed: used=%d spare=%d", gr.consumed, spare.consumed)
	}
	// The replay also works with the used grant itself (a crash-retry of the same approval).
	third := NewExecutor(e.exec.reg, policy(), DefaultLimits(), store, upperSan{}, 42, "ws-1", execOpts(g)...).Call(withGrant(7), 3, "danger_a", json.RawMessage(repoIn))
	if third.IsError || e.tools["danger_a"].count() != 1 || gr.consumed != 1 {
		t.Fatalf("%+v", third)
	}
}

// TC-G4 (REQ-7): a rule that turned deny between consume and run stops the effect.
func TestGrant_RuleDeniesBetweenConsumeAndRun(t *testing.T) {
	g := &fnGate{fn: func(r nhpolicy.Request) nhpolicy.Effect {
		if r.Revalidate {
			return nhpolicy.EffectDeny
		}
		return nhpolicy.EffectApprovalRequired
	}}
	store := newGrantStore()
	gr := goodGrant()
	store.grants[7] = gr
	e, ex := grantEnv(t, g, store)
	r := ex.Call(withGrant(7), 1, "danger_a", json.RawMessage(repoIn))
	if r.Status != StatusDenied || !r.IsError || r.Content != "denied: "+DenyRevalidateDenied || e.tools["danger_a"].count() != 0 {
		t.Fatalf("%+v", r)
	}
	if row := store.last(); row.Status != "denied" || row.Code != DenyRevalidateDenied {
		t.Fatalf("the started run must end denied/revalidate_denied: %+v", row)
	}
	if gr.consumed != 1 {
		t.Fatalf("the grant is spent once consumed (no second chance): %d", gr.consumed)
	}
}

// TC-G5 (REQ-7): a store that cannot verify grants refuses approved calls.
func TestGrant_StoreWithoutGrantSupportRefuses(t *testing.T) {
	g := approvalGate()
	e := newEnvGate(t, policy(), g, Limits{})
	r := e.exec.Call(withGrant(7), 1, "danger_a", json.RawMessage(repoIn))
	if r.Status != StatusDenied || !strings.Contains(r.Content, DenyRevalidateDenied) || e.tools["danger_a"].count() != 0 {
		t.Fatalf("%+v", r)
	}
}

// TC-G6 (REQ-7): a grant never overrides a deny rule and is not used when the rule already allows.
func TestGrant_NeverOverridesDenyOrIsSpentByAllow(t *testing.T) {
	store := newGrantStore()
	gr := goodGrant()
	store.grants[7] = gr
	deny := &fnGate{fn: func(nhpolicy.Request) nhpolicy.Effect { return nhpolicy.EffectDeny }}
	e, ex := grantEnv(t, deny, store)
	r := ex.Call(withGrant(7), 1, "danger_a", json.RawMessage(repoIn))
	if r.Status != StatusDenied || !strings.Contains(r.Content, DenyPolicyDenied) || e.tools["danger_a"].count() != 0 || gr.consumed != 0 {
		t.Fatalf("%+v consumed=%d", r, gr.consumed)
	}
	allow := &fnGate{fn: func(nhpolicy.Request) nhpolicy.Effect { return nhpolicy.EffectAllow }}
	e2, ex2 := grantEnv(t, allow, store)
	if r := ex2.Call(withGrant(7), 1, "danger_a", json.RawMessage(repoIn)); r.IsError || e2.tools["danger_a"].count() != 1 {
		t.Fatalf("%+v", r)
	}
	if gr.consumed != 0 || store.grantedBegins != 0 {
		t.Fatalf("an allowed call consumed a grant: consumed=%d beginGranted=%d", gr.consumed, store.grantedBegins)
	}
}

// TC-G7 (REQ-7): ResourceKey and InputSHA256 are stable for equal inputs and differ for different targets.
func TestResourceKeyAndInputHash(t *testing.T) {
	tool := tierTool("write_a", tenant.TierWrite)
	a := json.RawMessage(`{"repo":"o/r","number":12,"body":"x"}`)
	b := json.RawMessage(` { "body" : "x", "number" : 12 , "repo":"o/r" } `)
	if InputSHA256(a) != InputSHA256(b) || ResourceKey(tool, a) != ResourceKey(tool, b) {
		t.Fatal("equal inputs give different hash or key")
	}
	if len(InputSHA256(a)) != 64 {
		t.Fatal(InputSHA256(a))
	}
	n13 := json.RawMessage(`{"repo":"o/r","number":13,"body":"x"}`)
	if InputSHA256(a) == InputSHA256(n13) || ResourceKey(tool, a) == ResourceKey(tool, n13) {
		t.Fatal("different PR numbers share hash or key")
	}
	if got := ResourceKey(tool, a); got != "o/r#12" {
		t.Fatalf("%q", got)
	}
	if got := ResourceKey(tool, json.RawMessage(`{"task_id":"abc123"}`)); got != "task:abc123" {
		t.Fatalf("%q", got)
	}
	if got := ResourceKey(tool, json.RawMessage(`{"repo":"o/r","task_id":"abc123"}`)); got != "o/r/task:abc123" {
		t.Fatalf("%q", got)
	}
	if got := ResourceKey(tool, json.RawMessage(`{"body":"x"}`)); got != "" {
		t.Fatalf("no target fields must give an empty key, got %q", got)
	}
	if got := ResourceKey(tool, json.RawMessage(`not json`)); got != "" {
		t.Fatalf("%q", got)
	}
}

// TC-G8 (REQ-4): resource keys are always printable ASCII without spaces and bounded.
func TestResourceKey_HostileInputsStayValid(t *testing.T) {
	tool := tierTool("write_a", tenant.TierWrite)
	for name, in := range map[string]string{
		"spaces":   `{"repo":"o/r with space","number":1}`,
		"utf8":     `{"repo":"owner/réo","number":1}`,
		"control":  `{"task_id":"a\nb"}`,
		"huge":     `{"repo":"` + strings.Repeat("a", 4000) + `"}`,
		"vietnam":  `{"task_id":"nhiệm vụ 🧠"}`,
		"negative": `{"repo":"o/r","number":-5}`,
		"float":    `{"repo":"o/r","number":1.5}`,
	} {
		k := ResourceKey(tool, json.RawMessage(in))
		if len(k) > nhpolicy.MaxResourceKeyBytes {
			t.Fatalf("%s: key too long (%d)", name, len(k))
		}
		for i := 0; i < len(k); i++ {
			if k[i] < 0x21 || k[i] > 0x7e {
				t.Fatalf("%s: key %q is not printable ASCII without spaces", name, k)
			}
		}
	}
	a := ResourceKey(tool, json.RawMessage(`{"repo":"o/r a"}`))
	b := ResourceKey(tool, json.RawMessage(`{"repo":"o/r b"}`))
	if a == b {
		t.Fatal("two different hostile repos share a key")
	}
	if ResourceKey(tool, json.RawMessage(`{"repo":"o/r","number":-5}`)) != "o/r" {
		t.Fatal("a non-positive number must not appear in the key")
	}
}

type keyedTool struct{ repoTool }

func (keyedTool) ResourceKey(json.RawMessage) string { return "custom key with spaces" }

// TC-G9 (REQ-4): a tool's own ResourceKey is honoured, then made safe.
func TestResourceKey_ToolOverrideIsBounded(t *testing.T) {
	k := ResourceKey(keyedTool{tierTool("w", tenant.TierWrite)}, json.RawMessage(`{"repo":"o/r"}`))
	if !strings.HasPrefix(k, "h:") || strings.ContainsAny(k, " ") {
		t.Fatalf("%q", k)
	}
}
