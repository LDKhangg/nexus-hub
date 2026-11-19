package storage

import (
	"context"

	nhpolicy "github.com/LDKhangg/nexus-hub/internal/policy"
)

// testGate is a fake policy gate for tests that are not about the policy
// engine: writes are allowed, dangerous calls get the configured effect.
// It reproduces the v1 behaviour (dangerous: approval_required).
type testGate struct{ dangerous nhpolicy.Effect }

func (g testGate) Decide(_ context.Context, r nhpolicy.Request) (nhpolicy.Decision, error) {
	if r.Tier == nhpolicy.TierDangerous {
		return nhpolicy.Decision{Effect: g.dangerous}, nil
	}
	return nhpolicy.Decision{Effect: nhpolicy.EffectAllow}, nil
}

var (
	gateAllowAll        = testGate{dangerous: nhpolicy.EffectAllow}
	gateDangerousNeedOK = testGate{dangerous: nhpolicy.EffectApprovalRequired}
)
