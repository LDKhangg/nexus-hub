package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
)

type Client struct {
	http      *http.Client
	lookupEnv func(string) (string, bool)
	baseURL   string
}

func NewClient(client *http.Client, lookupEnv func(string) (string, bool)) *Client {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	// Never follow redirects carrying the bot token to a different host.
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{http: &copyClient, lookupEnv: lookupEnv, baseURL: "https://api.telegram.org"}
}

func (c *Client) Send(ctx context.Context, target tenant.TelegramTargetConfig, e domain.NormalizedEvent) (relay.DeliveryReceipt, error) {
	fail := func(code string, retry bool, after time.Duration) (relay.DeliveryReceipt, error) {
		return relay.DeliveryReceipt{}, &relay.DeliveryError{Code: code, Retryable: retry, RetryAfter: after}
	}
	if c.lookupEnv == nil {
		return fail("missing_token", false, 0)
	}
	token, ok := c.lookupEnv(target.BotTokenEnv)
	if !ok || token == "" {
		return fail("missing_token", false, 0)
	}
	body, _ := json.Marshal(struct {
		ChatID string `json:"chat_id"`
		Text   string `json:"text"`
		Topic  *int64 `json:"message_thread_id,omitempty"`
	}{target.ChatID, FormatNotice(e), target.MessageThreadID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/bot"+token+"/sendMessage", bytes.NewReader(body))
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
		OK        bool `json:"ok"`
		ErrorCode int  `json:"error_code"`
		Result    struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
		Parameters struct {
			RetryAfter      int64 `json:"retry_after"`
			MigrateToChatID int64 `json:"migrate_to_chat_id"`
		} `json:"parameters"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&envelope)
	code := resp.StatusCode
	if envelope.ErrorCode != 0 {
		code = envelope.ErrorCode
	}
	if code == 429 {
		after := time.Duration(envelope.Parameters.RetryAfter) * time.Second
		if after < 0 {
			after = 0
		}
		return fail("rate_limited", true, after)
	}
	if code >= 500 {
		return fail("server", true, 0)
	}
	if envelope.Parameters.MigrateToChatID != 0 {
		return fail("chat_migrated", false, 0)
	}
	if code >= 400 || code < 200 || code >= 300 {
		return fail("rejected", false, 0)
	}
	if err != nil {
		return fail("invalid_response", true, 0)
	}
	if !envelope.OK {
		return fail("rejected", false, 0)
	}
	if envelope.Result.MessageID <= 0 {
		return fail("missing_receipt", true, 0)
	}
	return relay.DeliveryReceipt{MessageID: envelope.Result.MessageID}, nil
}
