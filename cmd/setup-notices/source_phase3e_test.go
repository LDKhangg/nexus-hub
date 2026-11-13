package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestPhase3ESourceEnableDisablePollOnly(t *testing.T) {
	// TC-74 (REQ-E9, TD3d-4).
	ctx, pool := setupDB(t)
	_, source, _ := initWorkspace(t, ctx, pool)
	if _, err := pool.Exec(ctx, `UPDATE integrations SET config=jsonb_set(config,'{clickup_team_id}',to_jsonb('123'::text)),is_enabled=false WHERE id=$1`, source); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := sourceCommand(ctx, pool, []string{"enable", "--source-id", source}, &out); err != nil {
		t.Fatal(err)
	}
	var enabled bool
	if err := pool.QueryRow(ctx, `SELECT is_enabled FROM integrations WHERE id=$1`, source).Scan(&enabled); err != nil || !enabled {
		t.Fatalf("enabled=%v err=%v output=%s", enabled, err, out.String())
	}
	out.Reset()
	if err := sourceCommand(ctx, pool, []string{"disable", "--source-id", source}, &out); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT is_enabled FROM integrations WHERE id=$1`, source).Scan(&enabled); err != nil || enabled {
		t.Fatalf("enabled=%v err=%v output=%s", enabled, err, out.String())
	}
}

func TestPhase3ESourceEnableFailsClosedWithoutTeam(t *testing.T) {
	// TC-75 (REQ-E9, TD3d-4).
	ctx, pool := setupDB(t)
	_, source, _ := initWorkspace(t, ctx, pool)
	if _, err := pool.Exec(ctx, `UPDATE integrations SET config=config-'clickup_team_id',is_enabled=false WHERE id=$1`, source); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := sourceCommand(context.Background(), pool, []string{"enable", "--source-id", source}, &out)
	if err == nil || !strings.Contains(err.Error(), "clickup_team_id") {
		t.Fatalf("error=%v", err)
	}
	var enabled bool
	if err := pool.QueryRow(ctx, `SELECT is_enabled FROM integrations WHERE id=$1`, source).Scan(&enabled); err != nil || enabled {
		t.Fatalf("partial enable: %v %v", enabled, err)
	}
	for _, args := range [][]string{{"enable"}, {"enable", "--source-id", "not-a-uuid"}, {"enable", "--source-id", "00000000-0000-4000-8000-000000000000"}} {
		if err := sourceCommand(ctx, pool, args, &out); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
