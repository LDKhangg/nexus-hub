package main

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func clearPGConnectionEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGSERVICE", "PGSERVICEFILE"} {
		t.Setenv(name, "")
	}
}

// TC-Fb-1 (REQ-8).
func TestValidateDemoDatabaseURLAllowsOnlyLocalDevelopmentDatabases(t *testing.T) {
	clearPGConnectionEnv(t)
	for _, raw := range []string{
		"postgres://demo:fake@localhost:5432/nexus_dev?sslmode=disable",
		"postgres://demo:fake@127.0.0.1:5432/nexus_test?sslmode=disable",
		"postgres://demo:fake@[::1]:5432/nexus_dev?sslmode=disable",
	} {
		cfg, err := validateDemoDatabaseURL(raw)
		if err != nil {
			t.Errorf("valid loopback URL rejected: %v", err)
			continue
		}
		if cfg.ConnString() != raw {
			t.Errorf("validated configuration changed URL: got %q", cfg.ConnString())
		}
	}
}

// TC-Fb-2 (REQ-8).
func TestDemoSeedRejectsUnsafeDatabaseURLsBeforeOutput(t *testing.T) {
	clearPGConnectionEnv(t)
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"remote", "postgres://demo:fake@db.example.invalid/nexus_test", "demo seed requires a loopback database host"},
		{"remote development name", "postgres://demo:fake@db.example.invalid/nexus_dev", "demo seed requires a loopback database host"},
		{"production name", "postgres://demo:fake@localhost/nexus", "demo seed requires a database name ending in _dev or _test"},
		{"unix socket", "postgres://demo:fake@localhost/nexus_test?host=%2Fvar%2Frun%2Fpostgresql", "demo seed requires a loopback database host"},
		{"query host override", "postgres://demo:fake@localhost/nexus_test?host=db.example.invalid", "demo seed requires a loopback database host"},
		{"multiple hosts", "postgres://demo:fake@localhost:5432,127.0.0.1:5433/nexus_test", "demo seed does not allow fallback database hosts"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := run([]string{"seed", "demo"}, &out, func(name string) (string, bool) {
				if name == "DATABASE_URL" {
					return tt.raw, true
				}
				return "", false
			})
			if err == nil || err.Error() != tt.want {
				t.Fatalf("unsafe URL error = %v, want %q", err, tt.want)
			}
			if out.Len() != 0 || strings.Contains(err.Error(), tt.raw) {
				t.Fatal("refusal exposed the database URL or wrote command output")
			}
		})
	}
}

// TC-Fb-3 (REQ-8).
func TestDemoSeedRejectsUnsafePGEnvironmentOverrides(t *testing.T) {
	tests := []struct {
		name     string
		envName  string
		envValue string
		raw      string
		want     string
	}{
		{"host", "PGHOST", "db.example.invalid", "postgres:///nexus_test?sslmode=disable", "demo seed requires a loopback database host"},
		{"database", "PGDATABASE", "nexus_prod", "postgres://localhost?sslmode=disable", "demo seed requires a database name ending in _dev or _test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearPGConnectionEnv(t)
			t.Setenv(tt.envName, tt.envValue)
			var out bytes.Buffer
			err := run([]string{"seed", "demo"}, &out, func(name string) (string, bool) {
				return tt.raw, name == "DATABASE_URL"
			})
			if err == nil || err.Error() != tt.want {
				t.Fatalf("unsafe %s override error = %v, want %q", tt.envName, err, tt.want)
			}
			if out.Len() != 0 || strings.Contains(err.Error(), tt.raw) || strings.Contains(err.Error(), tt.envValue) {
				t.Fatal("refusal exposed configuration or wrote command output")
			}
		})
	}
}

type demoSeedOutput struct {
	WorkspaceID           string   `json:"workspace_id"`
	ClickUpSourceID       string   `json:"clickup_source_id"`
	TelegramIntegrationID string   `json:"telegram_integration_id"`
	PersonIDs             []string `json:"person_ids"`
	BehaviorID            int64    `json:"behavior_id"`
	WebhookSecret         string   `json:"webhook_secret"`
}

