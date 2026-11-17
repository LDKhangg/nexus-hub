package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/control/telegram"
	"github.com/LDKhangg/nexus-hub/internal/adapters/netguard"
	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	clickuptools "github.com/LDKhangg/nexus-hub/internal/adapters/tools/clickup"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/settings"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

const telegramUsage = `usage: telegram webhook set|status|delete --workspace-id <uuid> --integration-id <uuid>
       telegram topics --workspace-id <uuid>
       telegram topics rename --workspace-id <uuid>
       telegram send --workspace-id <uuid> [--topic general|<fixed name>]
       telegram group set|show --workspace-id <uuid> --integration-id <uuid> [--chat-id <id>]
  set     generate a new secret, store it encrypted and register
          <public_base_url>/webhook/telegram/<integration-id> with Telegram
  status  show what Telegram has registered
  delete  remove the webhook and its stored secret
The integration is the workspace's Telegram integration that holds the bot
token. The secret is never printed.
send    read a plain-text message from stdin (1-4000 characters, never argv)
        and send it to the workspace's owner chat: General, or the fixed-name
        topic given by --topic. Prints {"status":"sent","message_id":n}.
topics  list the workspace's forum topics: subject kind and key, thread id,
        state and times (topic and task names are never stored or shown)
        rename  rename every ClickUp task topic to "<custom id> · <title>" and
                post and pin the task link where it is missing; prints counts
                only (renamed, linked, failed); safe to run again
group   set  store the workspace's forum group chat id (owner_chat_id) on the
             Telegram control integration; use it again after Telegram turns
             the group into a supergroup (the chat id changes)
        show print the chat id the integration uses for the workspace`

// telegramHTTP is overridden in tests.
var telegramHTTP = func() *http.Client { return netguard.Client(15*time.Second, netguard.DefaultHosts) }

// telegramAllowedUpdates are the update types the hub reads.
var telegramAllowedUpdates = []string{"message", "callback_query"}

