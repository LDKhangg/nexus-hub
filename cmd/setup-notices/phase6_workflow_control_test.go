package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/workflow"
)

// TC-B15 (REQ-11): workflow show groups resources and reveals external IDs
// only for the explicit allowlist.
func TestPhase6WorkflowShow_ResourceMetadataAllowlist(t *testing.T) {
	items := workflowResourceMetadata([]workflow.ResourceBinding{
		{Kind: "secret.ticket", ExternalID: "must-not-leak"},
		{Kind: "github.pr", ExternalID: "repo#2"},
		{Kind: "github.pr", ExternalID: "repo#1"},
	})
	got := fmt.Sprint(items)
	if strings.Contains(got, "must-not-leak") || !strings.Contains(got, "secret.ticket") || !strings.Contains(got, "repo#1 repo#2") {
		t.Fatalf("resource metadata=%s", got)
	}
}

// TC-B16 (REQ-9, REQ-11): instruction views expose metadata and the highest
// checkpointed version, never instruction text.
func TestPhase6WorkflowShow_InstructionMetadataOnly(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	secret := "add integration test -- private owner wording"
	meta := workflowInstructionMetadata([]workflow.Instruction{
		{Version: 1, Text: secret, CreatedAt: now},
		{Version: 2, Text: "new " + secret, CreatedAt: now.Add(time.Minute)},
	}, []workflow.Step{{Checkpoint: &workflow.Checkpoint{References: []workflow.CheckpointReference{
		{Kind: workflow.CheckpointOwnerInstructions, ReferenceID: "1"},
	}}}})
	got := fmt.Sprint(meta)
	if strings.Contains(got, secret) {
		t.Fatalf("instruction text leaked: %s", got)
	}
	if meta["count"] != 2 || meta["latest_version"] != int64(2) || meta["used_version"] != int64(1) {
		t.Fatalf("instruction metadata=%v", meta)
	}
}
