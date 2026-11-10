package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type NoticeRepository struct {
	pool    *pgxpool.Pool
	queries *sqlc.Queries
	newID   func() (string, error)
}

func NewNoticeRepository(pool *pgxpool.Pool, newID func() (string, error)) (*NoticeRepository, error) {
	if pool == nil || newID == nil {
		return nil, errors.New("notice repository requires pool and ID generator")
	}
	return &NoticeRepository{pool: pool, queries: sqlc.New(pool), newID: newID}, nil
}

func lookupNoticeIntegration(ctx context.Context, q *sqlc.Queries, id pgtype.UUID) (tenant.WebhookIntegration, error) {
	row, err := q.LookupWebhookIntegration(ctx, id)
	if err != nil {
		return tenant.WebhookIntegration{}, err
	}
	var config map[string]any
	if err := json.Unmarshal(row.Config, &config); err != nil {
		return tenant.WebhookIntegration{}, err
	}
	return tenant.WebhookIntegration{ID: row.ID.String(), WorkspaceID: row.WorkspaceID.String(), Platform: row.Platform, Role: tenant.IntegrationRole(row.Role), IsEnabled: row.IsEnabled, Config: config}, nil
}

func (r *NoticeRepository) Accept(ctx context.Context, events []domain.NormalizedEvent) (relay.AcceptResult, error) {
	result, err := r.accept(ctx, events)
	if err != nil {
		return relay.AcceptResult{}, fmt.Errorf("accept notices: %w", err)
	}
	return result, nil
}
func (r *NoticeRepository) accept(ctx context.Context, events []domain.NormalizedEvent) (relay.AcceptResult, error) {
	var result relay.AcceptResult
	if len(events) == 0 {
		return result, nil
	}
	workspace, source, err := parseScopedIDs(events[0].WorkspaceID, events[0].IntegrationID)
	if err != nil {
		return result, err
	}
	for _, e := range events {
		if e.WorkspaceID != events[0].WorkspaceID || e.IntegrationID != events[0].IntegrationID || e.Source != domain.SourceClickup || e.EventKey == "" || e.ExternalID == "" || e.ExternalMessageID == "" || e.OccurredAt.IsZero() {
			return result, errors.New("invalid notice batch")
		}
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	q := r.queries.WithTx(tx)
	integration, err := lookupNoticeIntegration(ctx, q, source)
	if err != nil {
		return result, err
	}
	if integration.WorkspaceID != workspace.String() {
		return result, errors.New("source workspace mismatch")
	}
	if err := tenant.ValidateClickUpSource(integration); err != nil {
		return result, err
	}
	config, err := tenant.ParseClickUpNoticeConfig(integration.Config)
	if err != nil {
		return result, err
	}
	targetID, err := parseDBID(config.TargetIntegrationID)
	if err != nil {
		return result, err
	}
	target, err := lookupNoticeIntegration(ctx, q, targetID)
	if err != nil {
		return result, err
	}
	if err := tenant.ValidateTelegramTarget(integration, target); err != nil {
		return result, err
	}
	if _, err := tenant.ParseTelegramTargetConfig(target.Config); err != nil {
		return result, err
	}
	ordered := append([]domain.NormalizedEvent(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].ExternalID == ordered[j].ExternalID {
			return ordered[i].EventKey < ordered[j].EventKey
		}
		return ordered[i].ExternalID < ordered[j].ExternalID
	})
	for _, e := range ordered {
		if e.Metadata["webhook_id"] != config.WebhookID {
			return result, errors.New("webhook identity mismatch")
		}
		id, err := r.newID()
		if err != nil {
			return result, err
		}
		dbID, err := parseDBID(id)
		if err != nil {
			return result, err
		}
		// The unique-key upsert also locks an existing thread before IDs are allocated.
		thread, err := q.FindOrCreateNoticeThread(ctx, sqlc.FindOrCreateNoticeThreadParams{ID: dbID, WorkspaceID: workspace, IntegrationID: source, ExternalID: e.ExternalID, Title: e.Title})
		if err != nil {
			return result, err
		}
		_, err = q.FindNoticeDuplicate(ctx, sqlc.FindNoticeDuplicateParams{WorkspaceID: workspace, SourceIntegrationID: source, EventKey: e.EventKey, TargetIntegrationID: targetID})
		if err == nil {
			result.Duplicates++
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return result, err
		}
		metadata, err := json.Marshal(e.Metadata)
		if err != nil {
			return result, err
		}
		if e.Metadata == nil {
			metadata = []byte("{}")
		}
		messageID, err := q.SaveNoticeMessage(ctx, sqlc.SaveNoticeMessageParams{ThreadID: thread.ID, ExternalMessageID: pgtype.Text{String: e.ExternalMessageID, Valid: true}, SenderID: pgtype.Text{String: e.SenderID, Valid: e.SenderID != ""}, SenderName: e.SenderName, Content: e.Content, OccurredAt: pgtype.Timestamptz{Time: e.OccurredAt, Valid: true}, Metadata: metadata})
		if err != nil {
			return result, err
		}
		encoded, err := json.Marshal(e)
		if err != nil {
			return result, err
		}
		if err = q.InsertNoticeJob(ctx, sqlc.InsertNoticeJobParams{WorkspaceID: workspace, SourceIntegrationID: source, TargetIntegrationID: targetID, EventKey: e.EventKey, Event: encoded, MessageID: messageID}); err != nil {
			return result, err
		}
		if err = q.RefreshMessageThread(ctx, thread.ID); err != nil {
			return result, err
		}
		result.Accepted++
	}
	return result, tx.Commit(ctx)
}
