package clickup

import (
	"encoding/json"
	"strconv"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

type ClickUpWebhookPayload struct {
	Event  string `json:"event"`
	TaskID string `json:"task_id"`
	Task   struct {
		Name string `json:"name"`
	} `json:"task"`
	History []struct {
		Field   string `json:"field"`
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
type ClickUpParser struct{}

func NewClickUpParser() *ClickUpParser {
	return &ClickUpParser{}
}

func (h *ClickUpParser) Parser(rawBody []byte) (normalizedEvent domain.NormalizedEvent, err error) {
	var payload ClickUpWebhookPayload
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return domain.NormalizedEvent{}, err
	}

	var content, senderID, senderName string
	for _, item := range payload.History {
		if item.Field == "comment" {
			content = item.Comment.TextContent
			senderName = item.Comment.User.Username
			senderID = strconv.Itoa(item.Comment.User.ID)
			break
		}
	}
	event := domain.NormalizedEvent{
		WorkspaceID:   "default_workspace",
		IntegrationID: "clickup_default",
		Source:        domain.SourceClickup,
		ExternalID:    payload.TaskID,
		Title:         payload.Task.Name,
		SenderID:      senderID,
		SenderName:    senderName,
		Content:       content,
		Metadata: map[string]string{
			"event_type": payload.Event,
		},
	}
	return event, nil
}
