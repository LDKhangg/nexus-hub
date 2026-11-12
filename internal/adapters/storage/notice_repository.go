package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DebounceSettings controls agent-job coalescing: a job runs Window after
// the latest comment, but never later than MaxWait after the first one.
type DebounceSettings struct{ Window, MaxWait time.Duration }

const (
	defaultDebounceWindow  = 8 * time.Second
	defaultDebounceMaxWait = 30 * time.Second
)

type NoticeRepository struct {
	pool     *pgxpool.Pool
	queries  *sqlc.Queries
	newID    func() (string, error)
	debounce DebounceSettings

	mu    sync.RWMutex
	known map[string]tenant.Tier
}

func NewNoticeRepository(pool *pgxpool.Pool, newID func() (string, error), debounce DebounceSettings) (*NoticeRepository, error) {
	if pool == nil || newID == nil {
		return nil, errors.New("notice repository requires pool and ID generator")
	}
	if debounce.Window <= 0 {
		debounce.Window = defaultDebounceWindow
	}
	if debounce.MaxWait <= 0 {
		debounce.MaxWait = defaultDebounceMaxWait
	}
	return &NoticeRepository{pool: pool, queries: sqlc.New(pool), newID: newID, debounce: debounce}, nil
}

// SetKnownTools registers the tool tiers used to validate agent configs.
func (r *NoticeRepository) SetKnownTools(known map[string]tenant.Tier) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.known = known
}

func (r *NoticeRepository) knownTools() map[string]tenant.Tier {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.known
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
	agentCfg, err := tenant.ParseAgentConfig(integration.Config, r.knownTools())
	if err != nil {
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
		if agentCfg.Enabled && e.EventType == domain.EventCommandPosted {
			accepted, err := r.acceptAgentComment(ctx, q, e, workspace, source, thread.ID, config.OwnSenderID)
			if err != nil {
				return result, err
			}
			if accepted {
				result.Accepted++
			} else {
				result.Duplicates++
			}
			continue
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

// acceptAgentComment stores a comment for the agent: an owner comment is
// context only; any other comment upserts the thread's pending agent job.
// It reports false for a duplicate callback.
func (r *NoticeRepository) acceptAgentComment(ctx context.Context, q *sqlc.Queries, e domain.NormalizedEvent, workspace, source, thread pgtype.UUID, ownSenderID string) (bool, error) {
	role := "external_user"
	if ownSenderID != "" && e.SenderID == ownSenderID {
		role = "human_owner"
	}
	metadata := []byte("{}")
	if e.Metadata != nil {
		var err error
		if metadata, err = json.Marshal(e.Metadata); err != nil {
			return false, err
		}
	}
	_, err := q.SaveAgentMessage(ctx, sqlc.SaveAgentMessageParams{ThreadID: thread, ExternalMessageID: pgtype.Text{String: e.ExternalMessageID, Valid: true}, SenderID: pgtype.Text{String: e.SenderID, Valid: e.SenderID != ""}, SenderName: e.SenderName, SenderRole: role, Content: e.Content, OccurredAt: pgtype.Timestamptz{Time: e.OccurredAt, Valid: true}, Metadata: metadata})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if role == "external_user" {
		if _, err := q.UpsertPendingAgentJob(ctx, sqlc.UpsertPendingAgentJobParams{WorkspaceID: workspace, SourceIntegrationID: source, ThreadID: thread, WindowSecs: r.debounce.Window.Seconds(), MaxWaitSecs: r.debounce.MaxWait.Seconds()}); err != nil {
			return false, err
		}
	}
	return true, q.RefreshMessageThread(ctx, thread)
}
