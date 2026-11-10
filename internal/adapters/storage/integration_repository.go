package storage

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

type (
	IntegrationRepository  struct{ queries *sqlc.Queries }
	CreateIntegrationInput struct {
		ID, WorkspaceID, Platform string
		Role                      tenant.IntegrationRole
		Config                    map[string]any
		IsEnabled                 bool
	}
)

type UpdateIntegrationInput struct {
	ID, WorkspaceID string
	Role            tenant.IntegrationRole
	Config          map[string]any
	IsEnabled       bool
}

func NewIntegrationRepository(pool *pgxpool.Pool) (*IntegrationRepository, error) {
	if pool == nil {
		return nil, fmt.Errorf("integration repository: pool is required")
	}
	return &IntegrationRepository{queries: sqlc.New(pool)}, nil
}

func (r *IntegrationRepository) Create(ctx context.Context, in CreateIntegrationInput) (tenant.Integration, error) {
	workspace, id, err := parseScopedIDs(in.WorkspaceID, in.ID)
	if err != nil {
		return tenant.Integration{}, fmt.Errorf("create integration: %w", err)
	}
	config, err := encodeObject(in.Config)
	if err != nil {
		return tenant.Integration{}, fmt.Errorf("create integration: %w", err)
	}
	row, err := r.queries.CreateIntegration(ctx, sqlc.CreateIntegrationParams{ID: id, WorkspaceID: workspace, Platform: in.Platform, Role: string(in.Role), Config: config, IsEnabled: in.IsEnabled})
	if err != nil {
		return tenant.Integration{}, repositoryError("create integration", err, tenant.ErrIntegrationNotFound)
	}
	return integrationFromRow(sqlc.GetIntegrationRow(row))
}

func (r *IntegrationRepository) Get(ctx context.Context, workspaceID, id string) (tenant.Integration, error) {
	workspace, key, err := parseScopedIDs(workspaceID, id)
	if err != nil {
		return tenant.Integration{}, fmt.Errorf("get integration: %w", err)
	}
	row, err := r.queries.GetIntegration(ctx, sqlc.GetIntegrationParams{WorkspaceID: workspace, ID: key})
	if err != nil {
		return tenant.Integration{}, repositoryError("get integration", err, tenant.ErrIntegrationNotFound)
	}
	return integrationFromRow(row)
}

func (r *IntegrationRepository) List(ctx context.Context, workspaceID string, limit, offset int32) ([]tenant.Integration, error) {
	if err := validatePage(limit, offset); err != nil {
		return nil, fmt.Errorf("list integrations: %w", err)
	}
	workspace, err := parseDBID(workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list integrations: %w", err)
	}
	rows, err := r.queries.ListIntegrations(ctx, sqlc.ListIntegrationsParams{WorkspaceID: workspace, Limit: limit, Offset: offset})
	if err != nil {
		return nil, fmt.Errorf("list integrations: %w", err)
	}
	out := make([]tenant.Integration, 0, len(rows))
	for _, row := range rows {
		item, err := integrationFromRow(sqlc.GetIntegrationRow(row))
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

func (r *IntegrationRepository) Update(ctx context.Context, in UpdateIntegrationInput) (tenant.Integration, error) {
	workspace, id, err := parseScopedIDs(in.WorkspaceID, in.ID)
	if err != nil {
		return tenant.Integration{}, fmt.Errorf("update integration: %w", err)
	}
	config, err := encodeObject(in.Config)
	if err != nil {
		return tenant.Integration{}, fmt.Errorf("update integration: %w", err)
	}
	row, err := r.queries.UpdateIntegration(ctx, sqlc.UpdateIntegrationParams{WorkspaceID: workspace, ID: id, Role: string(in.Role), Config: config, IsEnabled: in.IsEnabled})
	if err != nil {
		return tenant.Integration{}, repositoryError("update integration", err, tenant.ErrIntegrationNotFound)
	}
	return integrationFromRow(sqlc.GetIntegrationRow(row))
}

func (r *IntegrationRepository) Delete(ctx context.Context, workspaceID, id string) error {
	workspace, key, err := parseScopedIDs(workspaceID, id)
	if err != nil {
		return fmt.Errorf("delete integration: %w", err)
	}
	_, err = r.queries.DeleteIntegration(ctx, sqlc.DeleteIntegrationParams{WorkspaceID: workspace, ID: key})
	return repositoryError("delete integration", err, tenant.ErrIntegrationNotFound)
}

// sqlc generates separate row types for this credential-free projection.
func integrationFromRow(row sqlc.GetIntegrationRow) (tenant.Integration, error) {
	var config map[string]any
	if err := json.Unmarshal(row.Config, &config); err != nil {
		return tenant.Integration{}, fmt.Errorf("decode integration config: %w", err)
	}
	return tenant.Integration{ID: row.ID.String(), WorkspaceID: row.WorkspaceID.String(), Platform: row.Platform, Role: tenant.IntegrationRole(row.Role), Config: config, IsEnabled: row.IsEnabled, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time}, nil
}

func (r *IntegrationRepository) LookupWebhookIntegration(
	ctx context.Context, id string,
) (tenant.WebhookIntegration, error) {
	dbID, err := parseDBID(id)
	if err != nil {
		return tenant.WebhookIntegration{}, fmt.Errorf("parse ID : %w", err)
	}
	row, err := r.queries.LookupWebhookIntegration(ctx, dbID)
	if err != nil {
		return tenant.WebhookIntegration{}, repositoryError(
			"lookup webhook integration",
			err,
			tenant.ErrIntegrationNotFound,
		)
	}
	var config map[string]any
	if err := json.Unmarshal(row.Config, &config); err != nil {
		return tenant.WebhookIntegration{}, fmt.Errorf("decode webhook config: %w", err)
	}
	return tenant.WebhookIntegration{
		ID:          row.ID.String(),
		WorkspaceID: row.WorkspaceID.String(),
		Platform:    row.Platform,
		Role:        tenant.IntegrationRole(row.Role),
		IsEnabled:   row.IsEnabled,
		Config:      config,
	}, nil
}
