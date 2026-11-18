package telegram

import (
	"context"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

// IntegrationLookup finds an integration by ID (implemented by the storage
// integration repository).
type IntegrationLookup interface {
	LookupWebhookIntegration(ctx context.Context, id string) (tenant.WebhookIntegration, error)
}

// Messenger implements behavior.Messenger: it sends through the bot of the
// named Telegram control integration, to the given chat.
type Messenger struct {
	client       *Client
	integrations IntegrationLookup
}

func NewMessenger(client *Client, integrations IntegrationLookup) *Messenger {
	return &Messenger{client: client, integrations: integrations}
}

var _ behavior.Messenger = (*Messenger)(nil)
var _ behavior.MessageEditor = (*Messenger)(nil)
var _ behavior.MessageButtonEditor = (*Messenger)(nil)
var _ behavior.TopicLinkPoster = (*Messenger)(nil)
var _ behavior.TopicRenamer = (*Messenger)(nil)

// ControlTokenRef returns the bot token reference of an eligible Telegram
// control integration in the workspace.
func ControlTokenRef(ctx context.Context, lookup IntegrationLookup, workspaceID, integrationID string) (string, error) {
	if lookup == nil || !tenant.ValidUUID(integrationID) {
		return "", &relay.DeliveryError{Code: "invalid_integration"}
	}
	in, err := lookup.LookupWebhookIntegration(ctx, integrationID)
	if err != nil {
		return "", &relay.DeliveryError{Code: "integration_unavailable", Retryable: true}
	}
	if in.WorkspaceID != workspaceID || tenant.ValidateTelegramControl(in) != nil {
		return "", &relay.DeliveryError{Code: "invalid_integration"}
	}
	cfg, err := tenant.ParseTelegramTargetConfig(tenant.OwnerTargetConfig(in.Config))
	if err != nil {
		return "", &relay.DeliveryError{Code: "invalid_integration"}
	}
	return cfg.BotTokenRef, nil
}

func (m *Messenger) SendMessage(ctx context.Context, to behavior.ChatRef, msg behavior.OutMessage) (int64, error) {
	ref, err := ControlTokenRef(ctx, m.integrations, to.WorkspaceID, to.IntegrationID)
	if err != nil {
		return 0, err
	}
	opts := MessageOptions{Buttons: buttons(msg.Buttons), ReplyTo: msg.ReplyTo, ForceReply: msg.ForceReply}
	if to.ThreadID > 0 {
		thread := to.ThreadID
		opts.ThreadID = &thread
	}
	return m.client.SendMessage(ctx, to.WorkspaceID, ref, chatIDString(to.ChatID), msg.Text, opts)
}

// CreateTopic creates a forum topic in the chat and returns its thread ID.
func (m *Messenger) CreateTopic(ctx context.Context, to behavior.ChatRef, name string) (int64, error) {
	ref, err := ControlTokenRef(ctx, m.integrations, to.WorkspaceID, to.IntegrationID)
	if err != nil {
		return 0, err
	}
	return m.client.CreateForumTopic(ctx, to.WorkspaceID, ref, chatIDString(to.ChatID), name)
}

// RenameTopic renames a forum topic; changed is false when it already had
// that name.
func (m *Messenger) RenameTopic(ctx context.Context, to behavior.ChatRef, threadID int64, name string) (bool, error) {
	ref, err := ControlTokenRef(ctx, m.integrations, to.WorkspaceID, to.IntegrationID)
	if err != nil {
		return false, err
	}
	return m.client.EditForumTopic(ctx, to.WorkspaceID, ref, chatIDString(to.ChatID), threadID, name)
}

// PinMessage pins a message the bot sent, without a notification.
func (m *Messenger) PinMessage(ctx context.Context, to behavior.ChatRef, messageID int64) error {
	ref, err := ControlTokenRef(ctx, m.integrations, to.WorkspaceID, to.IntegrationID)
	if err != nil {
		return err
	}
	return m.client.PinChatMessage(ctx, to.WorkspaceID, ref, chatIDString(to.ChatID), messageID)
}

// CloseTopic closes a forum topic.
func (m *Messenger) CloseTopic(ctx context.Context, to behavior.ChatRef, threadID int64) error {
	ref, err := ControlTokenRef(ctx, m.integrations, to.WorkspaceID, to.IntegrationID)
	if err != nil {
		return err
	}
	return m.client.CloseForumTopic(ctx, to.WorkspaceID, ref, chatIDString(to.ChatID), threadID)
}

// DeleteTopic deletes a forum topic and its messages.
func (m *Messenger) DeleteTopic(ctx context.Context, to behavior.ChatRef, threadID int64) error {
	ref, err := ControlTokenRef(ctx, m.integrations, to.WorkspaceID, to.IntegrationID)
	if err != nil {
		return err
	}
	return m.client.DeleteForumTopic(ctx, to.WorkspaceID, ref, chatIDString(to.ChatID), threadID)
}

// ReopenTopic reopens a forum topic.
func (m *Messenger) ReopenTopic(ctx context.Context, to behavior.ChatRef, threadID int64) error {
	ref, err := ControlTokenRef(ctx, m.integrations, to.WorkspaceID, to.IntegrationID)
	if err != nil {
		return err
	}
	return m.client.ReopenForumTopic(ctx, to.WorkspaceID, ref, chatIDString(to.ChatID), threadID)
}

// EditMessageText replaces the text of a message the bot sent (the progress
// message of a local task).
func (m *Messenger) EditMessageText(ctx context.Context, to behavior.ChatRef, messageID int64, text string) error {
	ref, err := ControlTokenRef(ctx, m.integrations, to.WorkspaceID, to.IntegrationID)
	if err != nil {
		return err
	}
	return m.client.EditMessageText(ctx, to.WorkspaceID, ref, chatIDString(to.ChatID), messageID, text, nil)
}

// EditMessageWithButtons replaces a message's text and inline keyboard (a
// ClickUp draft preview); no buttons removes the keyboard.
func (m *Messenger) EditMessageWithButtons(ctx context.Context, to behavior.ChatRef, messageID int64, text string, rows [][]behavior.Button) error {
	ref, err := ControlTokenRef(ctx, m.integrations, to.WorkspaceID, to.IntegrationID)
	if err != nil {
		return err
	}
	return m.client.EditMessageText(ctx, to.WorkspaceID, ref, chatIDString(to.ChatID), messageID, text, buttons(rows))
}

// SetButtons replaces a message's inline keyboard and keeps its text.
func (m *Messenger) SetButtons(ctx context.Context, to behavior.ChatRef, messageID int64, rows [][]behavior.Button) error {
	ref, err := ControlTokenRef(ctx, m.integrations, to.WorkspaceID, to.IntegrationID)
	if err != nil {
		return err
	}
	return m.client.EditReplyMarkup(ctx, to.WorkspaceID, ref, chatIDString(to.ChatID), messageID, buttons(rows))
}

func (m *Messenger) RemoveButtons(ctx context.Context, to behavior.ChatRef, messageID int64) error {
	ref, err := ControlTokenRef(ctx, m.integrations, to.WorkspaceID, to.IntegrationID)
	if err != nil {
		return err
	}
	return m.client.EditReplyMarkup(ctx, to.WorkspaceID, ref, chatIDString(to.ChatID), messageID, nil)
}

func (m *Messenger) AnswerCallback(ctx context.Context, to behavior.ChatRef, callbackID, text string) error {
	ref, err := ControlTokenRef(ctx, m.integrations, to.WorkspaceID, to.IntegrationID)
	if err != nil {
		return err
	}
	return m.client.AnswerCallbackQuery(ctx, to.WorkspaceID, ref, callbackID, text)
}

func buttons(rows [][]behavior.Button) [][]Button {
	if len(rows) == 0 {
		return nil
	}
	out := make([][]Button, len(rows))
	for i, row := range rows {
		out[i] = make([]Button, len(row))
		for j, b := range row {
			out[i][j] = Button{Text: b.Text, Data: b.Data, URL: b.URL}
		}
	}
	return out
}
