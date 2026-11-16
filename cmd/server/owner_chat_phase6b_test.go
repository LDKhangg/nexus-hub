package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func ownerChatRepo(t *testing.T) (context.Context, *storage.IntegrationRepository, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("integration test requires dedicated local TEST_DATABASE_URL")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid TEST_DATABASE_URL")
	}
	cc := cfg.ConnConfig
	if cc.Host != "127.0.0.1" || cc.Port != 5434 || cc.Database != "nexus_storage_test" || len(cc.Fallbacks) != 0 {
		t.Fatal("unsafe test database target")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	admin, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		t.Fatal("test DB connection failed")
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	if _, err := admin.Exec(ctx, `SELECT pg_advisory_lock(762150021)`); err != nil {
		t.Fatal("cannot acquire schema lock")
	}
	if _, err := admin.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal("cannot reset authorized schema")
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	files, _ := filepath.Glob("../../db/migrations/*.sql")
	if len(files) == 0 {
		t.Fatal("missing migrations")
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(b), "-- +goose Down")
		if _, err := pool.Exec(ctx, up); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := storage.NewIntegrationRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, repo, pool
}

// TC-A3.1 (REQ-3): the workspace's own group wins over the global
// owner_telegram_chat_id; a workspace without one falls back to it; a
// command reply keeps its chat and topic; another workspace's group is never
// used.
func TestPhase6bOwnerChatSelection(t *testing.T) {
	ctx, repo, pool := ownerChatRepo(t)
	const (
		wsA, wsB, wsC = "019a6690-abcd-7000-8000-000000000001", "019a6690-abcd-7000-8000-000000000002", "019a6690-abcd-7000-8000-000000000003"
		intA1, intB   = "019a6690-abcd-7000-8000-000000000011", "019a6690-abcd-7000-8000-000000000012"
		intC          = "019a6690-abcd-7000-8000-000000000013"
		intD          = "019a6690-abcd-7000-8000-000000000014"
	)
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,display_name) VALUES($1,'A'),($2,'B'),($3,'C')`, wsA, wsB, wsC); err != nil {
		t.Fatal(err)
	}
	mk := func(id, ws string, role tenant.IntegrationRole, enabled bool, cfg map[string]any) {
		t.Helper()
		if _, err := repo.Create(ctx, storage.CreateIntegrationInput{ID: id, WorkspaceID: ws, Platform: "telegram", Role: role, Config: cfg, IsEnabled: enabled}); err != nil {
			t.Fatal(err)
		}
	}
	mk(intA1, wsA, tenant.IntegrationRoleControl, true, map[string]any{"owner_chat_id": float64(-100111)})
	mk(intB, wsB, tenant.IntegrationRoleControl, true, map[string]any{"chat_id": "-100222"})
	mk(intC, wsC, tenant.IntegrationRoleControl, true, map[string]any{})
	o := ownerChats{integrations: repo, ownerChatID: 555}

	got, err := o.OwnerChat(ctx, wsA, behavior.TriggerData{})
	if err != nil || got.ChatID != -100111 || got.IntegrationID != intA1 || got.ThreadID != 0 {
		t.Fatalf("workspace A: %+v err=%v", got, err)
	}
	if got, err = o.OwnerChat(ctx, wsB, behavior.TriggerData{}); err != nil || got.ChatID != -100222 {
		t.Fatalf("workspace B (chat_id): %+v err=%v", got, err)
	}
	if got, err = o.OwnerChat(ctx, wsC, behavior.TriggerData{}); err != nil || got.ChatID != 555 || got.IntegrationID != intC {
		t.Fatalf("workspace C falls back to the global chat: %+v err=%v", got, err)
	}
	// A reply to a command stays in the command's chat and thread.
	if got, err = o.OwnerChat(ctx, wsA, behavior.TriggerData{IntegrationID: intA1, TelegramChatID: -100111, TelegramThreadID: 9}); err != nil || got.ChatID != -100111 || got.ThreadID != 9 {
		t.Fatalf("command reply: %+v err=%v", got, err)
	}
	// No global chat and no own group: an error, never another workspace's chat.
	none := ownerChats{integrations: repo}
	if got, err = none.OwnerChat(ctx, wsC, behavior.TriggerData{}); err == nil {
		t.Fatalf("expected an error without any owner chat, got %+v", got)
	}
	// A workspace with no integration never receives another workspace's group.
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,display_name) VALUES('019a6690-abcd-7000-8000-000000000004','D')`); err != nil {
		t.Fatal(err)
	}
	if got, err = o.OwnerChat(ctx, "019a6690-abcd-7000-8000-000000000004", behavior.TriggerData{}); err == nil {
		t.Fatalf("empty workspace got chat %+v", got)
	}
	// A disabled control integration is skipped.
	mk(intD, wsC, tenant.IntegrationRoleControl, false, map[string]any{"owner_chat_id": float64(-100999)})
	if got, err = o.OwnerChat(ctx, wsC, behavior.TriggerData{}); err != nil || got.ChatID != 555 {
		t.Fatalf("disabled integration used: %+v err=%v", got, err)
	}
}
