package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TelegramTopicStore keeps topic bindings: identifiers and state only.
//
// Race-safe creation is two steps. EnsureTopic inserts a reservation row with
// no thread id (ON CONFLICT DO NOTHING); only the caller whose insert returns
// the row is "claimed" and may call Telegram createForumTopic. It then calls
// SetTopicThread, which updates only a row whose thread id is still NULL. A
// concurrent caller finds the existing row and is not claimed. A crashed
// creator leaves a reservation that another caller may reclaim after the
// lease (guarded by a conditional UPDATE, so one reclaimer wins).
type TelegramTopicStore struct {
	q *sqlc.Queries
}

func NewTelegramTopicStore(pool *pgxpool.Pool) (*TelegramTopicStore, error) {
	if pool == nil {
		return nil, errors.New("telegram topic store requires pool")
	}
	return &TelegramTopicStore{q: sqlc.New(pool)}, nil
}

var _ behavior.TopicStore = (*TelegramTopicStore)(nil)

func topicError(err error) error {
	if isNoRows(err) {
		return behavior.ErrNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23503":
			return behavior.ErrNotFound
		case "23505":
			return behavior.ErrConflict
		case "22001", "23502", "23514":
			return behavior.ErrInvalid
		}
	}
	return err
}

func topicInvalid(field string) error { return fmt.Errorf("%w: topic %s", behavior.ErrInvalid, field) }

func validSubject(kind, key string) bool {
	switch kind {
	case behavior.TopicSubjectClickUpTask, behavior.TopicSubjectWorkflow, behavior.TopicSubjectFixed:
	default:
		return false
	}
	return key != "" && len(key) <= 128
}

type topicIDs struct {
	ws, integration pgtype.UUID
}

func topicKeyIDs(key behavior.TopicKey) (topicIDs, error) {
	ws, err := parseDBID(key.WorkspaceID)
	if err != nil {
		return topicIDs{}, topicInvalid("workspace_id")
	}
	integration, err := parseDBID(key.IntegrationID)
	if err != nil {
		return topicIDs{}, topicInvalid("integration_id")
	}
	if key.ChatID == 0 {
		return topicIDs{}, topicInvalid("chat_id")
	}
	if !validSubject(key.SubjectKind, key.SubjectKey) {
		return topicIDs{}, topicInvalid("subject")
	}
	return topicIDs{ws: ws, integration: integration}, nil
}

func mapTopic(row sqlc.TelegramTopic) behavior.Topic {
	return behavior.Topic{
		ID: row.ID, WorkspaceID: uuidString(row.WorkspaceID), IntegrationID: uuidString(row.IntegrationID),
		ChatID: row.ChatID, ThreadID: row.ThreadID.Int64, SubjectKind: row.SubjectKind, SubjectKey: row.SubjectKey,
		State: row.State, ReservedAt: row.ReservedAt.Time, CreatedAt: row.CreatedAt.Time,
		UpdatedAt: row.UpdatedAt.Time, ClosedAt: tsPtr(row.ClosedAt),
	}
}

func (s *TelegramTopicStore) EnsureTopic(ctx context.Context, key behavior.TopicKey) (behavior.Topic, bool, error) {
	ids, err := topicKeyIDs(key)
	if err != nil {
		return behavior.Topic{}, false, err
	}
	row, err := s.q.ReserveTelegramTopic(ctx, sqlc.ReserveTelegramTopicParams{
		WorkspaceID: ids.ws, IntegrationID: ids.integration, ChatID: key.ChatID,
		SubjectKind: key.SubjectKind, SubjectKey: key.SubjectKey,
	})
	if err == nil {
		return mapTopic(row), true, nil
	}
	if !isNoRows(err) {
		return behavior.Topic{}, false, topicError(err)
	}
	row, err = s.q.GetTelegramTopicBySubject(ctx, sqlc.GetTelegramTopicBySubjectParams{
		WorkspaceID: ids.ws, IntegrationID: ids.integration, ChatID: key.ChatID,
		SubjectKind: key.SubjectKind, SubjectKey: key.SubjectKey,
	})
	if err != nil {
		return behavior.Topic{}, false, topicError(err) // deleted in between: caller may retry
	}
	if row.ThreadID.Valid {
		return mapTopic(row), false, nil
	}
	reclaimed, err := s.q.ReclaimTelegramTopicReservation(ctx, sqlc.ReclaimTelegramTopicReservationParams{
		WorkspaceID: ids.ws, IntegrationID: ids.integration, ChatID: key.ChatID,
		SubjectKind: key.SubjectKind, SubjectKey: key.SubjectKey,
		LeaseSecs: behavior.TopicReservationLease.Seconds(),
	})
	if err == nil {
		return mapTopic(reclaimed), true, nil
	}
	if !isNoRows(err) {
		return behavior.Topic{}, false, topicError(err)
	}
	return mapTopic(row), false, nil
}

func (s *TelegramTopicStore) SetTopicThread(ctx context.Context, workspaceID string, id, threadID int64, reservedAt time.Time) (behavior.Topic, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.Topic{}, topicInvalid("workspace_id")
	}
	if id <= 0 || threadID <= 0 {
		return behavior.Topic{}, topicInvalid("thread_id")
	}
	row, err := s.q.SetTelegramTopicThread(ctx, sqlc.SetTelegramTopicThreadParams{
		WorkspaceID: ws, ID: id, ThreadID: pgtype.Int8{Int64: threadID, Valid: true},
		ReservedAt: pgtype.Timestamptz{Time: reservedAt, Valid: true},
	})
	if isNoRows(err) {
		// Missing/foreign, a thread id is already set, or the reservation was
		// taken over by another holder.
		return behavior.Topic{}, behavior.ErrConflict
	}
	if err != nil {
		return behavior.Topic{}, topicError(err)
	}
	return mapTopic(row), nil
}

