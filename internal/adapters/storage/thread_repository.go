package storage

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ThreadRepository struct{ queries *sqlc.Queries }
type CreateThreadInput struct {
	ID, WorkspaceID, IntegrationID, ExternalScope, ThreadType, ExternalID, Title string
	Metadata                                                                     map[string]any
}
type UpdateThreadInput struct {
	ID, WorkspaceID, Title string
	Metadata               map[string]any
}

func NewThreadRepository(pool *pgxpool.Pool) (*ThreadRepository, error) {
	if pool == nil {
		return nil, fmt.Errorf("thread repository: pool is required")
	}
	return &ThreadRepository{queries: sqlc.New(pool)}, nil
}
func (r *ThreadRepository) Create(ctx context.Context, in CreateThreadInput) (relay.Thread, error) {
	workspace, id, err := parseScopedIDs(in.WorkspaceID, in.ID)
	if err != nil {
		return relay.Thread{}, fmt.Errorf("create thread: %w", err)
	}
	integration, err := parseDBID(in.IntegrationID)
	if err != nil {
		return relay.Thread{}, fmt.Errorf("create thread integration: %w", err)
	}
	metadata, err := encodeObject(in.Metadata)
	if err != nil {
		return relay.Thread{}, fmt.Errorf("create thread: %w", err)
	}
	row, err := r.queries.CreateThread(ctx, sqlc.CreateThreadParams{ID: id, WorkspaceID: workspace, IntegrationID: integration, ExternalScope: in.ExternalScope, ThreadType: in.ThreadType, ExternalID: in.ExternalID, Title: in.Title, Metadata: metadata})
	if err != nil {
		return relay.Thread{}, repositoryError("create thread", err, relay.ErrThreadNotFound)
	}
	return threadFromRow(row)
}
func (r *ThreadRepository) Get(ctx context.Context, workspaceID, id string) (relay.Thread, error) {
	workspace, key, err := parseScopedIDs(workspaceID, id)
	if err != nil {
		return relay.Thread{}, fmt.Errorf("get thread: %w", err)
	}
	row, err := r.queries.GetThread(ctx, sqlc.GetThreadParams{WorkspaceID: workspace, ID: key})
	if err != nil {
		return relay.Thread{}, repositoryError("get thread", err, relay.ErrThreadNotFound)
	}
	return threadFromRow(row)
}
func (r *ThreadRepository) List(ctx context.Context, workspaceID string, limit, offset int32) ([]relay.Thread, error) {
	if err := validatePage(limit, offset); err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	workspace, err := parseDBID(workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	rows, err := r.queries.ListThreads(ctx, sqlc.ListThreadsParams{WorkspaceID: workspace, Limit: limit, Offset: offset})
	if err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	out := make([]relay.Thread, 0, len(rows))
	for _, row := range rows {
		item, err := threadFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}
func (r *ThreadRepository) Update(ctx context.Context, in UpdateThreadInput) (relay.Thread, error) {
	workspace, id, err := parseScopedIDs(in.WorkspaceID, in.ID)
	if err != nil {
		return relay.Thread{}, fmt.Errorf("update thread: %w", err)
	}
	metadata, err := encodeObject(in.Metadata)
	if err != nil {
		return relay.Thread{}, fmt.Errorf("update thread: %w", err)
	}
	row, err := r.queries.UpdateThread(ctx, sqlc.UpdateThreadParams{WorkspaceID: workspace, ID: id, Title: in.Title, Metadata: metadata})
	if err != nil {
		return relay.Thread{}, repositoryError("update thread", err, relay.ErrThreadNotFound)
	}
	return threadFromRow(row)
}
func (r *ThreadRepository) Delete(ctx context.Context, workspaceID, id string) error {
	workspace, key, err := parseScopedIDs(workspaceID, id)
	if err != nil {
		return fmt.Errorf("delete thread: %w", err)
	}
	_, err = r.queries.DeleteThread(ctx, sqlc.DeleteThreadParams{WorkspaceID: workspace, ID: key})
	return repositoryError("delete thread", err, relay.ErrThreadNotFound)
}
func threadFromRow(row sqlc.HubThread) (relay.Thread, error) {
	var metadata map[string]any
	if err := json.Unmarshal(row.Metadata, &metadata); err != nil {
		return relay.Thread{}, fmt.Errorf("decode thread metadata: %w", err)
	}
	var cursor *int64
	if row.SummaryThroughMessageID.Valid {
		v := row.SummaryThroughMessageID.Int64
		cursor = &v
	}
	return relay.Thread{ID: row.ID.String(), WorkspaceID: row.WorkspaceID.String(), IntegrationID: row.IntegrationID.String(), ExternalScope: row.ExternalScope, ThreadType: row.ThreadType, ExternalID: row.ExternalID, Title: row.Title, RollingSummary: row.RollingSummary, SummaryThroughMessageID: cursor, Metadata: metadata, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time}, nil
}
