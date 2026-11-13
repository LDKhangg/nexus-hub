package storage

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/settings"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SettingsStore persists validated app_settings values.
type SettingsStore struct {
	pool    *pgxpool.Pool
	queries *sqlc.Queries
}

// SettingRow is one stored setting.
type SettingRow struct {
	Key       string
	Value     json.RawMessage
	UpdatedAt time.Time
}

func NewSettingsStore(pool *pgxpool.Pool) (*SettingsStore, error) {
	if pool == nil {
		return nil, errors.New("settings store requires pool")
	}
	return &SettingsStore{pool: pool, queries: sqlc.New(pool)}, nil
}

var _ settings.Store = (*SettingsStore)(nil)

func (s *SettingsStore) Get(ctx context.Context, key string) (json.RawMessage, bool, error) {
	v, err := s.queries.GetSetting(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return v, true, nil
}

// Set validates the value for key before storing it.
func (s *SettingsStore) Set(ctx context.Context, key string, value json.RawMessage) error {
	if err := settings.Validate(key, value); err != nil {
		return err
	}
	return s.queries.SetSetting(ctx, sqlc.SetSettingParams{Key: key, Value: value})
}

// SetMany validates every value, then stores them all in one
// transaction: either every pair is stored or none is.
func (s *SettingsStore) SetMany(ctx context.Context, values map[string]json.RawMessage) error {
	keys := make([]string, 0, len(values))
	for k, v := range values {
		if err := settings.Validate(k, v); err != nil {
			return err
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	for _, k := range keys {
		if err := q.SetSetting(ctx, sqlc.SetSettingParams{Key: k, Value: values[k]}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *SettingsStore) List(ctx context.Context) ([]SettingRow, error) {
	rows, err := s.queries.ListSettings(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SettingRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, SettingRow{Key: r.Key, Value: r.Value, UpdatedAt: r.UpdatedAt.Time})
	}
	return out, nil
}

// All implements settings.Store.
func (s *SettingsStore) All(ctx context.Context) (map[string]json.RawMessage, error) {
	rows, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(rows))
	for _, r := range rows {
		out[r.Key] = r.Value
	}
	return out, nil
}