func telegramCommand(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer, lookup func(string) (string, bool)) error {
	if len(args) >= 1 {
		switch args[0] {
		case "topics":
			if len(args) >= 2 && args[1] == "rename" {
				return telegramTopicsRename(ctx, pool, args[2:], out, lookup)
			}
			return telegramTopics(ctx, pool, args[1:], out)
		case "group":
			return telegramGroup(ctx, pool, args[1:], out)
		case "send":
			return telegramSend(ctx, pool, args[1:], out, lookup)
		}
	}
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

// telegramTopics lists the workspace's topic bindings: identifiers, state
// and times only.
func telegramTopics(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	f := commandFlags("telegram topics")
	ws := f.String("workspace-id", "", "workspace UUID")
	if f.Parse(args) != nil || f.NArg() != 0 || !tenant.ValidUUID(*ws) {
		return errors.New("telegram topics requires a valid --workspace-id\n" + telegramUsage)
	}
	store, err := storage.NewTelegramTopicStore(pool)
	if err != nil {
		return err
	}
	topics, err := store.ListTopics(ctx, *ws)
	if err != nil {
		return errors.New("topics not listed")
	}
	rows := make([]map[string]any, 0, len(topics))
	for _, t := range topics {
		row := map[string]any{
			"subject_kind": t.SubjectKind, "subject_key": t.SubjectKey, "thread_id": t.ThreadID, "state": t.State,
			"integration_id": t.IntegrationID, "chat_id": t.ChatID,
			"created_at": t.CreatedAt.UTC().Format(time.RFC3339), "updated_at": t.UpdatedAt.UTC().Format(time.RFC3339),
		}
		if t.ThreadID <= 0 {
			row["state"] = "reserved"
		}
		if t.ClosedAt != nil {
			row["closed_at"] = t.ClosedAt.UTC().Format(time.RFC3339)
		}
		rows = append(rows, row)
	}
	return json.NewEncoder(out).Encode(map[string]any{"workspace_id": *ws, "count": len(rows), "topics": rows})
}

// telegramTopicsRename renames the workspace's ClickUp task topics to
// "<custom id> · <title>" and posts and pins the task link where it was
// never posted. Output is counts only: no names, titles or links.
func telegramTopicsRename(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer, lookup func(string) (string, bool)) error {
	f := commandFlags("telegram topics rename")
	ws := f.String("workspace-id", "", "workspace UUID")
	if f.Parse(args) != nil || f.NArg() != 0 || !tenant.ValidUUID(*ws) {
		return errors.New("telegram topics rename requires a valid --workspace-id\n" + telegramUsage)
	}
	store, err := storage.NewTelegramTopicStore(pool)
	if err != nil {
		return err
	}
	integrations, err := storage.NewIntegrationRepository(pool)
	if err != nil {
		return err
	}
	resolver, _, err := openResolver(pool, lookup)
	if err != nil {
		return err
	}
	messenger := telegram.NewMessenger(telegram.NewClient(telegramHTTP(), resolver), integrations)
	reader := clickuptools.NewTaskReader(telegramHTTP(), resolver)
	known := knownTools()
	router, err := behavior.NewTopicRouter(behavior.TopicRouterDeps{
		Store: store, Client: messenger,
		TaskInfo: func(ctx context.Context, workspaceID, _, taskID string) (behavior.TaskTopicInfo, error) {
			tokenRef, err := clickUpTokenRef(ctx, integrations, workspaceID, known)
			if err != nil {
				return behavior.TaskTopicInfo{}, err
			}
			st, err := reader.ReadTask(ctx, workspaceID, tokenRef, taskID)
			return behavior.TaskTopicInfo{Title: st.Name, CustomID: st.CustomID, URL: st.URL}, err
		},
	})
	if err != nil {
		return errors.New("topic router unavailable")
	}
	n, err := router.RefreshTaskTopics(ctx, *ws)
	if err != nil {
		return errors.New("topics not listed")
	}
	return json.NewEncoder(out).Encode(map[string]any{"workspace_id": *ws, "renamed": n.Renamed, "linked": n.Linked, "failed": n.Failed})
}

// clickUpTokenRef returns the ClickUp token reference of the workspace's
// first enabled ClickUp source, like the server's task reads.
func clickUpTokenRef(ctx context.Context, integrations *storage.IntegrationRepository, ws string, known map[string]tenant.Tier) (string, error) {
	list, err := integrations.List(ctx, ws, 100, 0)
	if err != nil {
		return "", errors.New("integrations not listed")
	}
	for _, in := range list {
		if in.IsEnabled && in.Platform == "clickup" && (in.Role == tenant.IntegrationRoleSource || in.Role == tenant.IntegrationRoleBoth) {
			cfg, err := tenant.ParseAgentConfig(in.Config, known)
			if err != nil {
				return "", errors.New("ClickUp source config is invalid")
			}
			return cfg.ClickUpTokenRef, nil
		}
	}
	return "", errors.New("no ClickUp source in workspace")
}

// ownerUserIDUnset reports whether owner_telegram_user_id is 0. When the
// setting cannot be read it returns false (no warning).
func ownerUserIDUnset(ctx context.Context, pool *pgxpool.Pool) bool {
	ss, err := storage.NewSettingsStore(pool)
	if err != nil {
		return false
	}
	s, err := settings.Load(ctx, ss)
	return err == nil && s.OwnerTelegramUserID == 0
}

// telegramGroup sets or shows the workspace's forum group chat id on its
// Telegram control integration.
func telegramGroup(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	if len(args) < 1 || (args[0] != "set" && args[0] != "show") {
		return errors.New(telegramUsage)
	}
	action := args[0]
	f := commandFlags("telegram group " + action)
	ws := f.String("workspace-id", "", "workspace UUID")
	integrationID := f.String("integration-id", "", "Telegram control integration UUID")
	chat := f.String("chat-id", "", "group chat id (a supergroup id looks like -100...)")
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || !tenant.ValidUUID(*ws) || !tenant.ValidUUID(*integrationID) {
		return errors.New("telegram group commands require a valid --workspace-id and --integration-id\n" + telegramUsage)
	}
	integrations, err := storage.NewIntegrationRepository(pool)
	if err != nil {
		return err
	}
	in, err := integrations.Get(ctx, *ws, *integrationID)
	if err != nil || !in.IsEnabled || in.Platform != "telegram" || (in.Role != tenant.IntegrationRoleControl && in.Role != tenant.IntegrationRoleBoth) {
		return errors.New("Telegram control integration not found in this workspace (needs platform telegram, role control or both, and enabled)")
	}
	if action == "show" {
		if *chat != "" {
			return errors.New(telegramUsage)
		}
		_, explicit := in.Config["owner_chat_id"]
		effective := tenant.TelegramOwnerChatID(in.Config)
		return json.NewEncoder(out).Encode(map[string]any{
			"integration_id": in.ID, "owner_chat_id_set": explicit && in.Config["owner_chat_id"] != nil, "chat_id": effective,
			"uses_global_owner_chat": effective == 0,
		})
	}
	id, perr := strconv.ParseInt(strings.TrimSpace(*chat), 10, 64)
	if perr != nil || id == 0 {
		return errors.New("telegram group set requires a non-zero numeric --chat-id\n" + telegramUsage)
	}
	conf := make(map[string]any, len(in.Config)+1)
	for k, v := range in.Config {
		conf[k] = v
	}
	conf["owner_chat_id"] = id
	if err := tenant.ValidateOwnerChatID(conf); err != nil {
		return errors.New("invalid --chat-id")
	}
	if _, err := integrations.Update(ctx, storage.UpdateIntegrationInput{ID: in.ID, WorkspaceID: *ws, Role: in.Role, Config: conf, IsEnabled: in.IsEnabled}); err != nil {
		return errors.New("group not stored")
	}
	result := map[string]any{
		"integration_id": in.ID, "chat_id": id, "status": "set",
		"note": "a running server may take a short while to use the new chat id",
	}
	if id < 0 && ownerUserIDUnset(ctx, pool) {
		result["warning"] = "owner_telegram_user_id is not set: messages from a group are ignored until it is set (nh settings set owner_telegram_user_id <your Telegram user id>)"
	}
	return json.NewEncoder(out).Encode(result)
}
