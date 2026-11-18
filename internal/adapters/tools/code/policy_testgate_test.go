package codetools

import (
	"context"

	nhpolicy "github.com/LDKhangg/nexus-hub/internal/policy"
)

// approvalGate is a fake policy gate: writes allowed, dangerous calls
// need the owner's approval (rule set v1).
type approvalGate struct{}

func (approvalGate) Decide(_ context.Context, r nhpolicy.Request) (nhpolicy.Decision, error) {
	if r.Tier == nhpolicy.TierDangerous {
		return nhpolicy.Decision{Effect: nhpolicy.EffectApprovalRequired}, nil
	}
	return nhpolicy.Decision{Effect: nhpolicy.EffectAllow}, nil
}
