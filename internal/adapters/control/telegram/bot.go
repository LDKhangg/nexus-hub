package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/relay"
)

// MaxCallbackData is Telegram's limit for inline button callback data, in
// bytes.
const MaxCallbackData = 64

// Button is one inline keyboard button that sends callback data.
type Button struct {
	Text string
	Data string
}

// MessageOptions are the optional parts of a sendMessage call.
type MessageOptions struct {
	ThreadID *int64
	// Buttons is an inline keyboard, one slice per row.
	Buttons [][]Button
	// ReplyTo makes the message a reply to that message (0: none).
	ReplyTo int64
	// ForceReply asks the client to open a reply to this message.
	ForceReply bool
}

// WebhookInfo is the part of getWebhookInfo the CLI shows. The secret token
// is never returned by Telegram.
type WebhookInfo struct {
	URL                string   `json:"url"`
	PendingUpdateCount int64    `json:"pending_update_count"`
	LastErrorDate      int64    `json:"last_error_date,omitempty"`
	LastErrorMessage   string   `json:"last_error_message,omitempty"`
	MaxConnections     int64    `json:"max_connections,omitempty"`
	AllowedUpdates     []string `json:"allowed_updates,omitempty"`
}

type inlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

type replyMarkup struct {
	InlineKeyboard [][]inlineButton `json:"inline_keyboard,omitempty"`
	ForceReply     bool             `json:"force_reply,omitempty"`
}

var errBadButton = &relay.DeliveryError{Code: "invalid_button"}

func keyboard(rows [][]Button) ([][]inlineButton, error) {
	out := make([][]inlineButton, 0, len(rows))
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		r := make([]inlineButton, 0, len(row))
		for _, b := range row {
			if b.Text == "" || units(b.Text) > 64 || b.Data == "" || len(b.Data) > MaxCallbackData || !utf8.ValidString(b.Data) {
				return nil, errBadButton
			}
			r = append(r, inlineButton{Text: b.Text, CallbackData: b.Data})
		}
		out = append(out, r)
	}
	return out, nil
}

// token resolves the bot token; errors carry only a safe code.
func (c *Client) token(ctx context.Context, workspaceID, tokenRef string) (string, error) {
	if c.resolver == nil {
		return "", &relay.DeliveryError{Code: "missing_token"}
	}
	token, err := c.resolver.Resolve(ctx, workspaceID, tokenRef)
	if err != nil || token == "" {
		return "", &relay.DeliveryError{Code: secretCode(err)}
	}
	return token, nil
}

// SendMessage sends plain text (no parse mode) with optional inline buttons
// and returns the message ID.
func (c *Client) SendMessage(ctx context.Context, workspaceID, tokenRef, chatID string, text string, opts MessageOptions) (int64, error) {
	token, err := c.token(ctx, workspaceID, tokenRef)
	if err != nil {
		return 0, err
	}
	if units(text) > MaxTextUnits {
		text = clip(text, MaxTextUnits-12) + " [truncated]"
	}
	payload := map[string]any{"chat_id": chatID, "text": text}
	if opts.ThreadID != nil {
		payload["message_thread_id"] = *opts.ThreadID
	}
	if opts.ReplyTo > 0 {
		payload["reply_parameters"] = map[string]any{"message_id": opts.ReplyTo, "allow_sending_without_reply": true}
	}
	switch {
	case len(opts.Buttons) > 0:
		kb, err := keyboard(opts.Buttons)
		if err != nil {
			return 0, err
		}
		payload["reply_markup"] = replyMarkup{InlineKeyboard: kb}
	case opts.ForceReply:
		payload["reply_markup"] = replyMarkup{ForceReply: true}
	}
	var result struct {
		MessageID int64 `json:"message_id"`
	}
	if err := c.do(ctx, token, "sendMessage", payload, &result); err != nil {
		return 0, err
	}
	if result.MessageID <= 0 {
		return 0, &relay.DeliveryError{Code: "missing_receipt", Retryable: true}
	}
	return result.MessageID, nil
}

// EditReplyMarkup replaces a message's inline keyboard; nil buttons remove
// it.
func (c *Client) EditReplyMarkup(ctx context.Context, workspaceID, tokenRef, chatID string, messageID int64, buttons [][]Button) error {
	token, err := c.token(ctx, workspaceID, tokenRef)
	if err != nil {
		return err
	}
	kb, err := keyboard(buttons)
	if err != nil {
		return err
	}
	payload := map[string]any{"chat_id": chatID, "message_id": messageID, "reply_markup": map[string]any{"inline_keyboard": kb}}
	return c.do(ctx, token, "editMessageReplyMarkup", payload, nil)
}

