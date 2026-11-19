package policy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// Phase 9 Batch A: Engine.Decide over a fake store (REQ-2, REQ-3).

type memEngineStore struct {
	mu        sync.Mutex
	set       *StoredRuleSet
	loadErr   error
	recordErr error
	records   []DecisionRecord
}

func (s *memEngineStore) ActiveRuleSet(context.Context, string) (StoredRuleSet, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return StoredRuleSet{}, false, s.loadErr
	}
	if s.set == nil {
		return StoredRuleSet{}, false, nil
	}
	return *s.set, true, nil
}

func (s *memEngineStore) RecordDecision(_ context.Context, r DecisionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recordErr != nil {
		return s.recordErr
	}
	s.records = append(s.records, r)
	return nil
}

func (s *memEngineStore) recorded() []DecisionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]DecisionRecord(nil), s.records...)
}

func stored(t *testing.T, rs RuleSet, version int32) *StoredRuleSet {
	t.Helper()
	body, sum, err := Canonical(rs)
	if err != nil {
		t.Fatal(err)
	}
	return &StoredRuleSet{WorkspaceID: "ws", Version: version, Schema: SchemaV1, Body: body, BodySHA256: sum, Active: true}
}

var engineKnown = NewKnownActions("code_search", "github_pr_comment", "github_pr_merge")

func engineReq(action string, tier Tier) Request {
	return Request{WorkspaceID: "ws", ActorKind: ActorAgent, Action: action, Tier: tier, ResourceKey: "o/r#7", InputSHA256: strings.Repeat("a", 64), ExecutionID: 5}
}

// TC-E1 (REQ-2, REQ-3): v1 decides; exactly one decisions row per call, with ids/names/hash only.
func TestEngine_V1DecidesAndRecordsOneRowPerCall(t *testing.T) {
	st := &memEngineStore{set: stored(t, DefaultV1(), 1)}
	e := NewEngine(st, engineKnown)
	ctx := context.Background()
	cases := []struct {
		action string
		tier   Tier
		want   Effect
		rule   string
	}{
		{"code_search", TierRead, EffectAllow, "defaults.read"},
		{"github_pr_comment", TierWrite, EffectAllow, "defaults.write"},
		{"github_pr_merge", TierDangerous, EffectApprovalRequired, "actions.github_pr_merge"},
	}
	for i, c := range cases {
		d, err := e.Decide(ctx, engineReq(c.action, c.tier))
		if err != nil || d.Effect != c.want || d.RuleKey != c.rule || d.RuleSetVersion != 1 || d.ApprovalTTL != 24*time.Hour {
			t.Fatalf("%s: %+v %v", c.action, d, err)
		}
		recs := st.recorded()
		if len(recs) != i+1 {
			t.Fatalf("after %d calls there are %d rows", i+1, len(recs))
		}
		r := recs[i]
		if r.Action != c.action || r.Tier != c.tier || r.Effect != c.want || r.RuleSetVersion != 1 || r.ResourceKey != "o/r#7" ||
			r.ActorKind != ActorAgent || r.ExecutionID != 5 || r.InputSHA256 != strings.Repeat("a", 64) || r.WorkspaceID != "ws" || r.CreatedAt.IsZero() {
			t.Fatalf("row %+v", r)
		}
	}
}

