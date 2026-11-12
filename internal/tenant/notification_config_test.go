package tenant

import "testing"

func sourceConfig() map[string]any {
	return map[string]any{"clickup_team_id": "42", "webhook_id": "w1", "webhook_secret_env": "NEXUS_CLICKUP_WEBHOOK_SECRET", "target_integration_id": "019a6690-abcd-7000-8000-000000000012"}
}
func TestNoticeConfig(t *testing.T) {
	c, err := ParseClickUpNoticeConfig(sourceConfig())
	if err != nil || c.WebhookID != "w1" {
		t.Fatalf("config %v %v", c, err)
	}
	for _, key := range []string{"clickup_team_id", "webhook_id", "webhook_secret_env", "target_integration_id"} {
		m := sourceConfig()
		delete(m, key)
		if _, err := ParseClickUpNoticeConfig(m); err == nil {
			t.Errorf("missing %s accepted", key)
		}
	}
	for _, m := range []map[string]any{{"chat_id": "-1001234567890", "bot_token_env": "BOT_TOKEN"}, {"chat_id": "-1001234567890", "bot_token_env": "BOT_TOKEN", "message_thread_id": float64(32)}} {
		c, err := ParseTelegramTargetConfig(m)
		if err != nil || c.ChatID != "-1001234567890" {
			t.Fatalf("target %v %v", c, err)
		}
	}
	for _, m := range []map[string]any{{"chat_id": float64(-12), "bot_token_env": "BOT_TOKEN"}, {"chat_id": "0", "bot_token_env": "BOT_TOKEN"}, {"chat_id": "9223372036854775808", "bot_token_env": "BOT_TOKEN"}, {"chat_id": "-123", "bot_token_env": ""}, {"chat_id": "-123", "bot_token_env": "BOT_TOKEN", "message_thread_id": -1}, {"chat_id": "-123", "bot_token_env": "BOT_TOKEN", "message_thread_id": 1.5}} {
		if _, err := ParseTelegramTargetConfig(m); err == nil {
			t.Fatalf("invalid target accepted: %v", m)
		}
	}
}
func TestNoticeRouteIsolation(t *testing.T) {
	s := WebhookIntegration{ID: "source", WorkspaceID: "workspace", Platform: "clickup", Role: IntegrationRoleSource, IsEnabled: true}
	target := WebhookIntegration{ID: "target", WorkspaceID: "workspace", Platform: "telegram", Role: IntegrationRoleControl, IsEnabled: true}
	if ValidateClickUpSource(s) != nil || ValidateTelegramTarget(s, target) != nil {
		t.Fatal("valid route rejected")
	}
	for _, mutation := range []func(*WebhookIntegration){func(v *WebhookIntegration) { v.IsEnabled = false }, func(v *WebhookIntegration) { v.Platform = "github" }, func(v *WebhookIntegration) { v.Role = IntegrationRoleControl }} {
		v := s
		mutation(&v)
		if ValidateClickUpSource(v) == nil {
			t.Fatal("invalid source accepted")
		}
	}
	for _, mutation := range []func(*WebhookIntegration){func(v *WebhookIntegration) { v.IsEnabled = false }, func(v *WebhookIntegration) { v.Platform = "clickup" }, func(v *WebhookIntegration) { v.Role = IntegrationRoleSource }, func(v *WebhookIntegration) { v.WorkspaceID = "other" }} {
		v := target
		mutation(&v)
		if ValidateTelegramTarget(s, v) == nil {
			t.Fatal("invalid target accepted")
		}
	}
}

func TestNotificationConfig_RefOrLegacyEnv(t *testing.T) {
	target := func(extra map[string]any) map[string]any {
		m := map[string]any{"chat_id": "-100123"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	c, err := ParseTelegramTargetConfig(target(map[string]any{"bot_token_ref": "telegram"}))
	if err != nil || c.BotTokenRef != "telegram" {
		t.Fatalf("ref: %+v %v", c, err)
	}
	c, err = ParseTelegramTargetConfig(target(map[string]any{"bot_token_env": "BOT_TOKEN"}))
	if err != nil || c.BotTokenRef != "env:BOT_TOKEN" {
		t.Fatalf("legacy: %+v %v", c, err)
	}
	c, err = ParseTelegramTargetConfig(target(map[string]any{"bot_token_ref": "telegram", "bot_token_env": "BOT_TOKEN"}))
	if err != nil || c.BotTokenRef != "telegram" {
		t.Fatalf("both: %+v %v", c, err)
	}
	for _, bad := range []map[string]any{{"bot_token_ref": "Bad Ref"}, {"bot_token_ref": "env:X"}, {}} {
		if _, err := ParseTelegramTargetConfig(target(bad)); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	src := sourceConfig()
	delete(src, "webhook_secret_env")
	src["webhook_secret_ref"] = "clickup-webhook"
	s, err := ParseClickUpNoticeConfig(src)
	if err != nil || s.WebhookSecretRef != "clickup-webhook" {
		t.Fatalf("source ref: %+v %v", s, err)
	}
	s, err = ParseClickUpNoticeConfig(sourceConfig())
	if err != nil || s.WebhookSecretRef != "env:NEXUS_CLICKUP_WEBHOOK_SECRET" {
		t.Fatalf("source legacy: %+v %v", s, err)
	}
}
