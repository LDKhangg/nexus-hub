package settings

import (
	"context"
	"encoding/json"
	"testing"
)

// TC-P7-20: telegram_mode defaults to auto, accepts only auto, webhook and
// poll, and is applied when stored.
func TestPhase4P7TelegramModeSetting(t *testing.T) {
	if Defaults().TelegramMode != TelegramModeAuto {
		t.Fatalf("default telegram_mode=%q", Defaults().TelegramMode)
	}
	if def, err := DefaultJSON("telegram_mode"); err != nil || string(def) != `"auto"` {
		t.Fatalf("default JSON=%s err=%v", def, err)
	}
	for _, mode := range []string{"auto", "webhook", "poll"} {
		raw, err := ParseCLIValue("telegram_mode", mode)
		if err != nil {
			t.Fatalf("CLI %q: %v", mode, err)
		}
		if err := Validate("telegram_mode", raw); err != nil {
			t.Fatalf("valid %s refused: %v", raw, err)
		}
		s, err := Load(context.Background(), mapStore{"telegram_mode": raw})
		if err != nil || s.TelegramMode != mode {
			t.Fatalf("stored %q not applied: %q err=%v", mode, s.TelegramMode, err)
		}
	}
	for _, bad := range []string{`"POLL"`, `"polling"`, `""`, `" poll"`, `1`, `true`, `null`, `["poll"]`} {
		if err := Validate("telegram_mode", json.RawMessage(bad)); err == nil {
			t.Fatalf("telegram_mode accepted %s", bad)
		}
	}
	if _, err := ParseCLIValue("telegram_mode", "longpoll"); err == nil {
		t.Fatal("CLI accepted an unknown mode")
	}
}

// TC-P7-21: TelegramPolling is true for poll, false for webhook, and for
// auto only when public_base_url is empty.
func TestPhase4P7TelegramPolling(t *testing.T) {
	for _, tc := range []struct {
		mode, base string
		want       bool
	}{
		{TelegramModeAuto, "", true},
		{TelegramModeAuto, "https://hub.example.com", false},
		{TelegramModeWebhook, "", false},
		{TelegramModeWebhook, "https://hub.example.com", false},
		{TelegramModePoll, "", true},
		{TelegramModePoll, "https://hub.example.com", true},
	} {
		s := Defaults()
		s.TelegramMode, s.PublicBaseURL = tc.mode, tc.base
		if got := s.TelegramPolling(); got != tc.want {
			t.Errorf("mode=%s base=%q polling=%v want %v", tc.mode, tc.base, got, tc.want)
		}
	}
	if !Defaults().TelegramPolling() {
		t.Fatal("defaults (auto, no public URL) do not poll")
	}
	// Loaded from storage, auto with a public URL uses the webhook.
	base, err := ParseCLIValue("public_base_url", "https://hub.example.com")
	if err != nil {
		t.Fatalf("public_base_url: %v", err)
	}
	s, err := Load(context.Background(), mapStore{"public_base_url": base})
	if err != nil || s.TelegramPolling() {
		t.Fatalf("auto+public URL polls: %+v err=%v", s.TelegramMode, err)
	}
}
