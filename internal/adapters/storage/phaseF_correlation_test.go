package storage

import (
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// TC-F-6 (REQ-2): inbound Telegram correlation survives a fresh store instance,
// which models process restart between HTTP receipt and inbox execution.
func TestPhaseFTelegramCorrelationPersistsAcrossRestart(t *testing.T) {
	ctx, pool := testDB(t)
	mustExec(t, ctx, pool, `INSERT INTO workspaces(id,display_name) VALUES($1,'A')`, workspaceA)
	first, err := NewBehaviorStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	const correlation = "019a6690-abcd-7000-8000-0000000000f6"
	inserted, err := first.RecordUpdate(ctx, workspaceA, behavior.InboundUpdate{
		UpdateID: 606, ChatID: 42, Kind: behavior.UpdateCommand, Command: "/status", CorrelationID: correlation,
	})
	if err != nil || !inserted {
		t.Fatalf("record inserted=%v err=%v", inserted, err)
	}

	restarted, err := NewBehaviorStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := restarted.ClaimUpdate(ctx, time.Now(), time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	if claimed.CorrelationID != correlation {
		t.Fatalf("correlation after restart=%q, want %q", claimed.CorrelationID, correlation)
	}

	if ok, err := restarted.RecordUpdate(ctx, workspaceA, behavior.InboundUpdate{
		UpdateID: 607, ChatID: 42, Kind: behavior.UpdateCommand, Command: "/status", CorrelationID: "not-a-uuid",
	}); err == nil || ok {
		t.Fatalf("malformed correlation accepted: inserted=%v err=%v", ok, err)
	}
}