// TC-Fb-4, TC-Fb-5, TC-Fb-6, TC-Fb-7 (REQ-8).
func TestDemoSeedCreatesSafeIdempotentFixtureInOldestWorkspace(t *testing.T) {
	ctx, pool := setupDB(t)
	oldest, newer := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,display_name,created_at) VALUES
		($1,'Existing Oldest',now()-interval '1 day'),($2,'Existing Newer',now())`, oldest, newer); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupDemoSeedRows(t, pool, oldest, newer) })

	for _, args := range [][]string{nil, {"demo", "extra"}, {"typo"}} {
		var out bytes.Buffer
		if err := seedCommand(ctx, pool, args, &out); err == nil || err.Error() != "seed requires exactly: seed demo" {
			t.Fatalf("invalid arguments %q were accepted: %v", args, err)
		}
		if out.Len() != 0 {
			t.Fatalf("invalid arguments %q wrote output", args)
		}
	}
	assertWorkspaceCount(t, ctx, pool, 2)

	firstRaw := runDemoSeed(t, ctx, pool)
	secondRaw := runDemoSeed(t, ctx, pool)
	var first, second demoSeedOutput
	if err := json.Unmarshal(firstRaw, &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(secondRaw, &second); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("second seed changed IDs: first=%+v second=%+v", first, second)
	}
	if first.WorkspaceID != oldest.String() || first.WebhookSecret != demoWebhookSecret || len(first.PersonIDs) != 2 {
		t.Fatalf("unexpected seed output: %+v", first)
	}
	for _, id := range append([]string{first.WorkspaceID, first.ClickUpSourceID, first.TelegramIntegrationID}, first.PersonIDs...) {
		if _, err := uuid.Parse(id); err != nil {
			t.Fatalf("output contains invalid UUID %q", id)
		}
	}
	assertSeedOutputKeys(t, firstRaw)
	assertSeedRows(t, ctx, pool, oldest, first)

	var configs string
	if err := pool.QueryRow(ctx, `SELECT string_agg(config::text,' ') FROM integrations WHERE workspace_id=$1`, oldest).Scan(&configs); err != nil {
		t.Fatal(err)
	}
	sensitive := string(firstRaw) + configs
	for _, pattern := range []string{`gh[pousr]_[A-Za-z0-9]{20,}`, `sk-ant-[A-Za-z0-9_-]{20,}`, `sk-[A-Za-z0-9]{20,}`, `[0-9]{8,10}:[A-Za-z0-9_-]{30,}`, `postgres(?:ql)?://`} {
		if regexp.MustCompile(pattern).MatchString(sensitive) {
			t.Fatalf("seed contains credential or DSN shape matching %q", pattern)
		}
	}
	for _, want := range []string{"NEXUS_DEMO_TELEGRAM_TOKEN", "NEXUS_DEMO_WEBHOOK_SECRET"} {
		if !strings.Contains(configs, want) {
			t.Errorf("seeded integration config lacks fake reference %s", want)
		}
	}
}

func runDemoSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := seedCommand(ctx, pool, []string{"demo"}, &out); err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), out.Bytes()...)
}

func assertWorkspaceCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspaces`).Scan(&got); err != nil || got != want {
		t.Fatalf("workspace count = %d, want %d: %v", got, want, err)
	}
}

func assertSeedOutputKeys(t *testing.T, raw []byte) {
	t.Helper()
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(values))
	for key := range values {
		got = append(got, key)
	}
	sort.Strings(got)
	want := []string{"behavior_id", "clickup_source_id", "person_ids", "telegram_integration_id", "webhook_secret", "workspace_id"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("seed output keys = %v, want %v", got, want)
	}
}

func assertSeedRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID uuid.UUID, output demoSeedOutput) {
	t.Helper()
	queries := []struct {
		name  string
		query string
		want  int
	}{
		{"integrations", `SELECT count(*) FROM integrations WHERE workspace_id=$1`, 2},
		{"contacts", `SELECT count(*) FROM contacts WHERE workspace_id=$1 AND note='Demo seed'`, 2},
		{"accounts", `SELECT count(*) FROM contact_accounts WHERE workspace_id=$1 AND source='manual'`, 2},
		{"behavior", `SELECT count(*) FROM behaviors WHERE workspace_id=$1 AND name='demo-notice' AND status='disabled'`, 1},
	}
	for _, check := range queries {
		var got int
		if err := pool.QueryRow(ctx, check.query, workspaceID).Scan(&got); err != nil || got != check.want {
			t.Errorf("%s count = %d, want %d: %v", check.name, got, check.want, err)
		}
	}
	var behaviorID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM behaviors WHERE workspace_id=$1 AND name='demo-notice'`, workspaceID).Scan(&behaviorID); err != nil || behaviorID != output.BehaviorID {
		t.Errorf("behavior id = %d, want %d: %v", behaviorID, output.BehaviorID, err)
	}
	assertWorkspaceCount(t, ctx, pool, 2)
}

func cleanupDemoSeedRows(t *testing.T, pool *pgxpool.Pool, workspaceIDs ...uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	for _, query := range []string{
		`DELETE FROM behaviors WHERE workspace_id=ANY($1)`,
		`DELETE FROM contact_accounts WHERE workspace_id=ANY($1)`,
		`DELETE FROM contacts WHERE workspace_id=ANY($1)`,
		`DELETE FROM integrations WHERE workspace_id=ANY($1)`,
		`DELETE FROM workspaces WHERE id=ANY($1)`,
	} {
		if _, err := pool.Exec(ctx, query, workspaceIDs); err != nil {
			t.Errorf("clean up demo seed rows: %v", err)
		}
	}
}
