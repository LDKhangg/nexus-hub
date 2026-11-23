package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const demoWebhookSecret = "demo-webhook-secret-not-for-production"

var errDemoSeedConflict = errors.New("demo seed conflicts with existing data")

type demoSeedFailure struct {
	cause error
}

func (e demoSeedFailure) Error() string { return "demo seed failed" }
func (e demoSeedFailure) Unwrap() error { return e.cause }

func seedFailure(statement string, err error) error {
	return demoSeedFailure{cause: fmt.Errorf("%s: %w", statement, err)}
}

func validateLoopbackHost(host string) error {
	host = strings.TrimSpace(host)
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("demo seed requires a loopback database host")
	}
	return nil
}

func guardDemoSeedDial(config *pgxpool.Config) {
	lookup := config.ConnConfig.LookupFunc
	if lookup == nil {
		lookup = net.DefaultResolver.LookupHost
	}
	config.ConnConfig.LookupFunc = func(ctx context.Context, host string) ([]string, error) {
		addrs, err := lookup(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, addr := range addrs {
			addressHost := addr
			if parsedHost, _, splitErr := net.SplitHostPort(addr); splitErr == nil {
				addressHost = parsedHost
			}
			ip := net.ParseIP(strings.Trim(addressHost, "[]"))
			if ip == nil || !ip.IsLoopback() {
				return nil, errors.New("demo seed requires a loopback database host")
			}
		}
		return addrs, nil
	}

	dial := config.ConnConfig.DialFunc
	config.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, errors.New("demo seed requires a loopback database host")
		}
		ip := net.ParseIP(strings.Trim(host, "[]"))
		if ip == nil || !ip.IsLoopback() {
			return nil, errors.New("demo seed requires a loopback database host")
		}
		return dial(ctx, network, addr)
	}
}

func validateDemoDatabaseURL(databaseURL string) (*pgxpool.Config, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, errors.New("invalid database configuration")
	}
	if err := validateLoopbackHost(config.ConnConfig.Host); err != nil {
		return nil, err
	}
	for _, fallback := range config.ConnConfig.Fallbacks {
		if err := validateLoopbackHost(fallback.Host); err != nil {
			return nil, err
		}
	}
	database := config.ConnConfig.Database
	if !strings.HasSuffix(database, "_dev") && !strings.HasSuffix(database, "_test") {
		return nil, errors.New("demo seed requires a database name ending in _dev or _test")
	}
	guardDemoSeedDial(config)
	return config, nil
}

func seedCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	if len(args) != 1 || args[0] != "demo" {
		return errors.New("seed requires exactly: seed demo")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return seedFailure("begin transaction", err)
	}
	defer tx.Rollback(ctx)

	var workspaceID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM workspaces ORDER BY created_at, id LIMIT 1`).Scan(&workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		workspaceID = uuid.NewSHA1(uuid.NameSpaceURL, []byte("nexus-hub/demo/workspace"))
		if _, err = tx.Exec(ctx, `INSERT INTO workspaces(id,display_name) VALUES($1,'Demo Workspace') ON CONFLICT(id) DO NOTHING`, workspaceID); err != nil {
			return seedFailure("insert workspace", err)
		}
	} else if err != nil {
		return seedFailure("select workspace", err)
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
			ON CONFLICT(id) DO NOTHING`,
			integration.id, workspaceID, integration.platform, integration.role, integration.config); err != nil {
			return seedFailure("insert integration", err)
		}
		var matches bool
		if err = tx.QueryRow(ctx, `SELECT workspace_id=$2 AND platform=$3 AND role=$4 AND config=$5::jsonb AND is_enabled
			FROM integrations WHERE id=$1`, integration.id, workspaceID, integration.platform, integration.role, integration.config).Scan(&matches); err != nil {
			return seedFailure("verify integration", err)
		}
		if !matches {
			return errDemoSeedConflict
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
			VALUES($1,$2,$3,'Demo seed') ON CONFLICT(id) DO NOTHING`, contactID, workspaceID, person.name); err != nil {
			return seedFailure("insert contact", err)
		}
		var contactMatches bool
		if err = tx.QueryRow(ctx, `SELECT workspace_id=$2 AND display_name IS NOT DISTINCT FROM $3 AND note='Demo seed'
			FROM contacts WHERE id=$1`, contactID, workspaceID, person.name).Scan(&contactMatches); err != nil {
			return seedFailure("verify contact", err)
		}
		if !contactMatches {
			return errDemoSeedConflict
		}
		if _, err = tx.Exec(ctx, `INSERT INTO contact_accounts(id,workspace_id,contact_id,platform,external_id,platform_name,source)
			VALUES($1,$2,$3,'clickup',$4,$5,'manual')
			ON CONFLICT DO NOTHING`,
			accountID, workspaceID, contactID, person.externalID, person.name); err != nil {
			return seedFailure("insert contact account", err)
		}
		var accountContactID uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT contact_id FROM contact_accounts
			WHERE workspace_id=$1 AND platform='clickup' AND external_id=$2`, workspaceID, person.externalID).Scan(&accountContactID); errors.Is(err, pgx.ErrNoRows) {
			return errDemoSeedConflict
		} else if err != nil {
			return seedFailure("verify contact account", err)
		}
		if accountContactID != contactID {
			return errDemoSeedConflict
		}
		personIDs = append(personIDs, contactID.String())
	}

	draft := json.RawMessage(`{"schema":1,"name":"demo-notice","description":"Disabled demo behavior","triggers":[{"type":"clickup.comment_added"}],"instructions":"Create one short informational demo item.","autonomy":{"notify_when_informational":true},"actions":{"notify":[{"type":"telegram.message"}]},"budgets":{"max_items":1},"dry_run":true}`)
	var behaviorID int64
	if err = tx.QueryRow(ctx, `INSERT INTO behaviors(workspace_id,name,status,draft,draft_source)
		VALUES($1,'demo-notice','disabled',$2,$3)
		ON CONFLICT(workspace_id,name) DO NOTHING
		RETURNING id`, workspaceID, draft, string(draft)).Scan(&behaviorID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return seedFailure("insert behavior", err)
	}
	var behaviorMatches bool
	if err = tx.QueryRow(ctx, `SELECT id,status='disabled' AND draft=$2::jsonb AND draft_source=$3
		FROM behaviors WHERE workspace_id=$1 AND name='demo-notice'`, workspaceID, draft, string(draft)).Scan(&behaviorID, &behaviorMatches); err != nil {
		return seedFailure("verify behavior", err)
	}
	if !behaviorMatches {
		return errDemoSeedConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return seedFailure("commit transaction", err)
	}

	return json.NewEncoder(out).Encode(map[string]any{
		"workspace_id": workspaceID.String(), "clickup_source_id": clickupID.String(),
		"telegram_integration_id": telegramID.String(), "person_ids": personIDs,
		"behavior_id": behaviorID, "webhook_secret": demoWebhookSecret,
	})
}
