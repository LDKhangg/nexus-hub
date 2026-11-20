package settings

import (
	"context"
	"encoding/json"
	"testing"
)

// TC-C-1..3 (REQ-11): retention is safe by default, bounded, and zero means forever.
func TestPhase12cRetentionSettingsContract(t *testing.T) {
	d := Defaults()
	if !d.RetentionEnabled || d.RetentionMessageLogsDays != 30 || d.RetentionToolRunsDays != 90 ||
		d.RetentionPolicyDecisionsDays != 180 || d.RetentionWorkflowDays != 90 || d.RetentionOutboundDays != 30 {
		t.Fatalf("unsafe defaults: %+v", d)
	}
	keys := []string{"retention_message_logs_days", "retention_tool_runs_days", "retention_policy_decisions_days", "retention_workflow_days", "retention_outbound_days"}
	for _, key := range keys {
		for _, raw := range []string{`0`, `1`, `3650`} {
			if err := Validate(key, json.RawMessage(raw)); err != nil {
				t.Errorf("%s rejected %s: %v", key, raw, err)
			}
		}
		for _, raw := range []string{`-1`, `3651`, `1.5`, `"30"`, `null`, `true`, `[]`, `{}`} {
			if err := Validate(key, json.RawMessage(raw)); err == nil {
				t.Errorf("%s accepted %s", key, raw)
			}
		}
	}
	if err := Validate("retention_enabled", json.RawMessage(`false`)); err != nil {
		t.Fatal(err)
	}
	if err := Validate("retention_enabled", json.RawMessage(`"false"`)); err == nil {
		t.Fatal("retention_enabled accepted a string")
	}
	s, err := Load(context.Background(), mapStore{
		"retention_enabled":           json.RawMessage(`false`),
		"retention_message_logs_days": json.RawMessage(`0`),
		"retention_tool_runs_days":    json.RawMessage(`1`),
	})
	if err != nil || s.RetentionEnabled || s.RetentionMessageLogsDays != 0 || s.RetentionToolRunsDays != 1 {
		t.Fatalf("override = %+v, %v", s, err)
	}
}
