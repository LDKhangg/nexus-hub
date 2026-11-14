package settings

import (
	"context"
	"encoding/json"
	"testing"
)

// TC-39 (REQ-12): telegram_assistant defaults to on, can be turned off from
// the CLI and refuses anything that is not a boolean.
func TestPhase4TelegramAssistantSetting(t *testing.T) {
	if !Defaults().TelegramAssistant {
		t.Fatal("telegram_assistant default is off")
	}
	raw, err := ParseCLIValue("telegram_assistant", "false")
	if err != nil || string(raw) != "false" {
		t.Fatalf("CLI false=%s err=%v", raw, err)
	}
	s, err := Load(context.Background(), mapStore{"telegram_assistant": raw})
	if err != nil || s.TelegramAssistant {
		t.Fatalf("stored false not applied: %+v err=%v", s.TelegramAssistant, err)
	}
	for _, bad := range []string{`"yes"`, `1`, `"false"`} {
		if err := Validate("telegram_assistant", json.RawMessage(bad)); err == nil {
			t.Fatalf("telegram_assistant accepted %s", bad)
		}
	}
	if _, err := ParseCLIValue("telegram_assistant", "maybe"); err == nil {
		t.Fatal("CLI accepted a non-boolean")
	}
	if def, err := DefaultJSON("telegram_assistant"); err != nil || string(def) != "true" {
		t.Fatalf("default JSON=%s err=%v", def, err)
	}
}
