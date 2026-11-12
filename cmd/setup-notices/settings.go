package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/settings"
	"github.com/jackc/pgx/v5/pgxpool"
)

func settingsCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	store, err := storage.NewSettingsStore(pool)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	switch {
	case len(args) == 2 && args[0] == "get":
		key := args[1]
		def, err := settings.DefaultJSON(key)
		if err != nil {
			return err
		}
		v, ok, err := store.Get(ctx, key)
		if err != nil {
			return errors.New("settings read failed")
		}
		if !ok {
			return enc.Encode(map[string]any{"key": key, "value": def, "source": "default"})
		}
		return enc.Encode(map[string]any{"key": key, "value": v, "source": "db"})
	case len(args) == 3 && args[0] == "set":
		raw, err := settings.ParseCLIValue(args[1], args[2])
		if err != nil {
			return err
		}
		// Cross-key constraints are checked on the merged set before storing.
		current, err := store.All(ctx)
		if err != nil {
			return errors.New("settings read failed")
		}
		current[args[1]] = raw
		if _, err := settings.Load(ctx, mapStore(current)); err != nil {
			return err
		}
		if err := store.Set(ctx, args[1], raw); err != nil {
			return errors.New("settings write failed")
		}
		return enc.Encode(map[string]any{"key": args[1], "value": raw, "source": "db", "note": "restart nexus-hub to apply"})
	case len(args) == 1 && args[0] == "list":
		stored, err := store.All(ctx)
		if err != nil {
			return errors.New("settings read failed")
		}
		for _, key := range settings.Keys() {
			row := map[string]any{"key": key}
			if v, ok := stored[key]; ok {
				row["value"], row["source"] = v, "db"
			} else {
				def, _ := settings.DefaultJSON(key)
				row["value"], row["source"] = def, "default"
			}
			if err := enc.Encode(row); err != nil {
				return err
			}
		}
		return nil
	}
	return errors.New("usage: settings get <key> | set <key> <value> | list")
}

type mapStore map[string]json.RawMessage

func (m mapStore) All(context.Context) (map[string]json.RawMessage, error) { return m, nil }
