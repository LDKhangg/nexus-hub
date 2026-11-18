package storage

import (
	"context"
	"errors"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ClickUpDraftStore persists ClickUp drafts (Phase 10 REQ-7). Every
// terminal transition clears the body in the same statement; the body is
// never logged.
type ClickUpDraftStore struct{ q *sqlc.Queries }

func NewClickUpDraftStore(pool *pgxpool.Pool) (*ClickUpDraftStore, error) {
	if pool == nil {
		return nil, errors.New("clickup draft store requires pool")
	}
	return &ClickUpDraftStore{q: sqlc.New(pool)}, nil
}

var _ behavior.ClickUpDraftStore = (*ClickUpDraftStore)(nil)

func mapClickUpDraft(r sqlc.ClickupDraft) behavior.ClickUpDraft {
	d := behavior.ClickUpDraft{
		ID: r.ID, WorkspaceID: uuidString(r.WorkspaceID), IntegrationID: uuidString(r.IntegrationID), ChatID: r.ChatID,
		ThreadID: r.ThreadID, TaskID: r.TaskID, ParentCommentID: r.ParentCommentID, MentionUserIDs: append([]string(nil), r.MentionUserIds...),
		NewStatus: r.NewStatus, Body: r.Body.String, BodySHA256: r.BodySha256, State: r.State, Source: r.Source,
		PlanTaskKey: r.PlanTaskKey, PreviewMessageID: r.PreviewMessageID, PostedCommentID: r.PostedCommentID, ReasonCode: r.ReasonCode,
		ExpiresAt: r.ExpiresAt.Time, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time, FinishedAt: tsPtr(r.FinishedAt),
	}
	if r.ExecutionID.Valid {
		d.ExecutionID = r.ExecutionID.Int64
	}
	return d
}

// draftRow maps a conditional update: no row is a state conflict.
func draftRow(r sqlc.ClickupDraft, err error) (behavior.ClickUpDraft, error) {
	if err != nil {
		if isNoRows(err) {
			return behavior.ClickUpDraft{}, behavior.ErrConflict
		}
		return behavior.ClickUpDraft{}, err
	}
	return mapClickUpDraft(r), nil
}

func draftRows(rows []sqlc.ClickupDraft, err error) ([]behavior.ClickUpDraft, error) {
	if err != nil {
		return nil, err
	}
	out := make([]behavior.ClickUpDraft, 0, len(rows))
	for _, r := range rows {
		out = append(out, mapClickUpDraft(r))
	}
	return out, nil
}

func (s *ClickUpDraftStore) CreateDraft(ctx context.Context, d behavior.NewClickUpDraft) (behavior.ClickUpDraft, error) {
	ws, err := parseDBID(d.Chat.WorkspaceID)
	if err != nil {
		return behavior.ClickUpDraft{}, invalid("workspace_id")
	}
	integration, err := parseDBID(d.Chat.IntegrationID)
	if err != nil {
		return behavior.ClickUpDraft{}, invalid("integration_id")
	}
	if d.Body == "" || d.ExpiresAt.IsZero() || d.Chat.ChatID == 0 {
		return behavior.ClickUpDraft{}, invalid("draft")
	}
	mentions := d.MentionUserIDs
	if mentions == nil {
		mentions = []string{}
	}
	row, err := s.q.InsertClickUpDraft(ctx, sqlc.InsertClickUpDraftParams{
		WorkspaceID: ws, IntegrationID: integration, ChatID: d.Chat.ChatID, ThreadID: d.Chat.ThreadID, TaskID: d.TaskID,
		ParentCommentID: d.ParentCommentID, MentionUserIds: mentions, NewStatus: d.NewStatus, Body: d.Body, BodySha256: d.BodySHA256,
		Source: d.Source, ExecutionID: optInt8(d.ExecutionID), PlanTaskKey: d.PlanTaskKey, ExpiresAt: ts(d.ExpiresAt),
	})
	if err != nil {
		return behavior.ClickUpDraft{}, err
	}
	return mapClickUpDraft(row), nil
}

func (s *ClickUpDraftStore) GetDraft(ctx context.Context, workspaceID string, id int64) (behavior.ClickUpDraft, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.ClickUpDraft{}, invalid("workspace_id")
	}
	row, err := s.q.GetClickUpDraft(ctx, sqlc.GetClickUpDraftParams{WorkspaceID: ws, ID: id})
	if err != nil {
		return behavior.ClickUpDraft{}, notFound(err)
	}
	return mapClickUpDraft(row), nil
}

