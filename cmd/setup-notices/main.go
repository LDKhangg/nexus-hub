package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.LookupEnv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer, lookup func(string) (string, bool)) error {
	if len(args) > 0 && args[0] == "master-key" {
		return masterKeyInit(args[1:], out) // needs no database
	}
	url, ok := lookup("DATABASE_URL")
	if !ok || url == "" {
		return errors.New("explicit DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return errors.New("invalid database configuration")
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return errors.New("database connection failed")
	}
	return runCommand(ctx, pool, args, out, lookup)
}

func commandFlags(name string) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(io.Discard) // Flags may contain private identifiers; do not echo arguments.
	return f
}

func runCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer, lookup func(string) (string, bool)) error {
	if len(args) == 0 {
		return errors.New("usage: setup-notices init|bind-webhook|inspect|retry-failed|master-key|secret|migrate-secrets|settings|enable-agent|disable-agent|github-account|github-repo|github-check|behavior|telegram")
	}
	switch args[0] {
	case "init":
		return initNotices(ctx, pool, args[1:], out)
	case "master-key":
		return masterKeyInit(args[1:], out)
	case "secret":
		return secretCommand(ctx, pool, args[1:], out, lookup)
	case "migrate-secrets":
		return migrateSecrets(ctx, pool, args[1:], out, lookup)
	case "settings":
		return settingsCommand(ctx, pool, args[1:], out)
	case "enable-agent":
		return setAgentEnabled(ctx, pool, args[0], args[1:], out, true)
	case "disable-agent":
		return setAgentEnabled(ctx, pool, args[0], args[1:], out, false)
	case "github-account":
		return githubAccountCommand(ctx, pool, args[1:], out)
	case "github-repo":
		return githubRepoCommand(ctx, pool, args[1:], out)
	case "behavior":
		return behaviorCommand(ctx, pool, args[1:], out)
	case "telegram":
		return telegramCommand(ctx, pool, args[1:], out, lookup)
	case "github-check":
		return githubCheck(ctx, pool, args[1:], out, lookup)
	case "bind-webhook":
		f := commandFlags(args[0])
		source := f.String("source-id", "", "source integration UUID")
		webhook := f.String("webhook-id", "", "registered webhook ID")
		if f.Parse(args[1:]) != nil || f.NArg() != 0 || !tenant.ValidUUID(*source) || *webhook == "" {
			return errors.New("bind-webhook requires valid --source-id and --webhook-id")
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
		var cfg map[string]any
		if json.Unmarshal(data, &cfg) != nil || cfg == nil {
			return errors.New("invalid source configuration")
		}
		cfg["webhook_id"] = *webhook
		parsed, err := tenant.ParseClickUpNoticeConfig(cfg)
		if err != nil {
			return err
		}
		// DB credential references are checked by the resolver at callback
		// time; legacy env references must exist locally now.
		if envName, legacy := strings.CutPrefix(parsed.WebhookSecretRef, tenant.LegacyEnvRefPrefix); legacy {
			if lookup == nil {
				return errors.New("webhook secret is missing from local environment")
			}
			if secret, ok := lookup(envName); !ok || secret == "" {
				return errors.New("webhook secret is missing from local environment")
			}
		}
		encoded, _ := json.Marshal(cfg)
		if _, err := tx.Exec(ctx, `UPDATE integrations SET config=$2,is_enabled=true,updated_at=now() WHERE id=$1`, *source, encoded); err != nil {
			return errors.New("binding failed")
		}
		if tx.Commit(ctx) != nil {
			return errors.New("binding commit failed")
		}
		return json.NewEncoder(out).Encode(map[string]string{"source_id": *source, "callback_path": "/webhook/clickup/" + *source})
	case "inspect":
		if len(args) != 1 {
			return errors.New("inspect takes no arguments")
		}
		return inspect(ctx, pool, out)
	case "retry-failed":
		f := commandFlags(args[0])
		id := f.Int64("job-id", 0, "failed delivery job ID")
		if f.Parse(args[1:]) != nil || f.NArg() != 0 || *id <= 0 {
			return errors.New("retry-failed requires positive --job-id")
		}
		count, err := pool.Exec(ctx, `UPDATE notification_outbox SET status='pending',attempts=0,next_attempt_at=now(),locked_until=NULL,claim_token=NULL,last_error_code=NULL,updated_at=now() WHERE id=$1 AND status='failed'`, *id)
		if err != nil {
			return errors.New("retry failed")
		}
		if count.RowsAffected() != 1 {
			return errors.New("job not found or not failed; no change made")
		}
		return json.NewEncoder(out).Encode(map[string]any{"job_id": *id, "status": "pending"})
	default:
		return errors.New("unknown setup command")
	}
}
