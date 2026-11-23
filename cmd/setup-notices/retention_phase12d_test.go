package main

import (
	"bytes"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// TC-D-17 (REQ-15): operators can distinguish a time-bounded partial sweep
// from a row-cap partial sweep using the exact stable status line.
func TestPhase12dRetentionPartialReasonOutput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason behavior.RetentionPartialReason
		want   string
	}{
		{"deadline", behavior.RetentionPartialDeadline, "status=partial (time limit reached, more rows may remain)\n"},
		{"cap", behavior.RetentionPartialCap, "status=partial (cap reached, more rows may remain)\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			writeRetentionResult(&out, behavior.RetentionResult{Partial: true, PartialReason: tc.reason})
			if got := out.String(); got != tc.want {
				t.Fatalf("output=%q want=%q", got, tc.want)
			}
		})
	}
}
