package workflow

import (
	"reflect"
	"strings"
	"testing"
)

// TC-A3.1 (REQ-3): owner-facing workflow references round-trip and parsing is
// explicit about malformed, zero, negative, and overflowing identifiers.
func TestPhase6WorkflowReferenceRoundTripAndInvalidInputs(t *testing.T) {
	for _, id := range []int64{1, 42, 1<<63 - 1} {
		ref := FormatRef(id)
		got, ok := ParseRef(ref)
		if !ok || got != id {
			t.Fatalf("ParseRef(%q) = (%d, %v), want (%d, true)", ref, got, ok, id)
		}
	}
	if got, ok := ParseRef("  wf-42\n"); !ok || got != 42 {
		t.Fatalf("case-insensitive trimmed reference = (%d, %v), want (42, true)", got, ok)
	}
	for _, bad := range []string{"", "WF-", "42", "WF-0", "WF--1", "WF-1x", "xWF-1", "WF-9223372036854775808"} {
		if got, ok := ParseRef(bad); ok || got != 0 {
			t.Errorf("ParseRef(%q) = (%d, %v), want (0, false)", bad, got, ok)
		}
	}
}

// TC-A3.2 (REQ-3): references embedded in owner text are unique, word
// bounded, ordered, Unicode-aware, overflow-safe, and capped at ten.
func TestPhase6FindWorkflowReferencesBoundariesDedupAndCap(t *testing.T) {
	text := "WF-2, wf-2; (WF-7) xWF-8 WF-9x _WF-10 WF-11_ áWF-12 WF-12 " +
		"WF-0 WF-9223372036854775808 WF-13 " + strings.Repeat("WF-14 ", 2) +
		"WF-15 WF-16 WF-17 WF-18 WF-19 WF-20 WF-21 WF-22 WF-23"
	want := []int64{2, 7, 12, 13, 14, 15, 16, 17, 18, 19}
	if got := FindRefs(text); !reflect.DeepEqual(got, want) {
		t.Fatalf("FindRefs() = %v, want %v", got, want)
	}
}