func (s *TelegramTopicStore) ReleaseTopic(ctx context.Context, workspaceID string, id int64) error {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return topicInvalid("workspace_id")
	}
	if id <= 0 {
		return topicInvalid("id")
	}
	_, err = s.q.ReleaseTelegramTopicReservation(ctx, sqlc.ReleaseTelegramTopicReservationParams{WorkspaceID: ws, ID: id})
	return topicError(err)
}

func (s *TelegramTopicStore) TopicBySubject(ctx context.Context, key behavior.TopicKey) (behavior.Topic, error) {
	ids, err := topicKeyIDs(key)
	if err != nil {
		return behavior.Topic{}, err
	}
	row, err := s.q.GetTelegramTopicBySubject(ctx, sqlc.GetTelegramTopicBySubjectParams{
		WorkspaceID: ids.ws, IntegrationID: ids.integration, ChatID: key.ChatID,
		SubjectKind: key.SubjectKind, SubjectKey: key.SubjectKey,
	})
	if err != nil {
		return behavior.Topic{}, topicError(err)
	}
	return mapTopic(row), nil
}

func (s *TelegramTopicStore) TopicByThread(ctx context.Context, workspaceID, integrationID string, chatID, threadID int64) (behavior.Topic, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.Topic{}, topicInvalid("workspace_id")
	}
	integration, err := parseDBID(integrationID)
	if err != nil {
		return behavior.Topic{}, topicInvalid("integration_id")
	}
	if chatID == 0 || threadID <= 0 {
		return behavior.Topic{}, behavior.ErrNotFound
	}
	row, err := s.q.GetTelegramTopicByThread(ctx, sqlc.GetTelegramTopicByThreadParams{
		WorkspaceID: ws, IntegrationID: integration, ChatID: chatID,
		ThreadID: pgtype.Int8{Int64: threadID, Valid: true},
	})
	if err != nil {
		return behavior.Topic{}, topicError(err)
	}
	return mapTopic(row), nil
}

func (s *TelegramTopicStore) SetTopicState(ctx context.Context, workspaceID string, id int64, state string) (behavior.Topic, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.Topic{}, topicInvalid("workspace_id")
	}
	if id <= 0 || (state != behavior.TopicOpen && state != behavior.TopicClosed) {
		return behavior.Topic{}, topicInvalid("state")
	}
	row, err := s.q.SetTelegramTopicState(ctx, sqlc.SetTelegramTopicStateParams{WorkspaceID: ws, ID: id, State: state})
	if err != nil {
		return behavior.Topic{}, topicError(err)
	}
	return mapTopic(row), nil
}

func (s *TelegramTopicStore) ListTopics(ctx context.Context, workspaceID string) ([]behavior.Topic, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, topicInvalid("workspace_id")
	}
	rows, err := s.q.ListTelegramTopics(ctx, ws)
	if err != nil {
		return nil, topicError(err)
	}
	out := make([]behavior.Topic, 0, len(rows))
	for _, row := range rows {
		out = append(out, mapTopic(row))
	}
	return out, nil
}

// DeleteTopic forgets a topic binding whose Telegram thread no longer
// exists, so the next send creates a new topic.
func (s *TelegramTopicStore) DeleteTopic(ctx context.Context, workspaceID string, id int64) error {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return topicInvalid("workspace_id")
	}
	if id <= 0 {
		return topicInvalid("id")
	}
	_, err = s.q.DeleteTelegramTopic(ctx, sqlc.DeleteTelegramTopicParams{WorkspaceID: ws, ID: id})
	return topicError(err)
}

var _ behavior.TopicLinkStore = (*TelegramTopicStore)(nil)

func topicLinkIDs(workspaceID string, id int64) (pgtype.UUID, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return pgtype.UUID{}, topicInvalid("workspace_id")
	}
	if id <= 0 {
		return pgtype.UUID{}, topicInvalid("id")
	}
	return ws, nil
}

// ClaimTopicLink claims the task link message of a created topic; only one
// caller ever gets true for a topic.
func (s *TelegramTopicStore) ClaimTopicLink(ctx context.Context, workspaceID string, id int64) (bool, error) {
	ws, err := topicLinkIDs(workspaceID, id)
	if err != nil {
		return false, err
	}
	n, err := s.q.ClaimTelegramTopicLink(ctx, sqlc.ClaimTelegramTopicLinkParams{WorkspaceID: ws, ID: id})
	if err != nil {
		return false, topicError(err)
	}
	return n == 1, nil
}

// SetTopicLink records the posted link message id of a claimed topic.
func (s *TelegramTopicStore) SetTopicLink(ctx context.Context, workspaceID string, id, messageID int64) error {
	ws, err := topicLinkIDs(workspaceID, id)
	if err != nil {
		return err
	}
	if messageID <= 0 {
		return topicInvalid("message_id")
	}
	n, err := s.q.SetTelegramTopicLink(ctx, sqlc.SetTelegramTopicLinkParams{WorkspaceID: ws, ID: id, MessageID: messageID})
	if err != nil {
		return topicError(err)
	}
	if n == 0 {
		return behavior.ErrConflict
	}
	return nil
}

// ReleaseTopicLink gives a claim back after a send that was surely not
// delivered, so a later run may post the link.
func (s *TelegramTopicStore) ReleaseTopicLink(ctx context.Context, workspaceID string, id int64) error {
	ws, err := topicLinkIDs(workspaceID, id)
	if err != nil {
		return err
	}
	_, err = s.q.ReleaseTelegramTopicLink(ctx, sqlc.ReleaseTelegramTopicLinkParams{WorkspaceID: ws, ID: id})
	return topicError(err)
}
