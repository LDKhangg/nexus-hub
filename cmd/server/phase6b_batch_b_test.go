package main

import (
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// TC-B1-R4b (review R4): a control integration configured with owner_chat_id
// only (no chat_id) is a poll target; one with no chat id at all, a disabled
// one and a non-control one are not.
func TestPhase6bB1_PollTargetsOwnerChatOnly(t *testing.T) {
	ctx, repo, pool := ownerChatRepo(t)
	const (
		ws                   = "019a6690-abcd-7000-8000-0000000000c1"
		ok, bare, off, notif = "019a6690-abcd-7000-8000-0000000000c2", "019a6690-abcd-7000-8000-0000000000c3", "019a6690-abcd-7000-8000-0000000000c4", "019a6690-abcd-7000-8000-0000000000c5"
	)
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,display_name) VALUES($1,'P')`, ws); err != nil {
		t.Fatal(err)
	}
	mk := func(id string, role tenant.IntegrationRole, enabled bool, cfg map[string]any) {
		t.Helper()
		if _, err := repo.Create(ctx, storage.CreateIntegrationInput{ID: id, WorkspaceID: ws, Platform: "telegram", Role: role, Config: cfg, IsEnabled: enabled}); err != nil {
			t.Fatal(err)
		}
	}
	mk(ok, tenant.IntegrationRoleControl, true, map[string]any{"owner_chat_id": float64(-1001234567890), "bot_token_env": "TOKEN"})
	mk(bare, tenant.IntegrationRoleControl, true, map[string]any{"bot_token_env": "TOKEN"})
	mk(off, tenant.IntegrationRoleControl, false, map[string]any{"owner_chat_id": float64(-100), "bot_token_env": "TOKEN"})
	mk(notif, tenant.IntegrationRoleSource, true, map[string]any{"owner_chat_id": float64(-100), "bot_token_env": "TOKEN"})
	wsRepo, err := storage.NewWorkspaceRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	got, err := telegramPollTargets{workspaces: wsRepo, integrations: repo}.TelegramPollTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].IntegrationID != ok || got[0].WorkspaceID != ws || got[0].TokenRef == "" {
		t.Fatalf("targets=%+v", got)
	}
}
