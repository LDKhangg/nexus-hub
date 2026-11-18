package agent

import (
	"context"

	nhpolicy "github.com/LDKhangg/nexus-hub/internal/policy"
)

// allowWritesGate is a fake policy gate for tests that are not about the
// policy engine: it allows everything.
type allowWritesGate struct{}

func (allowWritesGate) Decide(context.Context, nhpolicy.Request) (nhpolicy.Decision, error) {
	return nhpolicy.Decision{Effect: nhpolicy.EffectAllow}, nil
}
