package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (r *NoticeRepository) Claim(ctx context.Context, now time.Time) (*relay.NoticeJob, error) {
	at := pgtype.Timestamptz{Time: now, Valid: true}
	if err := r.queries.ExpireFinalNoticeClaims(ctx, at); err != nil {
		return nil, fmt.Errorf("expire claims: %w", err)
	}
	token, err := r.newID()
	if err != nil {
		return nil, err
	}
	id, err := parseDBID(token)
	if err != nil {
		return nil, err
	}
	row, err := r.queries.ClaimNotice(ctx, sqlc.ClaimNoticeParams{Token: id, AtTime: at})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim notice: %w", err)
	}
	job := &relay.NoticeJob{ID: row.ID, ClaimToken: row.ClaimToken.String(), Attempts: int(row.Attempts), TargetIntegrationID: row.TargetIntegrationID.String(), WorkspaceID: row.WorkspaceID.String()}
	if err := json.Unmarshal(row.Event, &job.Event); err != nil {
		// A malformed stored event must not keep cycling through expired leases.
		return nil, r.MarkFailed(ctx, *job, "invalid_stored_event")
	}
	return job, nil
}
func (r *NoticeRepository) ResolveTarget(ctx context.Context, job relay.NoticeJob) (tenant.TelegramTargetConfig, error) {
	token, err := parseDBID(job.ClaimToken)
	if err != nil {
		return tenant.TelegramTargetConfig{}, err
	}
	row, err := r.queries.GetNoticeClaim(ctx, sqlc.GetNoticeClaimParams{ID: job.ID, ClaimToken: token})
	if errors.Is(err, pgx.ErrNoRows) {
		return tenant.TelegramTargetConfig{}, relay.ErrStaleNoticeClaim
	}
	if err != nil {
		return tenant.TelegramTargetConfig{}, err
	}
	source, err := lookupNoticeIntegration(ctx, r.queries, row.SourceIntegrationID)
	if err != nil {
		return tenant.TelegramTargetConfig{}, err
	}
	target, err := lookupNoticeIntegration(ctx, r.queries, row.TargetIntegrationID)
	if err != nil {
		return tenant.TelegramTargetConfig{}, err
	}
	invalid := func() (tenant.TelegramTargetConfig, error) {
		return tenant.TelegramTargetConfig{}, &relay.DeliveryError{Code: "invalid_target"}
	}
	if row.WorkspaceID.String() != job.WorkspaceID || source.WorkspaceID != row.WorkspaceID.String() || tenant.ValidateClickUpSource(source) != nil || tenant.ValidateTelegramTarget(source, target) != nil {
		return invalid()
	}
	config, err := tenant.ParseTelegramTargetConfig(target.Config)
	if err != nil {
		return invalid()
	}
	return config, nil
}
func noticeTransition(count int64, err error) error {
	if err != nil {
		return fmt.Errorf("transition notice: %w", err)
	}
	if count != 1 {
		return relay.ErrStaleNoticeClaim
	}
	return nil
}
func (r *NoticeRepository) MarkSent(ctx context.Context, job relay.NoticeJob, receipt relay.DeliveryReceipt) error {
	if receipt.MessageID <= 0 {
		return errors.New("invalid delivery receipt")
	}
	token, err := parseDBID(job.ClaimToken)
	if err != nil {
		return err
	}
	count, err := r.queries.MarkNoticeSent(ctx, sqlc.MarkNoticeSentParams{ID: job.ID, Token: token, MessageID: receipt.MessageID})
	return noticeTransition(count, err)
}
func (r *NoticeRepository) MarkRetry(ctx context.Context, job relay.NoticeJob, next time.Time, code string) error {
	token, err := parseDBID(job.ClaimToken)
	if err != nil {
		return err
	}
	count, err := r.queries.MarkNoticeRetry(ctx, sqlc.MarkNoticeRetryParams{ID: job.ID, Token: token, NextAt: pgtype.Timestamptz{Time: next, Valid: true}, Code: code})
	return noticeTransition(count, err)
}
func (r *NoticeRepository) MarkFailed(ctx context.Context, job relay.NoticeJob, code string) error {
	token, err := parseDBID(job.ClaimToken)
	if err != nil {
		return err
	}
	count, err := r.queries.MarkNoticeFailed(ctx, sqlc.MarkNoticeFailedParams{ID: job.ID, Token: token, Code: code})
	return noticeTransition(count, err)
}
