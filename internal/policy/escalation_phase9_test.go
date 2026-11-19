package policy

import "testing"

// Phase 9 Batch B (Review A note): a call whose tool tier was raised for its
// input gets the stricter of the action rule and the raised tier's default,
// with the reason tier_escalated.

func p9EscReq(escalated bool) Request {
	return Request{WorkspaceID: "w", ActorKind: ActorAgent, Action: "git_push", Tier: TierDangerous, ResourceKey: "o/r", Escalated: escalated}
}

// TC-TE-1 (REQ-2): escalation turns an allow rule into the raised tier's stricter effect; without it the rule stands.
func TestP9Resolve_TierEscalationIsStricter(t *testing.T) {
	known := NewKnownActions("git_push")
	mk := func(rule, dangerous Effect) *RuleSet {
		rs := SafeRuleSet()
		rs.Defaults = map[Tier]Effect{TierRead: EffectAllow, TierWrite: EffectAllow, TierDangerous: dangerous}
		rs.Actions = map[string]Effect{"git_push": rule}
		return &rs
	}
	cases := []struct {
		name       string
		rs         *RuleSet
		escalated  bool
		wantEffect Effect
		wantReason string
		wantKey    string
	}{
		{"allow rule, escalated, dangerous needs approval", mk(EffectAllow, EffectApprovalRequired), true, EffectApprovalRequired, ReasonTierEscalated, "defaults.dangerous"},
		{"allow rule, escalated, dangerous denied", mk(EffectAllow, EffectDeny), true, EffectDeny, ReasonTierEscalated, "defaults.dangerous"},
		{"allow rule, not escalated", mk(EffectAllow, EffectApprovalRequired), false, EffectAllow, ReasonActionRule, "actions.git_push"},
		{"deny rule, escalated stays the rule", mk(EffectDeny, EffectApprovalRequired), true, EffectDeny, ReasonActionRule, "actions.git_push"},
		{"approval rule, escalated, dangerous allow keeps the rule", mk(EffectApprovalRequired, EffectAllow), true, EffectApprovalRequired, ReasonActionRule, "actions.git_push"},
		{"approval rule, escalated, dangerous deny", mk(EffectApprovalRequired, EffectDeny), true, EffectDeny, ReasonTierEscalated, "defaults.dangerous"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Resolve(tc.rs, 3, "", known, p9EscReq(tc.escalated))
			if d.Effect != tc.wantEffect || d.Reason != tc.wantReason || d.RuleKey != tc.wantKey {
				t.Fatalf("got %s/%s/%s want %s/%s/%s", d.Effect, d.Reason, d.RuleKey, tc.wantEffect, tc.wantReason, tc.wantKey)
			}
		})
	}
	// An escalation can never loosen: for every pair of effects the result is at least as strict as both.
	effects := []Effect{EffectAllow, EffectApprovalRequired, EffectDeny}
	for _, rule := range effects {
		for _, def := range effects {
			d := Resolve(mk(rule, def), 1, "", known, p9EscReq(true))
			if Stricter(rule, def) != d.Effect {
				t.Errorf("rule %s default %s: got %s want %s", rule, def, d.Effect, Stricter(rule, def))
			}
		}
	}
	// The built-in safe set (no stored rule set) also never loosens an escalated call.
	if d := Resolve(nil, 0, ReasonFallbackNoSet, known, p9EscReq(true)); d.Effect != EffectDeny {
		t.Fatalf("safe set, dangerous escalated: %s", d.Effect)
	}
}