// TC-E2 (REQ-2): no set, store error, invalid set and DB-edited set all use the safe set with a reason.
func TestEngine_FallbacksUseSafeSet(t *testing.T) {
	good := stored(t, DefaultV1(), 4)
	edited := *good
	edited.Body = []byte(strings.Replace(string(good.Body), `"write":"allow"`, `"write":"deny"`, 1))
	wrongSchema := *good
	wrongSchema.Schema = "policy/v9"
	unknownTool := stored(t, mustParse(t, "schema: policy/v1\ndefaults: {read: allow, write: allow, dangerous: allow}\nactions:\n  vanished_tool: allow\n"), 6)
	for name, c := range map[string]struct {
		st     *memEngineStore
		reason string
	}{
		"no set":       {&memEngineStore{}, ReasonFallbackNoSet},
		"store error":  {&memEngineStore{loadErr: errors.New("db down: password=hunter2")}, ReasonFallbackStoreErr},
		"edited in db": {&memEngineStore{set: &edited}, ReasonFallbackInvalid},
		"bad schema":   {&memEngineStore{set: &wrongSchema}, ReasonFallbackInvalid},
		"stale action": {&memEngineStore{set: unknownTool}, ReasonFallbackInvalid},
		"garbage body": {&memEngineStore{set: &StoredRuleSet{Schema: SchemaV1, Body: []byte("{"), BodySHA256: good.BodySHA256}}, ReasonFallbackInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			e := NewEngine(c.st, engineKnown)
			for tier, want := range map[Tier]Effect{TierRead: EffectAllow, TierWrite: EffectApprovalRequired, TierDangerous: EffectDeny} {
				action := map[Tier]string{TierRead: "code_search", TierWrite: "github_pr_comment", TierDangerous: "github_pr_merge"}[tier]
				d, err := e.Decide(context.Background(), engineReq(action, tier))
				if d.Effect != want || d.Reason != c.reason || d.RuleSetVersion != 0 {
					t.Fatalf("%s: %+v", tier, d)
				}
				if c.reason != ReasonFallbackNoSet && err == nil {
					t.Fatal("the load problem is not reported to the caller for logging")
				}
				if err != nil && strings.Contains(err.Error(), "hunter2") {
					t.Fatalf("error leaks the store error: %v", err)
				}
			}
			if n := len(c.st.recorded()); n != 3 {
				t.Fatalf("%d rows for 3 calls", n)
			}
		})
	}
}

// TC-E3 (REQ-2, REQ-3): a decision that cannot be recorded is deny (record_failed).
func TestEngine_RecordFailureDenies(t *testing.T) {
	st := &memEngineStore{set: stored(t, DefaultV1(), 1), recordErr: errors.New("insert failed: secret")}
	d, err := NewEngine(st, engineKnown).Decide(context.Background(), engineReq("github_pr_comment", TierWrite))
	if d.Effect != EffectDeny || d.Reason != ReasonRecordFailed || err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("%+v %v", d, err)
	}
	if d.RuleKey != "defaults.write" || d.RuleSetVersion != 1 {
		t.Fatalf("the failed record lost the rule that decided: %+v", d)
	}
	// A read is recorded too, so a failed record denies it as well.
	if d, _ := NewEngine(st, engineKnown).Decide(context.Background(), engineReq("code_search", TierRead)); d.Effect != EffectDeny {
		t.Fatalf("read allowed without audit: %+v", d)
	}
}

// TC-E4 (REQ-2): a nil engine, a zero engine and an engine with no store deny.
func TestEngine_NilAndZeroDeny(t *testing.T) {
	var nilEngine *Engine
	for name, e := range map[string]*Engine{"nil": nilEngine, "zero": {}, "no store": {Known: engineKnown}} {
		d, err := e.Decide(context.Background(), engineReq("code_search", TierRead))
		if d.Effect != EffectDeny || d.Reason != ReasonEngineUnavailable || err == nil {
			t.Fatalf("%s: %+v %v", name, d, err)
		}
	}
}

// TC-E5 (REQ-2, REQ-3): an invalid request is denied and still recorded, with the bad fields blanked.
func TestEngine_InvalidRequestRecordedBlanked(t *testing.T) {
	st := &memEngineStore{set: stored(t, DefaultV1(), 1)}
	e := NewEngine(st, engineKnown)
	bad := Request{WorkspaceID: "ws", ActorKind: "root", Action: "Bad Action!", Tier: "admin", ResourceKey: "has space é",
		InputSHA256: "not-a-hash", ExecutionID: -4, ToolRunID: -5, ApprovalID: -6}
	d, _ := e.Decide(context.Background(), bad)
	if d.Effect != EffectDeny || d.Reason != ReasonInvalidRequest {
		t.Fatalf("%+v", d)
	}
	recs := st.recorded()
	if len(recs) != 1 {
		t.Fatalf("%d rows", len(recs))
	}
	r := recs[0]
	if r.Action != "" || r.Tier != "" || r.ResourceKey != "" || r.ActorKind != "" || r.InputSHA256 != "" || r.ExecutionID != 0 || r.ToolRunID != 0 || r.ApprovalID != 0 || r.Effect != EffectDeny || r.Reason != ReasonInvalidRequest {
		t.Fatalf("row not blanked: %+v", r)
	}
	// An invalid request does not even read the rule set.
	if r.RuleSetVersion != 0 {
		t.Fatalf("%+v", r)
	}
}

