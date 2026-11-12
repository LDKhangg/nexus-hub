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
	cfg, err := tenant.ParseTelegramTargetConfig(in.Config)
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
	return m.client.SendMessage(ctx, to.WorkspaceID, ref, chatIDString(to.ChatID), msg.Text, MessageOptions{
		Buttons: buttons(msg.Buttons), ReplyTo: msg.ReplyTo, ForceReply: msg.ForceReply,
	})
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
			out[i][j] = Button{Text: b.Text, Data: b.Data}
		}
	}
	return out
}
