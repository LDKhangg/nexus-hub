package settings

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TC-D11 (REQ-18): assistant settings have independent defaults and strict
// model, timeout and per-chat rate boundaries.
func TestPhase5AssistantSettings(t *testing.T) {
	s, err := Load(context.Background(), mapStore{})
	if err != nil {
		t.Fatal(err)
	}
	// Phase 6a REQ-1: assistant_model now defaults to Sonnet.
	if s.AssistantModel != "claude-sonnet-5-5" || s.AssistantTimeout != 180*time.Second || s.AssistantRatePerMinute != 30 {
		t.Fatalf("assistant defaults: model=%q timeout=%s rate=%d", s.AssistantModel, s.AssistantTimeout, s.AssistantRatePerMinute)
	}

	valid := mapStore{
		"assistant_model":           json.RawMessage(`"claude-sonnet-5-5"`),
		"assistant_timeout":         json.RawMessage(`"10s"`),
		"assistant_rate_per_minute": json.RawMessage(`120`),
		"triage_model":              json.RawMessage(`"claude-haiku-5-5"`),
	}
	s, err = Load(context.Background(), valid)
	if err != nil || s.AssistantModel != "claude-sonnet-5-5" || s.AssistantTimeout != 10*time.Second || s.AssistantRatePerMinute != 120 || s.TriageModel != "claude-haiku-5-5" {
		t.Fatalf("independent settings=%+v err=%v", s, err)
	}
	for key, values := range map[string][]string{
		"assistant_model":           {`""`, `"haiku"`, `"gpt-5"`, `"claude-opus-5-5 extra"`},
		"assistant_timeout":         {`"9s"`, `"901s"`, `10`, `"forever"`},
		"assistant_rate_per_minute": {`0`, `121`, `1.5`, `"30"`},
	} {
		for _, value := range values {
			if err := Validate(key, json.RawMessage(value)); err == nil {
				t.Errorf("%s accepted %s", key, value)
			}
		}
	}
	for key, value := range map[string]string{
		"assistant_timeout":         `"900s"`,
		"assistant_rate_per_minute": `1`,
	} {
		if err := Validate(key, json.RawMessage(value)); err != nil {
			t.Errorf("%s rejected boundary %s: %v", key, value, err)
		}
	}
}
