package storage

import (
	"context"
	"fmt"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WorkspaceRepository exposes trusted administrative workspace operations.
type WorkspaceRepository struct{ queries *sqlc.Queries }
type CreateWorkspaceInput struct{ ID, DisplayName string }
type UpdateWorkspaceInput struct{ ID, DisplayName string }

func NewWorkspaceRepository(pool *pgxpool.Pool) (*WorkspaceRepository, error) {
	if pool == nil {
		return nil, fmt.Errorf("workspace repository: pool is required")
	}
	return &WorkspaceRepository{queries: sqlc.New(pool)}, nil
}
func (r *WorkspaceRepository) Create(ctx context.Context, in CreateWorkspaceInput) (tenant.Workspace, error) {
	id, err := parseDBID(in.ID)
	if err != nil {
		return tenant.Workspace{}, fmt.Errorf("create workspace: %w", err)
	}
	row, err := r.queries.CreateWorkspace(ctx, sqlc.CreateWorkspaceParams{ID: id, DisplayName: in.DisplayName})
	return workspaceFromRow(row), repositoryError("create workspace", err, tenant.ErrWorkspaceNotFound)
}
func (r *WorkspaceRepository) Get(ctx context.Context, id string) (tenant.Workspace, error) {
	key, err := parseDBID(id)
	if err != nil {
		return tenant.Workspace{}, fmt.Errorf("get workspace: %w", err)
	}
	row, err := r.queries.GetWorkspaceByID(ctx, key)
	return workspaceFromRow(row), repositoryError("get workspace", err, tenant.ErrWorkspaceNotFound)
}
func (r *WorkspaceRepository) List(ctx context.Context, limit, offset int32) ([]tenant.Workspace, error) {
	if err := validatePage(limit, offset); err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	rows, err := r.queries.ListWorkspaces(ctx, sqlc.ListWorkspacesParams{Limit: limit, Offset: offset})
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	out := make([]tenant.Workspace, 0, len(rows))
	for _, row := range rows {
		out = append(out, workspaceFromRow(row))
	}
	return out, nil
}
func (r *WorkspaceRepository) Update(ctx context.Context, in UpdateWorkspaceInput) (tenant.Workspace, error) {
	id, err := parseDBID(in.ID)
	if err != nil {
		return tenant.Workspace{}, fmt.Errorf("update workspace: %w", err)
	}
	row, err := r.queries.UpdateWorkspace(ctx, sqlc.UpdateWorkspaceParams{ID: id, DisplayName: in.DisplayName})
	return workspaceFromRow(row), repositoryError("update workspace", err, tenant.ErrWorkspaceNotFound)
}
func (r *WorkspaceRepository) Delete(ctx context.Context, id string) error {
	key, err := parseDBID(id)
	if err != nil {
		return fmt.Errorf("delete workspace: %w", err)
	}
	_, err = r.queries.DeleteWorkspace(ctx, key)
	return repositoryError("delete workspace", err, tenant.ErrWorkspaceNotFound)
}
func workspaceFromRow(row sqlc.Workspace) tenant.Workspace {
	return tenant.Workspace{ID: row.ID.String(), DisplayName: row.DisplayName, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time}
}
