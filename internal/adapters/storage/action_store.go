package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ActionStore is the outbound_actions queue.
type ActionStore struct{ q *sqlc.Queries }

func NewActionStore(pool *pgxpool.Pool) (*ActionStore, error) {
	if pool == nil {
		return nil, errors.New("action store requires pool")
	}
	return &ActionStore{q: sqlc.New(pool)}, nil
}

var _ relay.ActionStore = (*ActionStore)(nil)

func (s *ActionStore) Claim(ctx context.Context, now time.Time) (*relay.OutboundAction, error) {
	at := pgtype.Timestamptz{Time: now, Valid: true}
	if err := s.q.ExpireFinalActionClaims(ctx, at); err != nil {
		return nil, err
	}
	tok, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	token, _ := parseDBID(tok.String())
	row, err := s.q.ClaimOutboundAction(ctx, sqlc.ClaimOutboundActionParams{Token: token, AtTime: at})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var payload struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(row.Payload, &payload)
	return &relay.OutboundAction{ID: row.ID, ClaimToken: tok.String(), Attempts: int(row.Attempts), WorkspaceID: row.WorkspaceID.String(), IntegrationID: row.IntegrationID.String(), Text: payload.Text}, nil
}

func (s *ActionStore) token(a relay.OutboundAction) (pgtype.UUID, error) {
	t, err := parseDBID(a.ClaimToken)
	if err != nil {
		return t, relay.ErrStaleActionClaim
	}
	return t, nil
}

func (s *ActionStore) ResolveTarget(ctx context.Context, a relay.OutboundAction) (tenant.TelegramTargetConfig, error) {
	token, err := s.token(a)
	if err != nil {
		return tenant.TelegramTargetConfig{}, err
	}
	row, err := s.q.GetActionClaim(ctx, sqlc.GetActionClaimParams{ID: a.ID, Token: token})
	if errors.Is(err, pgx.ErrNoRows) {
		return tenant.TelegramTargetConfig{}, relay.ErrStaleActionClaim
	}
	if err != nil {
		return tenant.TelegramTargetConfig{}, err
	}
	invalid := &relay.DeliveryError{Code: "invalid_target"}
	target, err := lookupNoticeIntegration(ctx, s.q, row.IntegrationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return tenant.TelegramTargetConfig{}, invalid
	}
	if err != nil {
		return tenant.TelegramTargetConfig{}, err
	}
	if target.WorkspaceID != row.WorkspaceID.String() || !target.IsEnabled || target.Platform != "telegram" ||
		(target.Role != tenant.IntegrationRoleControl && target.Role != tenant.IntegrationRoleBoth) {
		return tenant.TelegramTargetConfig{}, invalid
	}
	cfg, err := tenant.ParseTelegramTargetConfig(target.Config)
	if err != nil {
		return tenant.TelegramTargetConfig{}, invalid
	}
	return cfg, nil
}

func (s *ActionStore) done(n int64, err error) error {
	if err != nil {
		return err
	}
	if n == 0 {
		return relay.ErrStaleActionClaim
	}
	return nil
}

func (s *ActionStore) MarkSent(ctx context.Context, a relay.OutboundAction, messageID int64) error {
	token, err := s.token(a)
	if err != nil {
		return err
	}
	return s.done(s.q.MarkActionSent(ctx, sqlc.MarkActionSentParams{ID: a.ID, Token: token, MessageID: messageID}))
}

func (s *ActionStore) MarkRetry(ctx context.Context, a relay.OutboundAction, at time.Time, code string) error {
	token, err := s.token(a)
	if err != nil {
		return err
	}
	return s.done(s.q.MarkActionRetry(ctx, sqlc.MarkActionRetryParams{ID: a.ID, Token: token, NextAt: pgtype.Timestamptz{Time: at, Valid: true}, Code: code}))
}

func (s *ActionStore) MarkFailed(ctx context.Context, a relay.OutboundAction, code string) error {
	token, err := s.token(a)
	if err != nil {
		return err
	}
	return s.done(s.q.MarkActionFailed(ctx, sqlc.MarkActionFailedParams{ID: a.ID, Token: token, Code: code}))
}
