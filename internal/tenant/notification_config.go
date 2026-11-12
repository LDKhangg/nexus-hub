package tenant

import (
	"encoding/hex"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// TelegramTargetConfig refers to credentials by reference, never their values.
// BotTokenRef names a credentials row, or "env:NAME" for a legacy
// bot_token_env reference that has not been migrated yet.
type TelegramTargetConfig struct {
	ChatID          string
	MessageThreadID *int64
	BotTokenRef     string
}

// ClickUpNoticeConfig: WebhookSecretRef follows the same convention.
type ClickUpNoticeConfig struct{ ClickUpTeamID, WebhookID, WebhookSecretRef, TargetIntegrationID, OwnSenderID string }

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// LegacyEnvRefPrefix marks a reference resolved from the process environment.
const LegacyEnvRefPrefix = "env:"

// credentialRef reads <base>_ref, falling back to the legacy <base>_env.
func credentialRef(m map[string]any, base string) (string, bool) {
	if ref := stringField(m, base+"_ref"); ref != "" {
		return ref, ValidCredentialRef(ref)
	}
	if env := stringField(m, base+"_env"); env != "" {
		return LegacyEnvRefPrefix + env, envName.MatchString(env)
	}
	return "", false
}

func ParseClickUpNoticeConfig(m map[string]any) (ClickUpNoticeConfig, error) {
	ref, refOK := credentialRef(m, "webhook_secret")
	c := ClickUpNoticeConfig{ClickUpTeamID: stringField(m, "clickup_team_id"), WebhookID: stringField(m, "webhook_id"), WebhookSecretRef: ref, TargetIntegrationID: stringField(m, "target_integration_id"), OwnSenderID: stringField(m, "own_sender_id")}
	if c.ClickUpTeamID == "" || c.WebhookID == "" || !refOK || !ValidUUID(c.TargetIntegrationID) {
		return c, errors.New("invalid ClickUp notice configuration")
	}
	if v, ok := m["own_sender_id"]; ok && v != nil {
		if _, ok := v.(string); !ok {
			return c, errors.New("invalid own sender ID")
		}
	}
	return c, nil
}
func ParseTelegramTargetConfig(m map[string]any) (TelegramTargetConfig, error) {
	ref, refOK := credentialRef(m, "bot_token")
	c := TelegramTargetConfig{ChatID: stringField(m, "chat_id"), BotTokenRef: ref}
	id, err := strconv.ParseInt(c.ChatID, 10, 64)
	if err != nil || id == 0 || !refOK {
		return c, errors.New("invalid Telegram target configuration")
	}
	if v, ok := m["message_thread_id"]; ok && v != nil {
		var n int64
		switch value := v.(type) {
		case int64:
			n = value
		case int:
			n = int64(value)
		case float64:
			if value >= float64(math.MaxInt64) || value != math.Trunc(value) {
				return c, errors.New("invalid topic ID")
			}
			n = int64(value)
		default:
			return c, errors.New("invalid topic ID")
		}
		if n <= 0 {
			return c, errors.New("invalid topic ID")
		}
		c.MessageThreadID = &n
	}
	return c, nil
}
func stringField(m map[string]any, key string) string {
	value, _ := m[key].(string)
	return strings.TrimSpace(value)
}

// ValidUUID checks canonical UUID syntax without importing a database driver.
func ValidUUID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	_, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	return err == nil
}
func ValidateClickUpSource(s WebhookIntegration) error {
	if !s.IsEnabled || s.Platform != "clickup" || (s.Role != IntegrationRoleSource && s.Role != IntegrationRoleBoth) {
		return errors.New("ineligible ClickUp source")
	}
	return nil
}
func ValidateTelegramTarget(source, target WebhookIntegration) error {
	if !target.IsEnabled || target.Platform != "telegram" || target.WorkspaceID != source.WorkspaceID || (target.Role != IntegrationRoleControl && target.Role != IntegrationRoleBoth) {
		return errors.New("ineligible Telegram target")
	}
	return nil
}
