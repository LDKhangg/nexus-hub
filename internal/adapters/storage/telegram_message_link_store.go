package storage

import (
	"context"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/jackc/pgx/v5/pgtype"
)

// Telegram message to ClickUp comment links (Phase 10 REQ-5). Ids only.

var (
	_ behavior.CommentLinkStore  = (*TelegramTopicStore)(nil)
	_ behavior.MessageLinkPruner = (*TelegramTopicStore)(nil)
)

// RecordCommentLink stores that a Telegram message shows a ClickUp comment.
// A second record of the same message keeps the first.
func (s *TelegramTopicStore) RecordCommentLink(ctx context.Context, l behavior.CommentLink) error {
	return insertCommentLink(ctx, s.q, l)
}

func insertCommentLink(ctx context.Context, q *sqlc.Queries, l behavior.CommentLink) error {
	if !behavior.ValidCommentLink(l) {
		return topicInvalid("message link")
	}
	ws, err := parseDBID(l.WorkspaceID)
	if err != nil {
		return topicInvalid("workspace_id")
	}
	integration, err := parseDBID(l.IntegrationID)
	if err != nil {
		return topicInvalid("integration_id")
	}
	_, err = q.InsertTelegramCommentLink(ctx, sqlc.InsertTelegramCommentLinkParams{
		WorkspaceID: ws, IntegrationID: integration, ChatID: l.ChatID, MessageID: l.MessageID,
		TaskID: l.TaskID, CommentID: l.CommentID,
	})
	return topicError(err)
}

// LookupCommentLink returns the ClickUp comment one Telegram message of the
// workspace's chat shows; behavior.ErrNotFound when none is known.
func (s *TelegramTopicStore) LookupCommentLink(ctx context.Context, workspaceID, integrationID string, chatID, messageID int64) (behavior.CommentLink, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.CommentLink{}, topicInvalid("workspace_id")
	}
	integration, err := parseDBID(integrationID)
	if err != nil {
		return behavior.CommentLink{}, topicInvalid("integration_id")
	}
	if chatID == 0 || messageID <= 0 {
		return behavior.CommentLink{}, behavior.ErrNotFound
	}
	row, err := s.q.GetTelegramCommentLink(ctx, sqlc.GetTelegramCommentLinkParams{
		WorkspaceID: ws, IntegrationID: integration, ChatID: chatID, MessageID: messageID,
	})
	if err != nil {
		return behavior.CommentLink{}, topicError(err)
	}
	return behavior.CommentLink{
		WorkspaceID: uuidString(ws), IntegrationID: uuidString(integration), ChatID: chatID, MessageID: messageID,
		TaskID: row.TaskID, CommentID: row.CommentID,
	}, nil
}

// PruneMessageLinks deletes the links created before before, in every
// workspace (a retention sweep).
func (s *TelegramTopicStore) PruneMessageLinks(ctx context.Context, before time.Time) (int64, error) {
	if before.IsZero() {
		return 0, nil
	}
	return s.q.PruneTelegramMessageLinks(ctx, pgtype.Timestamptz{Time: before, Valid: true})
}
