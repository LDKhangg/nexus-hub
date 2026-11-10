package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/memory"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MessageRepository preserves original messages and invalidates stale summaries.
type MessageRepository struct {
	pool    *pgxpool.Pool
	queries *sqlc.Queries
}

func NewMessageRepository(pool *pgxpool.Pool) (*MessageRepository, error) {
	if pool == nil {
		return nil, fmt.Errorf("message repository requires a pool")
	}
	return &MessageRepository{pool: pool, queries: sqlc.New(pool)}, nil
}

type CreateMessageInput struct {
	WorkspaceID, ThreadID       string
	ExternalMessageID, SenderID *string
	SenderName                  string
	SenderRole                  memory.SenderRole
	Content                     string
	OccurredAt                  time.Time
	Metadata                    map[string]any
}

type UpdateMessageInput struct {
	WorkspaceID, ThreadID string
	ID                    int64
	Content               string
	Metadata              map[string]any
}

func (r *MessageRepository) Create(ctx context.Context, input CreateMessageInput) (memory.Message, error) {
	workspace, thread, err := parseScopedIDs(input.WorkspaceID, input.ThreadID)
	if err != nil {
		return memory.Message{}, fmt.Errorf("create message: %w", err)
	}
	metadata, err := encodeObject(input.Metadata)
	if err != nil {
		return memory.Message{}, fmt.Errorf("create message: %w", err)
	}
	var result memory.Message
	err = r.withThreadWrite(ctx, workspace, thread, func(q *sqlc.Queries) error {
		row, err := q.CreateMessage(ctx, sqlc.CreateMessageParams{ThreadID: thread, ExternalMessageID: nullableText(input.ExternalMessageID), SenderID: nullableText(input.SenderID), SenderName: input.SenderName, SenderRole: string(input.SenderRole), Content: input.Content, OccurredAt: pgtype.Timestamptz{Time: input.OccurredAt, Valid: !input.OccurredAt.IsZero()}, Metadata: metadata})
		if err != nil {
			return err
		}
		result, err = messageFromRow(row)
		return err
	})
	if err != nil {
		return memory.Message{}, fmt.Errorf("create message: %w", err)
	}
	return result, nil
}

func (r *MessageRepository) Get(ctx context.Context, workspaceID, threadID string, id int64) (memory.Message, error) {
	workspace, thread, err := parseScopedIDs(workspaceID, threadID)
	if err != nil {
		return memory.Message{}, fmt.Errorf("get message: %w", err)
	}
	if id <= 0 {
		return memory.Message{}, fmt.Errorf("get message: ID must be positive")
	}
	row, err := r.queries.GetMessage(ctx, sqlc.GetMessageParams{WorkspaceID: workspace, ThreadID: thread, ID: id})
	if err != nil {
		return memory.Message{}, repositoryError("get message", err, memory.ErrMessageNotFound)
	}
	return messageFromRow(row)
}

