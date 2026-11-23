package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
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
	thread := uuid.MustParse("019a6690-abcd-7000-8000-0000000000c3").String()
	if _, err := pool.Exec(ctx, `INSERT INTO hub_threads(id,workspace_id,integration_id,thread_type,external_id) VALUES($1,$2,$3,'task','retention-cli')`, thread, workspace, integration); err != nil {
		t.Fatal(err)
	}
	var messageID int64
	if err := pool.QueryRow(ctx, `INSERT INTO message_logs(thread_id,sender_role,content,created_at)
		VALUES($1,'system','TOP-SECRET-PAYLOAD',now()-interval '40 days') RETURNING id`, thread).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO notification_outbox(workspace_id,source_integration_id,target_integration_id,event_key,event,message_id,status,updated_at)
		VALUES($1,$2,$2,'retention-cli','{}',$3,'sent',now()-interval '40 days')`, workspace, integration, messageID); err != nil {
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
	if !strings.Contains(show, "enabled=true") || !strings.Contains(show, "outbound_days=30") ||
		!strings.Contains(show, "candidates now (dry-run is exact only below five batches per group)") || !strings.Contains(show, "outbound_actions=1") {
		t.Fatalf("show output: %s", show)
	}
	if !strings.Contains(dry, "message_logs=1") || !strings.Contains(dry, "notification_outbox=1") ||
		!strings.Contains(dry, "outbound_actions=1") || strings.Contains(dry, "enabled=") {
		t.Fatalf("dry-run output: %s", dry)
	}
	var messages, outbox int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_logs WHERE id=$1`, messageID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE message_id=$1`, messageID).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if messages != 1 || outbox != 1 {
		t.Fatalf("dry-run deleted rows: message_logs=%d notification_outbox=%d", messages, outbox)
	}
	for _, out := range []string{show, dry} {
		if strings.Contains(out, "TOP-SECRET") || strings.Contains(out, workspace) || strings.Contains(out, integration) {
			t.Fatalf("private data leaked: %s", out)
		}
	}
	var partial bytes.Buffer
	writeRetentionResult(&partial, behavior.RetentionResult{Counts: map[string]int64{"outbound_actions": 5}, Partial: true})
	if got := partial.String(); !strings.Contains(got, "outbound_actions=5") || !strings.Contains(got, "status=partial (cap reached)") {
		t.Fatalf("partial output: %s", got)
	}
	deleted, err := run("run", "--workspace-id", workspace)
	if err != nil || deleted != dry {
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
