package behavior

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/agent/tools"
	nhpolicy "github.com/LDKhangg/nexus-hub/internal/policy"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// Phase 9 Batch A: the seeded v1 keeps the behavior run action and the
// draft poster working, and a missing gate denies (REQ-4, REQ-8).

type p9Store struct {
	mu      sync.Mutex
	set     nhpolicy.StoredRuleSet
	records []nhpolicy.DecisionRecord
}

func (s *p9Store) ActiveRuleSet(context.Context, string) (nhpolicy.StoredRuleSet, bool, error) {
	return s.set, true, nil
}
func (s *p9Store) RecordDecision(_ context.Context, r nhpolicy.DecisionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r)
	return nil
}

func p9V1Engine(t *testing.T, names ...string) (*nhpolicy.Engine, *p9Store) {
	t.Helper()
	body, sum, err := nhpolicy.Canonical(nhpolicy.DefaultV1())
	if err != nil {
		t.Fatal(err)
	}
	st := &p9Store{set: nhpolicy.StoredRuleSet{Version: 1, Schema: nhpolicy.SchemaV1, Body: body, BodySHA256: sum, Active: true}}
	return nhpolicy.NewEngine(st, nhpolicy.NewKnownActions(names...)), st
}

type p9Tool struct {
	name string
	mu   sync.Mutex
	ran  int
}

func (p *p9Tool) Spec() tools.ToolSpec {
	return tools.ToolSpec{Name: p.name, Tier: tenant.TierWrite, InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (p *p9Tool) Run(context.Context, tools.Call) (tools.ToolOutput, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ran++
	return tools.ToolOutput{Text: "ok", Summary: "ok"}, nil
}

type p9Audit struct {
	mu    sync.Mutex
	next  int64
	codes []string
}

func (a *p9Audit) Begin(context.Context, tools.RunStart) (int64, *tools.PriorRun, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.next++
	return a.next, nil, nil
}
func (a *p9Audit) Finish(context.Context, tools.RunFinish) error { return nil }
func (a *p9Audit) Deny(_ context.Context, _ tools.RunStart, code string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.codes = append(a.codes, code)
	return nil
}

type p9San struct{}

func (p9San) CleanOutbound(s string, _ bool) string { return s }

// TC-N1 (REQ-8): under the seeded v1 a behavior run action write runs as before, and the decision names the behavior actor.
func TestPhase9_V1KeepsRunActionWriteWorking(t *testing.T) {
	eng, st := p9V1Engine(t, "clickup_post_comment")
	tool := &p9Tool{name: "clickup_post_comment"}
	reg, err := tools.NewRegistry(tool)
	if err != nil {
		t.Fatal(err)
	}
	audit := &p9Audit{}
	factory := ActionExecutorFactory(reg, audit, p9San{}, tools.DefaultLimits(), eng)
	pol := tenant.ToolPolicyConfig{Enabled: []string{"clickup_post_comment"}, MaxWritesPerJob: 2}
	caller := factory(Run{ID: 5, WorkspaceID: "ws-1"}, "scope", pol, "ref")
	r := caller.Call(context.Background(), 1, "clickup_post_comment", json.RawMessage(`{"task_id":"86abc","text":"hi"}`))
	if r.IsError || r.Status != tools.StatusOK || tool.ran != 1 {
		t.Fatalf("%+v ran=%d", r, tool.ran)
	}
	if len(st.records) != 1 || st.records[0].ActorKind != nhpolicy.ActorBehavior || st.records[0].Effect != nhpolicy.EffectAllow || st.records[0].RuleSetVersion != 1 {
		t.Fatalf("%+v", st.records)
	}
}

// TC-N2 (REQ-4): the draft poster asks as actor draft; a nil gate denies the write for run actions and drafts.
func TestPhase9_DraftActorAndNilGate(t *testing.T) {
	eng, st := p9V1Engine(t, "clickup_post_comment")
	tool := &p9Tool{name: "clickup_post_comment"}
	reg, _ := tools.NewRegistry(tool)
	pol := tenant.ToolPolicyConfig{Enabled: []string{"clickup_post_comment"}, MaxWritesPerJob: 2}
	in := json.RawMessage(`{"task_id":"86abc","text":"hi"}`)
	if r := DraftExecutorFactory(reg, &p9Audit{}, p9San{}, tools.DefaultLimits(), eng)("ws-1", 9, pol, "ref").Call(context.Background(), 1, "clickup_post_comment", in); r.IsError || tool.ran != 1 {
		t.Fatalf("%+v", r)
	}
	if len(st.records) != 1 || st.records[0].ActorKind != nhpolicy.ActorDraft {
		t.Fatalf("%+v", st.records)
	}
	for name, call := range map[string]func(audit *p9Audit) tools.CallResult{
		"run action": func(a *p9Audit) tools.CallResult {
			return ActionExecutorFactory(reg, a, p9San{}, tools.DefaultLimits(), nil)(Run{ID: 5, WorkspaceID: "ws-1"}, "s", pol, "ref").Call(context.Background(), 1, "clickup_post_comment", in)
		},
		"draft": func(a *p9Audit) tools.CallResult {
			return DraftExecutorFactory(reg, a, p9San{}, tools.DefaultLimits(), nil)("ws-1", 9, pol, "ref").Call(context.Background(), 1, "clickup_post_comment", in)
		},
	} {
		a := &p9Audit{}
		before := tool.ran
		r := call(a)
		if !r.IsError || r.Status != tools.StatusDenied || tool.ran != before || len(a.codes) != 1 || a.codes[0] != tools.DenyPolicyDenied {
			t.Fatalf("%s: %+v codes=%v", name, r, a.codes)
		}
	}
}

// TC-N3 (REQ-4): a read-only run executor has no gate, and a non-read call through it is denied.
func TestPhase9_ReadOnlyExecutorDeniesWrites(t *testing.T) {
	tool := &p9Tool{name: "clickup_post_comment"}
	reg, _ := tools.NewRegistry(tool)
	audit := &p9Audit{}
	pol := tenant.ToolPolicyConfig{Enabled: []string{"clickup_post_comment"}, MaxWritesPerJob: 2}
	caller := ExecutorFactory(reg, audit, p9San{}, tools.DefaultLimits())(Run{ID: 1, WorkspaceID: "ws-1"}, pol, "ref", 4000, 4)
	r := caller.Call(context.Background(), 1, "clickup_post_comment", json.RawMessage(`{"task_id":"1"}`))
	if !r.IsError || tool.ran != 0 || strings.Contains(r.Content, "ok") {
		t.Fatalf("%+v", r)
	}
}
