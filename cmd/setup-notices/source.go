package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sourceCommand handles "source enable|disable --source-id <uuid>": it turns
// a ClickUp source integration on or off (integrations.is_enabled). It is
// the path for a source that is only polled (ClickUp Chat) and so is never
// enabled by bind-webhook. Webhook verification is not touched: a source
// without a webhook secret keeps refusing callbacks.
func sourceCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	if len(args) == 0 || (args[0] != "enable" && args[0] != "disable") {
		return errors.New("usage: source enable|disable --source-id <uuid>")
	}
	enable := args[0] == "enable"
	f := commandFlags("source " + args[0])
	source := f.String("source-id", "", "ClickUp source integration UUID")
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || !tenant.ValidUUID(*source) {
		return errors.New("source " + args[0] + " requires a valid --source-id")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return errors.New("database operation failed")
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(c)
	}()
	var data []byte
	if err := tx.QueryRow(ctx, `SELECT config FROM integrations WHERE id=$1 AND platform='clickup' AND role IN ('source','both') FOR UPDATE`, *source).Scan(&data); err != nil {
		return errors.New("ClickUp source not found")
	}
	if enable {
		var cfg map[string]any
		if json.Unmarshal(data, &cfg) != nil || cfg == nil {
			return errors.New("invalid source configuration")
		}
		team, _ := cfg["clickup_team_id"].(string)
		if strings.TrimSpace(team) == "" {
			return errors.New("source has no clickup_team_id; not enabled")
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE integrations SET is_enabled=$2,updated_at=now() WHERE id=$1`, *source, enable); err != nil {
		return errors.New("source update failed")
	}
	if tx.Commit(ctx) != nil {
		return errors.New("source update commit failed")
	}
	return json.NewEncoder(out).Encode(map[string]any{"source_id": *source, "is_enabled": enable})
}
