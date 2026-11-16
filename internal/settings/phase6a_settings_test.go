package settings

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

// TC-6A01 (REQ-1): the new assistant settings have the planned defaults,
// assistant_model now defaults to Sonnet, and all of them are listed.
func TestPhase6aAssistantSettingsDefaultsAndListing(t *testing.T) {
	s, err := Load(context.Background(), mapStore{})
	if err != nil {
		t.Fatal(err)
	}
	if s.AssistantModel != "claude-sonnet-5-5" || s.AssistantFastModel != "claude-haiku-5-5" || s.AssistantDeepModel != "claude-opus-5-5" || s.AssistantHistoryTurns != 6 {
		t.Fatalf("defaults: model=%q fast=%q deep=%q turns=%d", s.AssistantModel, s.AssistantFastModel, s.AssistantDeepModel, s.AssistantHistoryTurns)
	}
	keys := Keys()
	want := map[string]string{
		"assistant_model":         `"claude-sonnet-5-5"`,
		"assistant_fast_model":    `"claude-haiku-5-5"`,
		"assistant_deep_model":    `"claude-opus-5-5"`,
		"assistant_history_turns": `6`,
	}
	for key, def := range want {
		if !slices.Contains(keys, key) {
			t.Errorf("Keys() (settings list) misses %s", key)
		}
		got, err := DefaultJSON(key)
		if err != nil || string(got) != def {
			t.Errorf("DefaultJSON(%s)=%s err=%v, want %s", key, got, err, def)
		}
	}
}

// TC-6A02 (REQ-1): stored values override the defaults independently.
func TestPhase6aAssistantSettingsOverride(t *testing.T) {
	s, err := Load(context.Background(), mapStore{
		"assistant_fast_model":    json.RawMessage(`"claude-sonnet-5-5"`),
		"assistant_deep_model":    json.RawMessage(`"claude-sonnet-4-5-20250929"`),
		"assistant_history_turns": json.RawMessage(`20`),
	})
	if err != nil || s.AssistantFastModel != "claude-sonnet-5-5" || s.AssistantDeepModel != "claude-sonnet-4-5-20250929" || s.AssistantHistoryTurns != 20 || s.AssistantModel != "claude-sonnet-5-5" {
		t.Fatalf("settings=%+v err=%v", s, err)
	}
}

// TC-6A03 (REQ-1): the fast and deep models take the same validation as
// assistant_model; assistant_history_turns is an integer in 1..20, also when
// typed on the CLI.
func TestPhase6aAssistantSettingsValidation(t *testing.T) {
	bad := map[string][]string{
		"assistant_fast_model":    {`""`, `"haiku"`, `"gpt-5"`, `"claude-haiku-5-5 extra"`, `12`, `"openai/gpt-5"`},
		"assistant_deep_model":    {`""`, `"opus"`, `"gpt-5"`, `"claude-opus-5-5\n"`, `true`},
		"assistant_history_turns": {`0`, `21`, `-1`, `1.5`, `"6"`, `null`},
	}
	for key, values := range bad {
		for _, v := range values {
			if err := Validate(key, json.RawMessage(v)); err == nil {
				t.Errorf("%s accepted %s", key, v)
			}
			if err := Validate("assistant_model", json.RawMessage(v)); err == nil && key != "assistant_history_turns" {
				t.Errorf("assistant_model accepted %s (fast/deep must match it)", v)
			}
		}
	}
	for key, v := range map[string]string{
		"assistant_fast_model":    `"claude-haiku-5-5"`,
		"assistant_deep_model":    `"claude-opus-5-5"`,
		"assistant_history_turns": `1`,
	} {
		if err := Validate(key, json.RawMessage(v)); err != nil {
			t.Errorf("%s rejected %s: %v", key, v, err)
		}
	}
	if err := Validate("assistant_history_turns", json.RawMessage(`20`)); err != nil {
		t.Errorf("history turns rejected boundary 20: %v", err)
	}
	if raw, err := ParseCLIValue("assistant_history_turns", "12"); err != nil || string(raw) != "12" {
		t.Errorf("cli 12: %s %v", raw, err)
	}
	for _, text := range []string{"0", "21", "sáu", ""} {
		if _, err := ParseCLIValue("assistant_history_turns", text); err == nil {
			t.Errorf("cli accepted history turns %q", text)
		}
	}
	if raw, err := ParseCLIValue("assistant_fast_model", "claude-haiku-5-5"); err != nil || string(raw) != `"claude-haiku-5-5"` {
		t.Errorf("cli fast model: %s %v", raw, err)
	}
	if _, err := Load(context.Background(), mapStore{"assistant_history_turns": json.RawMessage(`99`)}); err == nil {
		t.Error("Load accepted a stored out-of-range history turns value")
	}
}
