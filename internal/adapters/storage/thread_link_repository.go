package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ThreadLinkRepository struct{ queries *sqlc.Queries }

type CreateThreadLinkInput struct {
	WorkspaceID, SourceThreadID, TargetThreadID string
	Metadata                                    map[string]any
}

type UpdateThreadLinkInput struct {
	WorkspaceID, SourceThreadID, TargetThreadID string
	Metadata                                    map[string]any
}

func NewThreadLinkRepository(pool *pgxpool.Pool) (*ThreadLinkRepository, error) {
	if pool == nil {
		return nil, fmt.Errorf("thread link repository: pool is required")
	}
	return &ThreadLinkRepository{queries: sqlc.New(pool)}, nil
}

// threadLinkKey compares parsed UUID bytes, so input case cannot affect identity.
func threadLinkKey(workspaceID, sourceID, targetID string) (sqlc.GetThreadLinkParams, error) {
	workspace, source, err := parseScopedIDs(workspaceID, sourceID)
	if err != nil {
		return sqlc.GetThreadLinkParams{}, err
	}
	target, err := parseDBID(targetID)
	if err != nil {
		return sqlc.GetThreadLinkParams{}, err
	}
	order := bytes.Compare(source.Bytes[:], target.Bytes[:])
	if order == 0 {
		return sqlc.GetThreadLinkParams{}, fmt.Errorf("thread link endpoints must differ")
	}
	if order > 0 {
		source, target = target, source
	}
	return sqlc.GetThreadLinkParams{WorkspaceID: workspace, SourceThreadID: source, TargetThreadID: target}, nil
}

func (r *ThreadLinkRepository) Create(ctx context.Context, in CreateThreadLinkInput) (relay.ThreadLink, error) {
	key, err := threadLinkKey(in.WorkspaceID, in.SourceThreadID, in.TargetThreadID)
	if err != nil {
		return relay.ThreadLink{}, fmt.Errorf("create thread link: %w", err)
	}
	metadata, err := encodeObject(in.Metadata)
	if err != nil {
		return relay.ThreadLink{}, fmt.Errorf("create thread link: %w", err)
	}
	row, err := r.queries.CreateThreadLink(ctx, sqlc.CreateThreadLinkParams{WorkspaceID: key.WorkspaceID, SourceThreadID: key.SourceThreadID, TargetThreadID: key.TargetThreadID, Metadata: metadata})
	if err != nil {
		return relay.ThreadLink{}, repositoryError("create thread link", err, relay.ErrThreadLinkNotFound)
	}
	return threadLinkFromRow(row)
}

func (r *ThreadLinkRepository) Get(ctx context.Context, workspaceID, sourceID, targetID string) (relay.ThreadLink, error) {
	key, err := threadLinkKey(workspaceID, sourceID, targetID)
	if err != nil {
		return relay.ThreadLink{}, fmt.Errorf("get thread link: %w", err)
	}
	row, err := r.queries.GetThreadLink(ctx, key)
	if err != nil {
		return relay.ThreadLink{}, repositoryError("get thread link", err, relay.ErrThreadLinkNotFound)
	}
	return threadLinkFromRow(row)
}

func (r *ThreadLinkRepository) List(ctx context.Context, workspaceID, threadID string, limit, offset int32) ([]relay.ThreadLink, error) {
	if err := validatePage(limit, offset); err != nil {
		return nil, fmt.Errorf("list thread links: %w", err)
	}
	workspace, thread, err := parseScopedIDs(workspaceID, threadID)
	if err != nil {
		return nil, fmt.Errorf("list thread links: %w", err)
	}
	rows, err := r.queries.ListThreadLinks(ctx, sqlc.ListThreadLinksParams{WorkspaceID: workspace, ThreadID: thread, Limit: limit, Offset: offset})
	if err != nil {
		return nil, fmt.Errorf("list thread links: %w", err)
	}
	out := make([]relay.ThreadLink, 0, len(rows))
	for _, row := range rows {
		item, err := threadLinkFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

func (r *ThreadLinkRepository) Update(ctx context.Context, in UpdateThreadLinkInput) (relay.ThreadLink, error) {
	key, err := threadLinkKey(in.WorkspaceID, in.SourceThreadID, in.TargetThreadID)
	if err != nil {
		return relay.ThreadLink{}, fmt.Errorf("update thread link: %w", err)
	}
	metadata, err := encodeObject(in.Metadata)
	if err != nil {
		return relay.ThreadLink{}, fmt.Errorf("update thread link: %w", err)
	}
	row, err := r.queries.UpdateThreadLink(ctx, sqlc.UpdateThreadLinkParams{WorkspaceID: key.WorkspaceID, SourceThreadID: key.SourceThreadID, TargetThreadID: key.TargetThreadID, Metadata: metadata})
	if err != nil {
		return relay.ThreadLink{}, repositoryError("update thread link", err, relay.ErrThreadLinkNotFound)
	}
	return threadLinkFromRow(row)
}

func (r *ThreadLinkRepository) Delete(ctx context.Context, workspaceID, sourceID, targetID string) error {
	key, err := threadLinkKey(workspaceID, sourceID, targetID)
	if err != nil {
		return fmt.Errorf("delete thread link: %w", err)
	}
	_, err = r.queries.DeleteThreadLink(ctx, sqlc.DeleteThreadLinkParams(key))
	return repositoryError("delete thread link", err, relay.ErrThreadLinkNotFound)
}

func threadLinkFromRow(row sqlc.ThreadLink) (relay.ThreadLink, error) {
	var metadata map[string]any
	if err := json.Unmarshal(row.Metadata, &metadata); err != nil {
		return relay.ThreadLink{}, fmt.Errorf("decode thread link metadata: %w", err)
	}
	return relay.ThreadLink{WorkspaceID: row.WorkspaceID.String(), SourceThreadID: row.SourceThreadID.String(), TargetThreadID: row.TargetThreadID.String(), Metadata: metadata, CreatedAt: row.CreatedAt.Time}, nil
}
