package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func setupID(id string) (string, error) {
	if id != "" {
		parsed, err := uuid.Parse(id)
		if err != nil {
			return "", errors.New("invalid supplied UUID")
		}
		return parsed.String(), nil
	}
	generated, err := uuid.NewV7()
	if err != nil {
		return "", errors.New("UUID generation failed")
	}
	return generated.String(), nil
}

func initNotices(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	f := commandFlags("init")
	name := f.String("workspace-name", "", "workspace name")
	chat := f.String("chat-id", "", "Telegram chat ID")
	team := f.String("team-id", "", "external ClickUp team ID")
	w := f.String("workspace-id", "", "existing workspace UUID")
	s := f.String("source-id", "", "existing source UUID")
	target := f.String("target-id", "", "existing target UUID")
	topic := f.Int64("message-thread-id", 0, "existing topic ID")
	tokenEnv := f.String("bot-token-env", "NEXUS_TELEGRAM_BOT_TOKEN", "token environment reference")
	secretEnv := f.String("webhook-secret-env", "NEXUS_CLICKUP_WEBHOOK_SECRET", "signature secret environment reference")
	own := f.String("own-sender-id", "", "optional own ClickUp bot sender ID")
	if f.Parse(args) != nil || f.NArg() != 0 || strings.TrimSpace(*name) == "" || strings.TrimSpace(*team) == "" || *topic < 0 {
		return errors.New("init requires --workspace-name, --chat-id and --team-id; optional topic must be positive")
	}
	workspace, err := setupID(*w)
	if err != nil {
		return err
	}
	source, err := setupID(*s)
	if err != nil {
		return err
	}
	destination, err := setupID(*target)
	if err != nil {
		return err
	}
	if source == destination {
		return errors.New("source and target IDs must differ")
	}
	targetConfig := map[string]any{"chat_id": *chat, "bot_token_env": *tokenEnv}
	if *topic != 0 {
		targetConfig["message_thread_id"] = *topic
	}
	if _, err := tenant.ParseTelegramTargetConfig(targetConfig); err != nil {
		return err
	}
	sourceConfig := map[string]any{"clickup_team_id": *team, "webhook_secret_env": *secretEnv, "target_integration_id": destination, "own_sender_id": *own, "webhook_id": "unbound"}
	if _, err := tenant.ParseClickUpNoticeConfig(sourceConfig); err != nil {
		return err
	}
	delete(sourceConfig, "webhook_id")
	srcJSON, _ := json.Marshal(sourceConfig)
	targetJSON, _ := json.Marshal(targetConfig)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return errors.New("setup transaction failed")
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(c)
	}()
	if _, err := tx.Exec(ctx, `INSERT INTO workspaces(id,display_name) VALUES($1,$2) ON CONFLICT(id) DO NOTHING`, workspace, *name); err != nil {
		return errors.New("workspace setup failed")
	}
	// Merge owned configuration fields only. Never select or overwrite credentials.
	for _, item := range []struct {
		id, platform, role string
		config             []byte
		enabled            bool
	}{{destination, "telegram", "control", targetJSON, true}, {source, "clickup", "source", srcJSON, false}} {
		var actual string
		err := tx.QueryRow(ctx, `INSERT INTO integrations(id,workspace_id,platform,role,config,is_enabled) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(id) DO UPDATE SET config=integrations.config || EXCLUDED.config,updated_at=now() WHERE integrations.workspace_id=EXCLUDED.workspace_id AND integrations.platform=EXCLUDED.platform AND integrations.role IN (EXCLUDED.role,'both') RETURNING id`, item.id, workspace, item.platform, item.role, item.config, item.enabled).Scan(&actual)
		if err != nil {
			return errors.New("integration setup failed: IDs must belong to this workspace and platform")
		}
	}
	if tx.Commit(ctx) != nil {
		return errors.New("setup commit failed")
	}
	return json.NewEncoder(out).Encode(map[string]string{"workspace_id": workspace, "source_id": source, "target_id": destination, "callback_path": "/webhook/clickup/" + source})
}
