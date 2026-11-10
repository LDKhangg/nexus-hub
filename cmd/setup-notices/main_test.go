package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func setupDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires dedicated local TEST_DATABASE_URL")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid test URL")
	}
	if cfg.ConnConfig.Host != "127.0.0.1" || cfg.ConnConfig.Database != "nexus_storage_test" || len(cfg.ConnConfig.Fallbacks) != 0 {
		t.Fatal("test URL must use dedicated loopback database with sslmode=disable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal("test connection failed")
	}
	name := "nexus_setup_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_, err := admin.Exec(c, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		if err != nil {
			t.Error(err)
		}
		_ = admin.Close(c)
	})
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	files, _ := filepath.Glob("../../db/migrations/*.sql")
	if len(files) == 0 {
		t.Fatal("migrations missing")
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(data), "-- +goose Down")
		if _, err := pool.Exec(ctx, up); err != nil {
			t.Fatal(err)
		}
	}
	return ctx, pool
}

func TestExplicitDatabaseRequired(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"inspect"}, &out, func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("missing DB accepted")
	}
}

func TestSetupLifecycle(t *testing.T) {
	ctx, pool := setupDB(t)
	var out bytes.Buffer
	secret := func(string) (string, bool) { return "private-value", true }
	args := []string{"init", "--workspace-name", "Personal", "--chat-id", "-10042", "--team-id", "team"}
	if err := runCommand(ctx, pool, args, &out, secret); err != nil {
		t.Fatal(err)
	}
	var ids struct {
		WorkspaceID string `json:"workspace_id"`
		SourceID    string `json:"source_id"`
		TargetID    string `json:"target_id"`
		Callback    string `json:"callback_path"`
	}
	if err := json.Unmarshal(out.Bytes(), &ids); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{ids.WorkspaceID, ids.SourceID, ids.TargetID} {
		parsed, err := uuid.Parse(id)
		if err != nil || parsed.Version() != 7 {
			t.Fatalf("not UUIDv7: %s", id)
		}
	}
	if ids.Callback != "/webhook/clickup/"+ids.SourceID || strings.Contains(out.String(), "private-value") {
		t.Fatal("unsafe/incorrect output")
	}
	var enabled bool
	if err := pool.QueryRow(ctx, "SELECT is_enabled FROM integrations WHERE id=$1", ids.SourceID).Scan(&enabled); err != nil || enabled {
		t.Fatal("unbound source enabled")
	}
	_, err := pool.Exec(ctx, `UPDATE integrations SET config=config || '{"unrelated":"keep"}'::jsonb, encrypted_credentials='opaque-test-value' WHERE id=$1`, ids.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	args = append(args, "--workspace-id", ids.WorkspaceID, "--source-id", ids.SourceID, "--target-id", ids.TargetID)
	if err := runCommand(ctx, pool, args, &out, secret); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM integrations").Scan(&count); err != nil || count != 2 {
		t.Fatal("rerun created duplicate integrations")
	}
	bind := []string{"bind-webhook", "--source-id", ids.SourceID, "--webhook-id", "w1"}
	if err := runCommand(ctx, pool, bind, &out, func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("missing secret enabled source")
	}
	if err := runCommand(ctx, pool, bind, &out, secret); err != nil {
		t.Fatal(err)
	}
	var keep, cipher string
	if err := pool.QueryRow(ctx, `SELECT config->>'unrelated',encrypted_credentials,is_enabled FROM integrations WHERE id=$1`, ids.SourceID).Scan(&keep, &cipher, &enabled); err != nil || keep != "keep" || cipher != "opaque-test-value" || !enabled {
		t.Fatal("binding lost config/credentials")
	}
	other := uuid.NewString()
	if _, err := pool.Exec(ctx, "INSERT INTO workspaces(id,display_name) VALUES($1,'Other')", other); err != nil {
		t.Fatal(err)
	}
	wrong := append([]string{}, args...)
	for i := range wrong {
		if wrong[i] == ids.WorkspaceID {
			wrong[i] = other
		}
	}
	if err := runCommand(ctx, pool, wrong, &out, secret); err == nil {
		t.Fatal("cross-workspace ID reuse accepted")
	}
	if err := runCommand(ctx, pool, []string{"inspect"}, &out, secret); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "private-value") || strings.Contains(out.String(), "opaque-test-value") {
		t.Fatal("secret leaked")
	}
}

func TestRetryFailedOnly(t *testing.T) {
	ctx, pool := setupDB(t)
	w, s, target, thread := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, q := range []string{fmt.Sprintf("INSERT INTO workspaces(id,display_name) VALUES('%s','Test')", w), fmt.Sprintf("INSERT INTO integrations(id,workspace_id,platform) VALUES('%s','%s','clickup'),('%s','%s','telegram')", s, w, target, w), fmt.Sprintf("INSERT INTO hub_threads(id,workspace_id,integration_id,thread_type,external_id) VALUES('%s','%s','%s','task','t')", thread, w, s), fmt.Sprintf("INSERT INTO message_logs(thread_id,sender_role,content) VALUES('%s','external_user','test')", thread)} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for _, status := range []string{"failed", "sent", "processing", "pending"} {
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO notification_outbox(workspace_id,source_integration_id,target_integration_id,event_key,event,message_id,status,attempts) VALUES($1,$2,$3,$4,'{}',1,$4,8) RETURNING id`, w, s, target, status).Scan(&id); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err := runCommand(ctx, pool, []string{"retry-failed", "--job-id", fmt.Sprint(id)}, &out, nil)
		if (err == nil) != (status == "failed") {
			t.Fatalf("retry %s: %v", status, err)
		}
		if status == "failed" {
			var attempts int
			var actual string
			if err := pool.QueryRow(ctx, "SELECT status,attempts FROM notification_outbox WHERE id=$1", id).Scan(&actual, &attempts); err != nil || actual != "pending" || attempts != 0 {
				t.Fatal("failed job not reset")
			}
		}
	}
}
