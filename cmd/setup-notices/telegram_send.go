package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/adapters/control/telegram"
	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/settings"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxTelegramSendRunes bounds one message of "telegram send".
const maxTelegramSendRunes = 4000

// readSendText reads the message from stdin: 1-4000 runes, valid UTF-8.
// It is never echoed or logged.
func readSendText() (string, error) {
	b, err := io.ReadAll(io.LimitReader(stdin, 4*maxTelegramSendRunes+1))
	if err != nil {
		return "", errors.New("could not read the message from stdin")
	}
	text := strings.TrimRight(string(b), "\r\n")
	switch {
	case strings.TrimSpace(text) == "":
		return "", errors.New("empty message: pass the text on stdin")
	case !utf8.ValidString(text):
		return "", errors.New("the message is not valid UTF-8")
	case utf8.RuneCountInString(text) > maxTelegramSendRunes:
		return "", errors.New("message too long (limit 4000 characters)")
	}
	return text, nil
}

// ownerChatFor finds the workspace's owner chat like the server does: the
// control integration's own group wins, then owner_telegram_chat_id.
func ownerChatFor(ctx context.Context, pool *pgxpool.Pool, integrations *storage.IntegrationRepository, ws string) (behavior.ChatRef, error) {
	list, err := integrations.List(ctx, ws, 100, 0)
	if err != nil {
		return behavior.ChatRef{}, errors.New("integrations not listed")
	}
	var control []tenant.Integration
	for _, in := range list {
		if in.IsEnabled && in.Platform == "telegram" && (in.Role == tenant.IntegrationRoleControl || in.Role == tenant.IntegrationRoleBoth) {
			control = append(control, in)
		}
	}
	for _, in := range control {
		if chat := tenant.TelegramOwnerChatID(in.Config); chat != 0 {
			return behavior.ChatRef{WorkspaceID: ws, IntegrationID: in.ID, ChatID: chat}, nil
		}
	}
	if len(control) > 0 {
		if ss, err := storage.NewSettingsStore(pool); err == nil {
			if s, err := settings.Load(ctx, ss); err == nil && s.OwnerTelegramChatID != 0 {
				return behavior.ChatRef{WorkspaceID: ws, IntegrationID: control[0].ID, ChatID: s.OwnerTelegramChatID}, nil
			}
		}
	}
	return behavior.ChatRef{}, errors.New("no owner chat: the workspace has no Telegram control integration with a chat id")
}

// telegramSend sends one plain-text message (from stdin) to the workspace's
// owner chat, in General or in a fixed-name topic.
func telegramSend(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer, lookup func(string) (string, bool)) error {
	f := commandFlags("telegram send")
	ws := f.String("workspace-id", "", "workspace UUID")
	topic := f.String("topic", behavior.ReportTopicGeneral, "general or a fixed topic name")
	if f.Parse(args) != nil || f.NArg() != 0 || !tenant.ValidUUID(*ws) {
		return errors.New("telegram send requires a valid --workspace-id and takes no positional arguments (the text comes from stdin)\n" + telegramUsage)
	}
	name := strings.TrimSpace(*topic)
	if name == "" || len(name) > behavior.MaxReportTopicBytes || !utf8.ValidString(name) ||
		name == behavior.ReportTopicTask || name == behavior.ReportTopicWorkflow {
		return errors.New("--topic must be general or a fixed topic name of 1-64 bytes")
	}
	text, err := readSendText()
	if err != nil {
		return err
	}
	integrations, err := storage.NewIntegrationRepository(pool)
	if err != nil {
		return err
	}
	to, err := ownerChatFor(ctx, pool, integrations, *ws)
	if err != nil {
		return err
	}
	resolver, _, err := openResolver(pool, lookup)
	if err != nil {
		return err
	}
	messenger := telegram.NewMessenger(telegram.NewClient(telegramHTTP(), resolver), integrations)
	fellBack := false
	if name != behavior.ReportTopicGeneral {
		store, err := storage.NewTelegramTopicStore(pool)
		if err != nil {
			return err
		}
		router, err := behavior.NewTopicRouter(behavior.TopicRouterDeps{Store: store, Client: messenger})
		if err != nil {
			return errors.New("topic router unavailable")
		}
		to = router.Destination(ctx, to, behavior.TopicIntent{Mode: name})
		fellBack = to.ThreadID == 0
	}
	id, err := messenger.SendMessage(ctx, to, behavior.OutMessage{Text: text})
	if err != nil {
		return errors.New("Telegram send failed (" + telegram.ErrorCode(err) + ")")
	}
	res := map[string]any{"status": "sent", "message_id": id}
	if fellBack {
		res["topic"] = "fallback_general"
	}
	return json.NewEncoder(out).Encode(res)
}
