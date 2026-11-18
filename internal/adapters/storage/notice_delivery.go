package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/domain"
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
	r.resolveNoticeNames(ctx, job)
	return job, nil
}

// resolveNoticeNames fills the display names a notice shows (the sender and
// an added assignee) from the people directory; the stored event keeps no
// names. Unknown accounts keep the notice's fallback.
func (r *NoticeRepository) resolveNoticeNames(ctx context.Context, job *relay.NoticeJob) {
	e := &job.Event
	if r.people == nil || job.WorkspaceID == "" {
		return
	}
	if e.SenderName == "" && e.SenderID != "" {
		if name, ok := r.people.DisplayName(ctx, job.WorkspaceID, "clickup", e.SenderID); ok {
			e.SenderName = name
		}
	}
	if id := e.Metadata["assignee_added"]; id != "" && e.Metadata["assignee_name"] == "" {
		if name, ok := r.people.DisplayName(ctx, job.WorkspaceID, "clickup", id); ok {
			e.Metadata["assignee_name"] = name
		}
	}
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
	// One owner chat per workspace: owner_chat_id, else chat_id.
	conf := tenant.OwnerTargetConfig(target.Config)
	config, err := tenant.ParseTelegramTargetConfig(conf)
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
	link, linked := noticeCommentLink(job, receipt)
	if !linked {
		count, err := r.queries.MarkNoticeSent(ctx, sqlc.MarkNoticeSentParams{ID: job.ID, Token: token, MessageID: receipt.MessageID})
		return noticeTransition(count, err)
	}
	// A notice that shows a ClickUp comment records its message link with
	// the sent state (Phase 10 REQ-5).
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("transition notice: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	q := r.queries.WithTx(tx)
	count, err := q.MarkNoticeSent(ctx, sqlc.MarkNoticeSentParams{ID: job.ID, Token: token, MessageID: receipt.MessageID})
	if err := noticeTransition(count, err); err != nil {
		return err
	}
	if err := insertCommentLink(ctx, q, link); err != nil {
		return fmt.Errorf("record message link: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("transition notice: %w", err)
	}
	return nil
}

// noticeCommentLink returns the message link of a delivered notice that
// shows a ClickUp comment; ok is false for other notices or a receipt
// without a chat.
func noticeCommentLink(job relay.NoticeJob, receipt relay.DeliveryReceipt) (behavior.CommentLink, bool) {
	if job.Event.EventType != domain.EventCommandPosted || receipt.ChatID == 0 {
		return behavior.CommentLink{}, false
	}
	commentID, ok := behavior.CommentIDFromMessageRef(job.Event.ExternalMessageID)
	if !ok {
		return behavior.CommentLink{}, false
	}
	l := behavior.CommentLink{WorkspaceID: job.WorkspaceID, IntegrationID: job.TargetIntegrationID, ChatID: receipt.ChatID, MessageID: receipt.MessageID, TaskID: job.Event.ExternalID, CommentID: commentID}
	return l, behavior.ValidCommentLink(l)
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