// TC-E6 (REQ-2): the unknown action is denied even when v1 default allows its tier.
func TestEngine_UnknownActionDeniedAndRecorded(t *testing.T) {
	st := &memEngineStore{set: stored(t, DefaultV1(), 1)}
	d, _ := NewEngine(st, engineKnown).Decide(context.Background(), engineReq("brand_new_tool", TierWrite))
	if d.Effect != EffectDeny || d.Reason != ReasonUnknownAction {
		t.Fatalf("%+v", d)
	}
	if recs := st.recorded(); len(recs) != 1 || recs[0].Reason != ReasonUnknownAction || recs[0].Action != "brand_new_tool" {
		t.Fatalf("%+v", recs)
	}
}

// TC-E7 (REQ-2): a rule set change takes effect on the next decision, without a restart.
func TestEngine_NextDecisionSeesNewRuleSet(t *testing.T) {
	st := &memEngineStore{set: stored(t, DefaultV1(), 1)}
	e := NewEngine(st, engineKnown)
	r := engineReq("github_pr_comment", TierWrite)
	if d, _ := e.Decide(context.Background(), r); d.Effect != EffectAllow {
		t.Fatalf("%+v", d)
	}
	deny := mustParse(t, "schema: policy/v1\ndefaults: {read: allow, write: deny, dangerous: deny}\n")
	st.mu.Lock()
	st.set = stored(t, deny, 2)
	st.mu.Unlock()
	if d, _ := e.Decide(context.Background(), r); d.Effect != EffectDeny || d.RuleSetVersion != 2 {
		t.Fatalf("%+v", d)
	}
}

// TC-E8 (REQ-2): concurrent decisions each record exactly one row.
func TestEngine_ConcurrentDecisionsOneRowEach(t *testing.T) {
	st := &memEngineStore{set: stored(t, DefaultV1(), 1)}
	e := NewEngine(st, engineKnown)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Decide(context.Background(), engineReq("code_search", TierRead))
		}()
	}
	wg.Wait()
	if n := len(st.recorded()); n != 40 {
		t.Fatalf("%d rows for 40 decisions", n)
	}
}

// TC-E9 (REQ-7): the revalidation request is recorded with revalidate_denied under a deny rule.
func TestEngine_RevalidateDeniedRecorded(t *testing.T) {
	rs := mustParse(t, "schema: policy/v1\ndefaults: {read: allow, write: allow, dangerous: deny}\n")
	st := &memEngineStore{set: stored(t, rs, 7)}
	r := engineReq("github_pr_merge", TierDangerous)
	r.Revalidate, r.ToolRunID, r.ApprovalID = true, 9, 3
	d, _ := NewEngine(st, engineKnown).Decide(context.Background(), r)
	if d.Effect != EffectDeny || d.Reason != ReasonRevalidateDenied {
		t.Fatalf("%+v", d)
	}
	if recs := st.recorded(); len(recs) != 1 || recs[0].Reason != ReasonRevalidateDenied || recs[0].ToolRunID != 9 || recs[0].ApprovalID != 3 {
		t.Fatalf("%+v", recs)
	}
}

func TestGateExpiry(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	if got := GateExpiry(now, Decision{ApprovalTTL: 3 * time.Hour}); !got.Equal(now.Add(3 * time.Hour)) {
		t.Fatal(got)
	}
	if got := GateExpiry(now, Decision{}); !got.Equal(now.Add(24 * time.Hour)) {
		t.Fatalf("zero ttl must default to 24h, got %v", got)
	}
}