func (r *MessageRepository) List(ctx context.Context, workspaceID, threadID string, limit, offset int32) ([]memory.Message, error) {
	workspace, thread, err := parseScopedIDs(workspaceID, threadID)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	if err = validatePage(limit, offset); err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	rows, err := r.queries.ListMessages(ctx, sqlc.ListMessagesParams{WorkspaceID: workspace, ThreadID: thread, Limit: limit, Offset: offset})
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	result := make([]memory.Message, 0, len(rows))
	for _, row := range rows {
		item, err := messageFromRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func (r *MessageRepository) Update(ctx context.Context, input UpdateMessageInput) (memory.Message, error) {
	workspace, thread, err := parseScopedIDs(input.WorkspaceID, input.ThreadID)
	if err != nil {
		return memory.Message{}, fmt.Errorf("update message: %w", err)
	}
	if input.ID <= 0 {
		return memory.Message{}, fmt.Errorf("update message: ID must be positive")
	}
	metadata, err := encodeObject(input.Metadata)
	if err != nil {
		return memory.Message{}, fmt.Errorf("update message: %w", err)
	}
	var result memory.Message
	err = r.withThreadWrite(ctx, workspace, thread, func(q *sqlc.Queries) error {
		if _, err := q.GetMessage(ctx, sqlc.GetMessageParams{WorkspaceID: workspace, ThreadID: thread, ID: input.ID}); err != nil {
			return repositoryError("find message", err, memory.ErrMessageNotFound)
		}
		if err := q.InvalidateThreadSummary(ctx, sqlc.InvalidateThreadSummaryParams{ID: thread, SummaryThroughMessageID: pgtype.Int8{Int64: input.ID, Valid: true}}); err != nil {
			return err
		}
		row, err := q.UpdateMessage(ctx, sqlc.UpdateMessageParams{WorkspaceID: workspace, ThreadID: thread, ID: input.ID, Content: input.Content, Metadata: metadata})
		if err != nil {
			return repositoryError("write message", err, memory.ErrMessageNotFound)
		}
		result, err = messageFromRow(row)
		return err
	})
	if err != nil {
		return memory.Message{}, fmt.Errorf("update message: %w", err)
	}
	return result, nil
}

func (r *MessageRepository) Delete(ctx context.Context, workspaceID, threadID string, id int64) error {
	workspace, thread, err := parseScopedIDs(workspaceID, threadID)
	if err != nil {
		return fmt.Errorf("delete message: %w", err)
	}
	if id <= 0 {
		return fmt.Errorf("delete message: ID must be positive")
	}
	err = r.withThreadWrite(ctx, workspace, thread, func(q *sqlc.Queries) error {
		if _, err := q.GetMessage(ctx, sqlc.GetMessageParams{WorkspaceID: workspace, ThreadID: thread, ID: id}); err != nil {
			return repositoryError("find message", err, memory.ErrMessageNotFound)
		}
		// Clear the cursor before deleting a message referenced by its foreign key.
		if err := q.InvalidateThreadSummary(ctx, sqlc.InvalidateThreadSummaryParams{ID: thread, SummaryThroughMessageID: pgtype.Int8{Int64: id, Valid: true}}); err != nil {
			return err
		}
		_, err := q.DeleteMessage(ctx, sqlc.DeleteMessageParams{WorkspaceID: workspace, ThreadID: thread, ID: id})
		return repositoryError("remove message", err, memory.ErrMessageNotFound)
	})
	if err != nil {
		return fmt.Errorf("delete message: %w", err)
	}
	return nil
}

// All repository message writers acquire this lock before allocating IDs or
// changing history. Future summary writers must take the same thread lock.
func (r *MessageRepository) withThreadWrite(ctx context.Context, workspace, thread pgtype.UUID, write func(*sqlc.Queries) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin message transaction: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanupCtx)
	}()
	q := r.queries.WithTx(tx)
	if _, err = q.LockMessageThread(ctx, sqlc.LockMessageThreadParams{WorkspaceID: workspace, ID: thread}); err != nil {
		return repositoryError("lock message thread", err, relay.ErrThreadNotFound)
	}
	if err = write(q); err != nil {
		return err
	}
	if err = q.RefreshMessageThread(ctx, thread); err != nil {
		return fmt.Errorf("refresh message thread: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit message transaction: %w", err)
	}
	return nil
}

func messageFromRow(row sqlc.MessageLog) (memory.Message, error) {
	var metadata map[string]any
	if err := json.Unmarshal(row.Metadata, &metadata); err != nil {
		return memory.Message{}, fmt.Errorf("decode message metadata: %w", err)
	}
	return memory.Message{ID: row.ID, ThreadID: row.ThreadID.String(), ExternalMessageID: textPointer(row.ExternalMessageID), SenderID: textPointer(row.SenderID), SenderName: row.SenderName, SenderRole: memory.SenderRole(row.SenderRole), Content: row.Content, OccurredAt: row.OccurredAt.Time, CreatedAt: row.CreatedAt.Time, Metadata: metadata}, nil
}
