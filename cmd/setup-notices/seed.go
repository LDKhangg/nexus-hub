package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const demoWebhookSecret = "demo-webhook-secret-not-for-production"

func validateDemoDatabaseURL(databaseURL string) error {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return errors.New("invalid database configuration")
	}
	host := strings.TrimSpace(config.ConnConfig.Host)
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("demo seed requires a loopback database host")
	}
	database := config.ConnConfig.Database
	if !strings.HasSuffix(database, "_dev") && !strings.HasSuffix(database, "_test") {
		return errors.New("demo seed requires a database name ending in _dev or _test")
	}
	return nil
}

func seedCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	if len(args) != 1 || args[0] != "demo" {
		return errors.New("seed requires exactly: seed demo")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return errors.New("demo seed failed")
	}
	defer tx.Rollback(ctx)

	var workspaceID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM workspaces ORDER BY created_at, id LIMIT 1`).Scan(&workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		workspaceID = uuid.NewSHA1(uuid.NameSpaceURL, []byte("nexus-hub/demo/workspace"))
		if _, err = tx.Exec(ctx, `INSERT INTO workspaces(id,display_name) VALUES($1,'Demo Workspace') ON CONFLICT(id) DO NOTHING`, workspaceID); err != nil {
			return errors.New("demo seed failed")
		}
	} else if err != nil {
		return errors.New("demo seed failed")
	}

	demoID := func(kind string) uuid.UUID {
		return uuid.NewSHA1(workspaceID, []byte("nexus-hub/demo/"+kind))
	}
	telegramID := demoID("telegram")
	clickupID := demoID("clickup")
	telegramConfig, _ := json.Marshal(map[string]any{
		"chat_id": "-1000000000001", "bot_token_env": "NEXUS_DEMO_TELEGRAM_TOKEN",
	})
	clickupConfig, _ := json.Marshal(map[string]any{
		"clickup_team_id": "demo-team", "webhook_id": "demo-webhook",
		"webhook_secret_env": "NEXUS_DEMO_WEBHOOK_SECRET", "target_integration_id": telegramID.String(),
		"own_sender_id": "demo-owner",
	})
	for _, integration := range []struct {
		id       uuid.UUID
		platform string
		role     string
		config   []byte
	}{
		{telegramID, "telegram", "control", telegramConfig},
		{clickupID, "clickup", "source", clickupConfig},
	} {
		if _, err = tx.Exec(ctx, `INSERT INTO integrations(id,workspace_id,platform,role,config,is_enabled)
			VALUES($1,$2,$3,$4,$5,true)
			ON CONFLICT(id) DO UPDATE SET config=excluded.config,is_enabled=true,updated_at=now()
			WHERE integrations.workspace_id=excluded.workspace_id AND integrations.platform=excluded.platform`,
			integration.id, workspaceID, integration.platform, integration.role, integration.config); err != nil {
			return errors.New("demo seed failed")
		}
	}

	people := []struct {
		kind, name, externalID string
	}{
		{"alice", "Alice Demo", "demo-alice"},
		{"bob", "Bob Demo", "demo-bob"},
	}
	personIDs := make([]string, 0, len(people))
	for _, person := range people {
		contactID := demoID("contact/" + person.kind)
		accountID := demoID("account/" + person.kind)
		if _, err = tx.Exec(ctx, `INSERT INTO contacts(id,workspace_id,display_name,note)
			VALUES($1,$2,$3,'Demo seed') ON CONFLICT(id) DO UPDATE SET display_name=excluded.display_name,updated_at=now()
			WHERE contacts.workspace_id=excluded.workspace_id`, contactID, workspaceID, person.name); err != nil {
			return errors.New("demo seed failed")
		}
		if _, err = tx.Exec(ctx, `INSERT INTO contact_accounts(id,workspace_id,contact_id,platform,external_id,platform_name,source)
			VALUES($1,$2,$3,'clickup',$4,$5,'manual')
			ON CONFLICT(workspace_id,platform,external_id) DO UPDATE SET contact_id=excluded.contact_id,platform_name=excluded.platform_name,last_seen=now()`,
			accountID, workspaceID, contactID, person.externalID, person.name); err != nil {
			return errors.New("demo seed failed")
		}
		personIDs = append(personIDs, contactID.String())
	}

	draft := json.RawMessage(`{"schema":1,"name":"demo-notice","description":"Disabled demo behavior","triggers":[{"type":"clickup.comment_added"}],"instructions":"Create one short informational demo item.","autonomy":{"notify_when_informational":true},"actions":{"notify":[{"type":"telegram.message"}]},"budgets":{"max_items":1},"dry_run":true}`)
	var behaviorID int64
	if err = tx.QueryRow(ctx, `INSERT INTO behaviors(workspace_id,name,status,draft,draft_source)
		VALUES($1,'demo-notice','disabled',$2,$2::text)
		ON CONFLICT(workspace_id,name) DO UPDATE SET status='disabled',draft=excluded.draft,draft_source=excluded.draft_source,updated_at=now()
		RETURNING id`, workspaceID, draft).Scan(&behaviorID); err != nil {
		return errors.New("demo seed failed")
	}
	if err = tx.Commit(ctx); err != nil {
		return errors.New("demo seed failed")
	}

	return json.NewEncoder(out).Encode(map[string]any{
		"workspace_id": workspaceID.String(), "clickup_source_id": clickupID.String(),
		"telegram_integration_id": telegramID.String(), "person_ids": personIDs,
		"behavior_id": behaviorID, "webhook_secret": demoWebhookSecret,
	})
}