// AnswerCallbackQuery stops the button's spinner and shows a short text.
func (c *Client) AnswerCallbackQuery(ctx context.Context, workspaceID, tokenRef, callbackID, text string) error {
	token, err := c.token(ctx, workspaceID, tokenRef)
	if err != nil {
		return err
	}
	if units(text) > 190 {
		text = clip(text, 190)
	}
	payload := map[string]any{"callback_query_id": callbackID}
	if text != "" {
		payload["text"] = text
	}
	return c.do(ctx, token, "answerCallbackQuery", payload, nil)
}

// SetWebhook registers the webhook URL with its secret token. The secret is
// sent only in the request body.
func (c *Client) SetWebhook(ctx context.Context, workspaceID, tokenRef, url, secretToken string, allowedUpdates []string) error {
	token, err := c.token(ctx, workspaceID, tokenRef)
	if err != nil {
		return err
	}
	payload := map[string]any{"url": url, "secret_token": secretToken, "allowed_updates": allowedUpdates}
	return c.do(ctx, token, "setWebhook", payload, nil)
}

// GetWebhookInfo returns the registered webhook state.
func (c *Client) GetWebhookInfo(ctx context.Context, workspaceID, tokenRef string) (WebhookInfo, error) {
	token, err := c.token(ctx, workspaceID, tokenRef)
	if err != nil {
		return WebhookInfo{}, err
	}
	var info WebhookInfo
	if err := c.do(ctx, token, "getWebhookInfo", map[string]any{}, &info); err != nil {
		return WebhookInfo{}, err
	}
	if units(info.LastErrorMessage) > 200 {
		info.LastErrorMessage = clip(info.LastErrorMessage, 200)
	}
	return info, nil
}

// DeleteWebhook removes the webhook; pending updates are kept.
func (c *Client) DeleteWebhook(ctx context.Context, workspaceID, tokenRef string) error {
	token, err := c.token(ctx, workspaceID, tokenRef)
	if err != nil {
		return err
	}
	return c.do(ctx, token, "deleteWebhook", map[string]any{"drop_pending_updates": false}, nil)
}

// do posts one Bot API method. Errors are *relay.DeliveryError with a safe
// code; the token and the provider's text never appear in them.
func (c *Client) do(ctx context.Context, token, method string, payload any, result any) error {
	fail := func(code string, retry bool, after time.Duration) error {
		return &relay.DeliveryError{Code: code, Retryable: retry, RetryAfter: after}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fail("invalid_request", false, 0)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/bot"+token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return fail("invalid_request", false, 0)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fail("transport", true, 0)
	}
	defer resp.Body.Close()
	var envelope struct {
		OK         bool            `json:"ok"`
		ErrorCode  int             `json:"error_code"`
		Result     json.RawMessage `json:"result"`
		Parameters struct {
			RetryAfter int64 `json:"retry_after"`
		} `json:"parameters"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&envelope)
	code := resp.StatusCode
	if envelope.ErrorCode != 0 {
		code = envelope.ErrorCode
	}
	switch {
	case code == 429:
		after := time.Duration(envelope.Parameters.RetryAfter) * time.Second
		if after < 0 {
			after = 0
		}
		return fail("rate_limited", true, after)
	case code >= 500:
		return fail("server", true, 0)
	case code < 200 || code >= 300:
		return fail("rejected", false, 0)
	case decodeErr != nil:
		return fail("invalid_response", true, 0)
	case !envelope.OK:
		return fail("rejected", false, 0)
	}
	if result != nil && len(envelope.Result) > 0 {
		if json.Unmarshal(envelope.Result, result) != nil {
			return fail("invalid_response", false, 0)
		}
	}
	return nil
}

// ErrorCode returns the safe code of a client error, or "unknown".
func ErrorCode(err error) string {
	var de *relay.DeliveryError
	if errors.As(err, &de) && de.Code != "" {
		return de.Code
	}
	return "unknown"
}

// chatIDString formats a numeric chat ID for the Bot API.
func chatIDString(id int64) string { return strconv.FormatInt(id, 10) }
