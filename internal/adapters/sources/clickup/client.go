package clickup

import (
	"encoding/json"

	"github.com/LDKhangg/nexus-hub/internal/relay"
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

func Parser(rawBody []byte) (normalizedEvent relay.NormalizedEvent, err error) {
	var payload ClickUpWebhookPayload
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return relay.NormalizedEvent{}, err
	}

	var event relay.NormalizedEvent{}
	return event,nil;
}
