package policy

import (
	"context"
	"time"
)

// GateRequest asks for an approval of kind policy_gate bound to one exact
// action (REQ-6). Ids, names and a hash only.
type GateRequest struct {
	WorkspaceID string
	Action      string
	ResourceKey string
	// InputSHA256 is the hex SHA-256 of the action's canonical input.
	InputSHA256 string
	// ExecutionID is the waiting workflow execution, or 0.
	ExecutionID int64
	// RequestedUserID is the workspace member the gate is asked for
	// (users.id), or "".
	RequestedUserID string
	// ExpiresAt is now plus the rule set's approval TTL; the store caps
	// it at the execution's hard deadline.
	ExpiresAt time.Time
	// Now is the time an existing open gate's expiry is compared with;
	// zero means the current time.
	Now time.Time
}

// Gate is a stored policy_gate approval.
type Gate struct {
	ID          int64
	Nonce       string
	ExpiresAt   time.Time
	ExecutionID int64
	// Created is false when an open gate for the same workspace, action,
	// resource key and input hash already existed and is returned instead.
	Created bool
	// ClosedID is the open gate for the same action that this call expired
	// first because it was past its expiry or its execution had ended (0
	// for none); its message still shows the buttons.
	ClosedID int64
}

// GateStore creates policy_gate approvals. Granting is verified and
// consumed by the tool run store (tools.GrantedRunStore).
type GateStore interface {
	CreateGate(ctx context.Context, req GateRequest) (Gate, error)
}

// GateExpiry is the expiry of a gate created now under decision d.
func GateExpiry(now time.Time, d Decision) time.Time {
	ttl := d.ApprovalTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return now.Add(ttl)
}
