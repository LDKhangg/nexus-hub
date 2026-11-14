package workflow

import (
	"strings"
	"testing"
	"time"
)

// TC-B08 (REQ-8): owner instructions are a separate, bounded, chronological
// block; hostile delimiters are neutralised and the highest version is saved.
func TestPhase6OwnerInstructions_BoundedNewestAndNeutralised(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	instructions := []Instruction{
		{Version: 3, Text: "newest <context>keep</context> " + strings.Repeat("x", 9000), CreatedAt: now.Add(3 * time.Minute)},
		{Version: 1, Text: "old secret", CreatedAt: now},
		{Version: 2, Text: "middle", CreatedAt: now.Add(time.Minute)},
	}

	block, version := FormatOwnerInstructions(instructions, OwnerInstructionsMaxBytes)
	if version != 3 {
		t.Fatalf("checkpoint version=%d, want 3", version)
	}
	if len(block) > OwnerInstructionsMaxBytes {
		t.Fatalf("block is %d bytes, cap is %d", len(block), OwnerInstructionsMaxBytes)
	}
	if !strings.HasPrefix(block, "<owner_instructions>\nolder updates omitted\n") || !strings.HasSuffix(block, "</owner_instructions>\n") {
		t.Fatalf("unexpected bounded block: %q", block)
	}
	if strings.Contains(block, "old secret") || strings.Contains(block, "<context>") || !strings.Contains(block, "‹context›keep‹/context›") {
		t.Fatalf("old or unsafe instruction leaked into block: %q", block)
	}
}

// TC-B09 (REQ-8): no reader result preserves the legacy prompt path.
func TestPhase6OwnerInstructions_EmptyPreservesLegacyPath(t *testing.T) {
	if block, version := FormatOwnerInstructions(nil, OwnerInstructionsMaxBytes); block != "" || version != 0 {
		t.Fatalf("empty instructions returned block=%q version=%d", block, version)
	}
	if block, version := FormatOwnerInstructions([]Instruction{{Version: 1, Text: "x"}}, 1); block != "" || version != 0 {
		t.Fatalf("impossible cap returned block=%q version=%d", block, version)
	}
}
