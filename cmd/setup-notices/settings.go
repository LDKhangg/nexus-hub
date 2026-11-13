package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	case len(args) >= 3 && len(args)%2 == 1 && args[0] == "set":
		// One or more key value pairs, stored together in one transaction.
		pairs := map[string]json.RawMessage{}
		var order []string
		for i := 1; i < len(args); i += 2 {
			key := args[i]
			if _, dup := pairs[key]; dup {
				return fmt.Errorf("settings: key %q given twice", key)
			}
			raw, err := settings.ParseCLIValue(key, args[i+1])
			if err != nil {
				return err
			}
			pairs[key] = raw
			order = append(order, key)
		}
		// Cross-key constraints are checked on the merged set before storing.
		current, err := store.All(ctx)
		if err != nil {
			return errors.New("settings read failed")
		}
		for k, v := range pairs {
			current[k] = v
		}
		if _, err := settings.Load(ctx, mapStore(current)); err != nil {
			return err
		}
		if err := store.SetMany(ctx, pairs); err != nil {
			return errors.New("settings write failed")
		}
		for _, k := range order {
			if err := enc.Encode(map[string]any{"key": k, "value": pairs[k], "source": "db", "note": "restart nexus-hub to apply"}); err != nil {
				return err
			}
		}
		return nil
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
	return errors.New("usage: settings get <key> | set <key> <value> [<key> <value> ...] | list")
}

type mapStore map[string]json.RawMessage

func (m mapStore) All(context.Context) (map[string]json.RawMessage, error) { return m, nil }