func (s *ClickUpDraftStore) SetDraftPreview(ctx context.Context, workspaceID string, id, messageID int64) error {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return invalid("workspace_id")
	}
	if messageID <= 0 {
		return invalid("message_id")
	}
	n, err := s.q.SetClickUpDraftPreview(ctx, sqlc.SetClickUpDraftPreviewParams{MessageID: messageID, WorkspaceID: ws, ID: id})
	if err != nil {
		return err
	}
	if n == 0 {
		return behavior.ErrConflict
	}
	return nil
}

func (s *ClickUpDraftStore) ReviseDraft(ctx context.Context, workspaceID string, id int64, oldSHA, body, sha string, now time.Time) (behavior.ClickUpDraft, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.ClickUpDraft{}, invalid("workspace_id")
	}
	if body == "" {
		return behavior.ClickUpDraft{}, invalid("body")
	}
	return draftRow(s.q.ReviseClickUpDraft(ctx, sqlc.ReviseClickUpDraftParams{
		Body: body, BodySha256: sha, WorkspaceID: ws, ID: id, OldSha256: oldSHA, AtTime: ts(now),
	}))
}

func (s *ClickUpDraftStore) ClaimDraftPost(ctx context.Context, workspaceID string, id int64, sha string, previewMessageID int64, now time.Time) (behavior.ClickUpDraft, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.ClickUpDraft{}, invalid("workspace_id")
	}
	if previewMessageID <= 0 {
		return behavior.ClickUpDraft{}, behavior.ErrConflict
	}
	return draftRow(s.q.ClaimClickUpDraftPost(ctx, sqlc.ClaimClickUpDraftPostParams{
		WorkspaceID: ws, ID: id, BodySha256: sha, PreviewMessageID: previewMessageID, AtTime: ts(now),
	}))
}

func (s *ClickUpDraftStore) FinishDraftPosted(ctx context.Context, workspaceID string, id int64, commentID, reasonCode string) (behavior.ClickUpDraft, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.ClickUpDraft{}, invalid("workspace_id")
	}
	return draftRow(s.q.FinishClickUpDraftPosted(ctx, sqlc.FinishClickUpDraftPostedParams{
		PostedCommentID: commentID, ReasonCode: reasonCode, WorkspaceID: ws, ID: id,
	}))
}

func (s *ClickUpDraftStore) FailDraft(ctx context.Context, workspaceID string, id int64, reasonCode string) (behavior.ClickUpDraft, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.ClickUpDraft{}, invalid("workspace_id")
	}
	return draftRow(s.q.FailClickUpDraft(ctx, sqlc.FailClickUpDraftParams{ReasonCode: reasonCode, WorkspaceID: ws, ID: id}))
}

func (s *ClickUpDraftStore) CancelDraft(ctx context.Context, workspaceID string, id int64) (behavior.ClickUpDraft, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.ClickUpDraft{}, invalid("workspace_id")
	}
	return draftRow(s.q.CancelClickUpDraft(ctx, sqlc.CancelClickUpDraftParams{WorkspaceID: ws, ID: id}))
}

func (s *ClickUpDraftStore) ExpireDrafts(ctx context.Context, now time.Time, limit int) ([]behavior.ClickUpDraft, error) {
	if limit <= 0 {
		return nil, nil
	}
	return draftRows(s.q.ExpireClickUpDrafts(ctx, sqlc.ExpireClickUpDraftsParams{AtTime: ts(now), MaxRows: int32(limit)}))
}

func (s *ClickUpDraftStore) ReclaimStalePosts(ctx context.Context, before time.Time, limit int) ([]behavior.ClickUpDraft, error) {
	if limit <= 0 {
		return nil, nil
	}
	return draftRows(s.q.ReclaimStaleClickUpDraftPosts(ctx, sqlc.ReclaimStaleClickUpDraftPostsParams{Before: ts(before), MaxRows: int32(limit)}))
}
