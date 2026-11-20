package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TC-C-12..13 (REQ-11): show and dry-run agree, run deletes, scope works,
// output is count-only, and invalid arguments fail safely.
func TestPhase12cRetentionCLI(t *testing.T) {
	ctx, pool := setupDB(t)
	workspace := uuid.MustParse("019a6690-abcd-7000-8000-0000000000c1").String()
	integration := uuid.MustParse("019a6690-abcd-7000-8000-0000000000c2").String()
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces(id,display_name) VALUES($1,'Retention CLI')`, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO integrations(id,workspace_id,platform) VALUES($1,$2,'telegram')`, integration, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO outbound_actions(workspace_id,integration_id,kind,payload,dedupe_key,status,updated_at)
		VALUES($1,$2,'telegram_message','{"text":"TOP-SECRET-PAYLOAD"}','retention-cli','sent',now()-interval '40 days')`, workspace, integration); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		t.Helper()
		var out bytes.Buffer
		err := retentionCommand(ctx, pool, args, &out)
		return out.String(), err
	}
	show, err := run("show", "--workspace-id", workspace)
	if err != nil {
		t.Fatal(err)
	}
	dry, err := run("run", "--workspace-id", workspace, "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(show, "enabled=true") || !strings.Contains(show, "outbound_days=30") || !strings.Contains(show, "outbound_actions=1") {
		t.Fatalf("show output: %s", show)
	}
	if !strings.Contains(dry, "outbound_actions=1") || strings.Contains(dry, "enabled=") {
		t.Fatalf("dry-run output: %s", dry)
	}
	for _, out := range []string{show, dry} {
		if strings.Contains(out, "TOP-SECRET") || strings.Contains(out, workspace) || strings.Contains(out, integration) {
			t.Fatalf("private data leaked: %s", out)
		}
	}
	deleted, err := run("run", "--workspace-id", workspace)
	if err != nil || !strings.Contains(deleted, "outbound_actions=1") {
		t.Fatalf("run output=%s err=%v", deleted, err)
	}
	after, err := run("run", "--workspace-id", workspace, "--dry-run")
	if err != nil || !strings.Contains(after, "outbound_actions=0") {
		t.Fatalf("after output=%s err=%v", after, err)
	}

	bad := [][]string{
		nil,
		{"unknown"},
		{"show", "--dry-run"},
		{"run", "--workspace-id", "not-a-uuid"},
		{"run", "--workspace-id", uuid.MustParse("019a6690-abcd-7000-8000-0000000000ff").String()},
		{"run", "extra"},
	}
	for _, args := range bad {
		out, err := run(args...)
		if err == nil {
			t.Errorf("accepted args %v, output %q", args, out)
		}
		if strings.Contains(err.Error(), "TOP-SECRET") || strings.Contains(out, "TOP-SECRET") {
			t.Fatalf("bad-argument response leaked payload: %v %q", err, out)
		}
	}
}
