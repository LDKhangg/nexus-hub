package clickup

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

const (
	signatureHeader = "X-Signature"
	botMarker       = "\U0001F916"
)

var ErrInvalidSignature = errors.New("clickup: invalid webhook signature")

type ClickUpWebhookPayload struct {
	Event  string `json:"event"`
	TaskID string `json:"task_id"`
	Task   struct {
		Name string `json:"name"`
	} `json:"task"`
	History []struct {
		Field   string `json:"field"`
		Date    string `json:"date"`
		Comment struct {
			ID          string `json:"id"`
			TextContent string `json:"text_content"`
			User        struct {
				ID       int    `json:"id"`
				Username string `json:"username"`
			} `json:"user"`
		} `json:"comment"`
	} `json:"history_items"`
}

var eventTypes = map[string]domain.EventType{
	"taskCommentPosted": domain.EventCommandPosted,
	"taskStatusUpdated": domain.EventStatusChanged,
}

type ClickUpParser struct {
	webhookSecret string
	ownSenderID   string
}

// NewClickUpParser: secret rỗng = tắt verify chữ ký (chỉ dùng cho dev).
func NewClickUpParser(webhookSecret string, ownSenderID string) *ClickUpParser {
	return &ClickUpParser{
		webhookSecret: webhookSecret,
		ownSenderID:   ownSenderID,
	}
}

// Verify kiểm tra header X-Signature = hex(HMAC-SHA256(secret, body)).
func (p *ClickUpParser) Verify(header http.Header, rawBody []byte) error {
	if p.webhookSecret == "" {
		return nil
	}
	got, err := hex.DecodeString(header.Get(signatureHeader))
	if err != nil || len(got) == 0 {
		return ErrInvalidSignature
	}
	mac := hmac.New(sha256.New, []byte(p.webhookSecret))
	mac.Write(rawBody)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return ErrInvalidSignature
	}
	return nil
}

func (p *ClickUpParser) Parser(rawBody []byte) (domain.NormalizedEvent, error) {
	var payload ClickUpWebhookPayload
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return domain.NormalizedEvent{}, err
	}

	eventType, ok := eventTypes[payload.Event]
	if !ok {
		return domain.NormalizedEvent{}, domain.ErrIgnoredEvent
	}

	var content, senderID, senderName string
	occurredAt := time.Now()
	for _, item := range payload.History {
		if item.Field != "comment" {
			continue
		}
		content = item.Comment.TextContent
		senderName = item.Comment.User.Username
		senderID = strconv.Itoa(item.Comment.User.ID)
		if ms, err := strconv.ParseInt(item.Date, 10, 64); err == nil {
			occurredAt = time.UnixMilli(ms)
		}
		break
	}
	if content == "" {
		return domain.NormalizedEvent{}, domain.ErrIgnoredEvent
	}

	isMyOwnBot := senderID == p.ownSenderID && strings.HasPrefix(content, botMarker)

	return domain.NormalizedEvent{
		WorkspaceID:   "default_workspace",
		IntegrationID: "clickup_default",
		Source:        domain.SourceClickup,
		EventType:     eventType,
		ExternalID:    payload.TaskID,
		Title:         payload.Task.Name,
		SenderID:      senderID,
		SenderName:    senderName,
		Content:       content,
		Metadata:      map[string]string{"event_type": payload.Event},
		OccurredAt:    occurredAt,
		FromOwnBot:    isMyOwnBot,
	}, nil
}
