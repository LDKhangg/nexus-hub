package storage

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"regexp"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/policy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	_ policy.GateStore = (*PolicyStore)(nil)

	gateHashRe     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	gateResourceRe = regexp.MustCompile(`^[\x21-\x7e]*$`)
)

// ErrInvalidGateRequest is returned by CreateGate for a request that does
// not fit the approval's shape; nothing is stored.
var ErrInvalidGateRequest = errors.New("policy gate: invalid request")

// CreateGate stores an open policy_gate approval bound to the request's
// workspace, action, resource key, input hash and execution, or returns
// the open one that already exists for the same workspace, action,
// resource key and input hash (Created false). An existing open gate is
// returned as stored, even when its expiry has passed and the sweep has
// not expired it yet; the caller compares ExpiresAt.
func (s *PolicyStore) CreateGate(ctx context.Context, req policy.GateRequest) (policy.Gate, error) {
	ws, err := parseDBID(req.WorkspaceID)
	if err != nil || !policy.ActionNameRe.MatchString(req.Action) || len(req.ResourceKey) > policy.MaxResourceKeyBytes ||
		!gateResourceRe.MatchString(req.ResourceKey) || !gateHashRe.MatchString(req.InputSHA256) || req.ExecutionID < 0 || req.ExpiresAt.IsZero() {
		return policy.Gate{}, ErrInvalidGateRequest
	}
	var user pgtype.UUID
	if req.RequestedUserID != "" {
		if user, err = parseDBID(req.RequestedUserID); err != nil {
			return policy.Gate{}, ErrInvalidGateRequest
		}
	}
	exec := pgtype.Int8{Int64: req.ExecutionID, Valid: req.ExecutionID > 0}
	// Two rounds: an open gate decided between the insert and the read
	// frees the index, so the second insert succeeds.
	for range 2 {
		nonce, err := gateNonce()
		if err != nil {
			return policy.Gate{}, err
		}
		row, err := s.q.CreatePolicyGateApproval(ctx, sqlc.CreatePolicyGateApprovalParams{
			WorkspaceID: ws, PolicyAction: req.Action, ResourceKey: req.ResourceKey, Nonce: nonce,
			ExpiresAt: pgtype.Timestamptz{Time: req.ExpiresAt, Valid: true}, InputSha256: req.InputSHA256,
			ExecutionID: exec, RequestedUserID: user,
		})
		if err == nil {
			return policy.Gate{ID: row.ID, Nonce: row.Nonce, ExpiresAt: row.ExpiresAt.Time, ExecutionID: row.ExecutionID, Created: true}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return policy.Gate{}, err
		}
		open, err := s.q.GetOpenPolicyGateApproval(ctx, sqlc.GetOpenPolicyGateApprovalParams{
			WorkspaceID: ws, PolicyAction: req.Action, ResourceKey: req.ResourceKey, InputSha256: req.InputSHA256,
		})
		if err == nil {
			return policy.Gate{ID: open.ID, Nonce: open.Nonce, ExpiresAt: open.ExpiresAt.Time, ExecutionID: open.ExecutionID}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return policy.Gate{}, err
		}
	}
	return policy.Gate{}, errors.New("policy gate: open gate changed concurrently")
}

// gateNonce is a random callback nonce (approvals.nonce shape).
func gateNonce() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
