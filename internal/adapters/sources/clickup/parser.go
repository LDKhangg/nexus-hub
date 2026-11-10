package clickup

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

const (
	signatureHeader = "X-Signature"
	botMarker       = "🤖"
)

var ErrInvalidSignature = errors.New("clickup: invalid webhook signature")

type webhookUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}
type ClickUpWebhookPayload struct {
	Event     string `json:"event"`
	TaskID    string `json:"task_id"`
	WebhookID string `json:"webhook_id"`
	Task      struct {
		Name string `json:"name"`
	} `json:"task"`
	History []struct {
		ID     string      `json:"id"`
		Field  string      `json:"field"`
		Date   string      `json:"date"`
		User   webhookUser `json:"user"`
		Before *struct {
			Status string `json:"status"`
		} `json:"before"`
		After *struct {
			Status string `json:"status"`
		} `json:"after"`
		Comment struct {
			ID          string      `json:"id"`
			TextContent string      `json:"text_content"`
			User        webhookUser `json:"user"`
		} `json:"comment"`
	} `json:"history_items"`
}

type ClickUpParser struct{ webhookSecret, ownSenderID string }

func NewClickUpParser(secret, ownSenderID string) *ClickUpParser {
	return &ClickUpParser{secret, ownSenderID}
}

// Verify authenticates the exact request bytes; an absent secret always fails closed.
func (p *ClickUpParser) Verify(header http.Header, raw []byte) error {
	if p.webhookSecret == "" {
		return ErrInvalidSignature
	}
	got, err := hex.DecodeString(header.Get(signatureHeader))
	if err != nil || len(got) != sha256.Size {
		return ErrInvalidSignature
	}
	mac := hmac.New(sha256.New, []byte(p.webhookSecret))
	_, _ = mac.Write(raw)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return ErrInvalidSignature
	}
	return nil
}

// Parser preserves each source history item. Trusted tenant IDs are attached by delivery.
func (p *ClickUpParser) Parser(raw []byte) ([]domain.NormalizedEvent, error) {
	var payload ClickUpWebhookPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, errors.New("clickup: malformed JSON")
	}
	var kind domain.EventType
	var field string
	switch payload.Event {
	case "taskCommentPosted":
		kind, field = domain.EventCommandPosted, "comment"
	case "taskStatusUpdated":
		kind, field = domain.EventStatusChanged, "status"
	default:
		return nil, domain.ErrIgnoredEvent
	}
	if strings.TrimSpace(payload.TaskID) == "" || strings.TrimSpace(payload.WebhookID) == "" {
		return nil, errors.New("clickup: missing source identity")
	}
	events := make([]domain.NormalizedEvent, 0, len(payload.History))
	for _, item := range payload.History {
		if item.Field != field {
			continue
		}
		if item.ID == "" {
			return nil, errors.New("clickup: missing history identity")
		}
		actor, content, messageID := item.User, "", "status:"+item.ID
		if field == "comment" {
			if item.Comment.ID == "" {
				return nil, errors.New("clickup: missing comment identity")
			}
			actor, content, messageID = item.Comment.User, item.Comment.TextContent, "comment:"+item.Comment.ID
		} else {
			if item.After == nil || item.After.Status == "" {
				return nil, errors.New("clickup: missing new status")
			}
			before := "Unknown"
			if item.Before != nil && item.Before.Status != "" {
				before = item.Before.Status
			}
			content = before + " → " + item.After.Status
		}
		id := ""
		if actor.ID != 0 {
			id = strconv.FormatInt(actor.ID, 10)
		}
		name := actor.Username
		if name == "" {
			name = "Unknown user"
		}
		when := time.Now().UTC()
		if item.Date != "" {
			ms, err := strconv.ParseInt(item.Date, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("clickup: invalid source timestamp")
			}
			when = time.UnixMilli(ms).UTC()
		}
		events = append(events, domain.NormalizedEvent{Source: domain.SourceClickup, EventType: kind, ExternalID: payload.TaskID, EventKey: payload.WebhookID + ":" + item.ID, ExternalMessageID: messageID, Title: payload.Task.Name, SenderID: id, SenderName: name, Content: content, OccurredAt: when, FromOwnBot: p.ownSenderID != "" && id == p.ownSenderID && strings.HasPrefix(content, botMarker), Metadata: map[string]string{"event_type": payload.Event, "webhook_id": payload.WebhookID, "task_url": "https://app.clickup.com/t/" + url.PathEscape(payload.TaskID)}})
	}
	if len(events) == 0 {
		return nil, errors.New("clickup: missing matching history")
	}
	return events, nil
}
