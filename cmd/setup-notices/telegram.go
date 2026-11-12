package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/control/telegram"
	"github.com/LDKhangg/nexus-hub/internal/adapters/netguard"
	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/settings"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

const telegramUsage = `usage: telegram webhook set|status|delete --workspace-id <uuid> --integration-id <uuid>
  set     generate a new secret, store it encrypted and register
          <public_base_url>/webhook/telegram/<integration-id> with Telegram
  status  show what Telegram has registered
  delete  remove the webhook and its stored secret
The integration is the workspace's Telegram integration that holds the bot
token. The secret is never printed.`

// telegramHTTP is overridden in tests.
var telegramHTTP = func() *http.Client { return netguard.Client(15*time.Second, netguard.DefaultHosts) }

// telegramAllowedUpdates are the update types the hub reads.
var telegramAllowedUpdates = []string{"message", "callback_query"}

func telegramCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer, lookup func(string) (string, bool)) error {
	if len(args) < 2 || args[0] != "webhook" {
		return errors.New(telegramUsage)
	}
	action := args[1]
	f := commandFlags("telegram webhook " + action)
	ws := f.String("workspace-id", "", "workspace UUID")
	integrationID := f.String("integration-id", "", "Telegram integration UUID")
	if f.Parse(args[2:]) != nil || f.NArg() != 0 || !tenant.ValidUUID(*ws) || !tenant.ValidUUID(*integrationID) {
		return errors.New("telegram webhook commands require a valid --workspace-id and --integration-id\n" + telegramUsage)
	}
	switch action {
	case "set", "status", "delete":
	default:
		return errors.New(telegramUsage)
	}
	integrations, err := storage.NewIntegrationRepository(pool)
	if err != nil {
		return err
	}
	tokenRef, err := telegram.ControlTokenRef(ctx, integrations, *ws, *integrationID)
	if err != nil {
		return errors.New("Telegram control integration not found in this workspace (needs platform telegram, role control or both, enabled, and a bot token reference)")
	}
	resolver, store, err := openResolver(pool, lookup)
	if err != nil {
		return err
	}
	client := telegram.NewClient(telegramHTTP(), resolver)
	enc := json.NewEncoder(out)
	switch action {
	case "set":
		base, err := publicBaseURL(ctx, pool)
		if err != nil {
			return err
		}
		url := strings.TrimRight(base, "/") + "/webhook/telegram/" + *integrationID
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return errors.New("random source failed")
		}
		secret := base64.RawURLEncoding.EncodeToString(raw) // 43 of [A-Za-z0-9_-], as Telegram requires
		// Stored first: the server accepts the new secret as soon as Telegram
		// switches to it. A failed setWebhook is fixed by running set again.
		if err := resolver.SetSecret(ctx, *ws, behavior.CredentialTelegramWebhookSecret, behavior.CredentialKindTelegramWebhook, secret); err != nil {
			return errors.New("webhook secret not stored")
		}
		if err := client.SetWebhook(ctx, *ws, tokenRef, url, secret, telegramAllowedUpdates); err != nil {
			return errors.New("secret stored, but Telegram setWebhook failed (" + telegram.ErrorCode(err) + "); run set again")
		}
		return enc.Encode(map[string]any{
			"integration_id": *integrationID, "url": url, "allowed_updates": telegramAllowedUpdates, "status": "set",
			"note": "a running server may take up to 5 minutes to pick up the new secret",
		})
	case "status":
		info, err := client.GetWebhookInfo(ctx, *ws, tokenRef)
		if err != nil {
			return errors.New("Telegram getWebhookInfo failed (" + telegram.ErrorCode(err) + ")")
		}
		configured, err := hasCredential(ctx, store, *ws)
		if err != nil {
			return err
		}
		expected := ""
		if base, err := publicBaseURL(ctx, pool); err == nil {
			expected = strings.TrimRight(base, "/") + "/webhook/telegram/" + *integrationID
		}
		res := map[string]any{
			"integration_id": *integrationID, "url": info.URL, "expected_url": expected, "url_matches": expected != "" && info.URL == expected,
			"secret_stored": configured, "pending_update_count": info.PendingUpdateCount, "allowed_updates": info.AllowedUpdates,
		}
		if info.LastErrorDate > 0 {
			res["last_error_at"] = time.Unix(info.LastErrorDate, 0).UTC().Format(time.RFC3339)
			res["last_error_message"] = info.LastErrorMessage
		}
		return enc.Encode(res)
	default: // delete
		if err := client.DeleteWebhook(ctx, *ws, tokenRef); err != nil {
			return errors.New("Telegram deleteWebhook failed (" + telegram.ErrorCode(err) + ")")
		}
		secretDeleted := store.Delete(ctx, *ws, behavior.CredentialTelegramWebhookSecret) == nil
		return enc.Encode(map[string]any{"integration_id": *integrationID, "status": "deleted", "secret_deleted": secretDeleted})
	}
}

// publicBaseURL reads the validated public_base_url setting.
func publicBaseURL(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	ss, err := storage.NewSettingsStore(pool)
	if err != nil {
		return "", err
	}
	s, err := settings.Load(ctx, ss)
	if err != nil {
		return "", errors.New("settings are invalid; fix them with the settings command")
	}
	if s.PublicBaseURL == "" {
		return "", errors.New("public_base_url is not set; run: settings set public_base_url https://<host>")
	}
	return s.PublicBaseURL, nil
}

func hasCredential(ctx context.Context, store *storage.CredentialStore, ws string) (bool, error) {
	metas, err := store.List(ctx, ws)
	if err != nil {
		return false, errors.New("secret list failed")
	}
	for _, m := range metas {
		if m.Name == behavior.CredentialTelegramWebhookSecret {
			return true, nil
		}
	}
	return false, nil
}
