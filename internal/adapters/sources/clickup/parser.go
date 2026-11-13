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

// flexID accepts a JSON string or number and never fails: any other value
// (null, object, bool) reads as empty, so an unexpected shape cannot reject
// an otherwise valid callback.
type flexID string

func (f *flexID) UnmarshalJSON(b []byte) error {
	*f = ""
	var s string
	if json.Unmarshal(b, &s) == nil {
		*f = flexID(strings.TrimSpace(s))
		return nil
	}
	var n json.Number
	if json.Unmarshal(b, &n) == nil {
		*f = flexID(n.String())
	}
	return nil
}

// maxTaggedUsers caps the tagged user IDs read from one comment.
const maxTaggedUsers = 50

// commentPart is one part of a ClickUp comment body. Parts of type "tag"
// (and the rarer "user_mention") name a tagged user in user.id.
type commentPart struct {
	Type string `json:"type"`
	User struct {
		ID flexID `json:"id"`
	} `json:"user"`
}

// commentParts reads a comment body's parts and never fails: a body that
// is not an array of parts reads as no parts, so an unexpected shape
// cannot reject an otherwise valid callback.
type commentParts []commentPart

func (c *commentParts) UnmarshalJSON(b []byte) error {
	*c = nil
	var raw []json.RawMessage
	if json.Unmarshal(b, &raw) != nil {
		return nil
	}
	for _, r := range raw {
		var part commentPart
		if json.Unmarshal(r, &part) == nil {
			*c = append(*c, part)
		}
	}
	return nil
}

// taggedUserIDs returns the distinct user IDs tagged by the parts, in
// order, at most maxTaggedUsers. IDs that are not plain identifiers are
// dropped. The comment text is never read here.
func (c commentParts) taggedUserIDs() []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range c {
		if part.Type != "tag" && part.Type != "user_mention" {
			continue
		}
		id := string(part.User.ID)
		if !validUserID(id) || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) == maxTaggedUsers {
			break
		}
	}
	return out
}

func validUserID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// historyValue is a history item's before/after value: a status object for
// status changes, a user object for assignee changes.
type historyValue struct {
	Status string `json:"status"`
	ID     flexID `json:"id"`
}

type ClickUpWebhookPayload struct {
	Event     string `json:"event"`
	TaskID    string `json:"task_id"`
	WebhookID string `json:"webhook_id"`
	Task      struct {
		Name string `json:"name"`
		List struct {
			ID flexID `json:"id"`
		} `json:"list"`
	} `json:"task"`
	History []struct {
		ID       string        `json:"id"`
		Field    string        `json:"field"`
		Date     string        `json:"date"`
		ParentID flexID        `json:"parent_id"`
		User     webhookUser   `json:"user"`
		Before   *historyValue `json:"before"`
		After    *historyValue `json:"after"`
		Comment  struct {
			ID          string       `json:"id"`
			TextContent string       `json:"text_content"`
			Parts       commentParts `json:"comment"`
			User        webhookUser  `json:"user"`
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
	case "taskAssigneeUpdated":
		kind, field = domain.EventAssigneeChanged, "assignee_add"
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
		listID := string(payload.Task.List.ID)
		if listID == "" {
			listID = string(item.ParentID)
		}
		extra := map[string]string{}
		actor, content, messageID := item.User, "", "status:"+item.ID
		switch field {
		case "assignee_add":
			// Only additions start anything; an item without a user ID
			// is skipped (no notice path exists for assignee events).
			if item.After == nil || item.After.ID == "" {
				continue
			}
			messageID = "assignee:" + item.ID
			extra["assignee_added"] = string(item.After.ID)
		case "comment":
			if item.Comment.ID == "" {
				return nil, errors.New("clickup: missing comment identity")
			}
			actor, content, messageID = item.Comment.User, item.Comment.TextContent, "comment:"+item.Comment.ID
			if tagged := item.Comment.Parts.taggedUserIDs(); len(tagged) > 0 {
				extra["tagged_user_ids"] = strings.Join(tagged, ",")
			}
		default:
			if item.After == nil || item.After.Status == "" {
				return nil, errors.New("clickup: missing new status")
			}
			before := "Unknown"
			if item.Before != nil && item.Before.Status != "" {
				before = item.Before.Status
			}
			content = before + " → " + item.After.Status
			extra["status_to"] = item.After.Status
			if item.Before != nil && item.Before.Status != "" {
				extra["status_from"] = item.Before.Status
			}
		}
		if listID != "" {
			extra["list_id"] = listID
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
		metadata := map[string]string{"event_type": payload.Event, "webhook_id": payload.WebhookID, "task_url": "https://app.clickup.com/t/" + url.PathEscape(payload.TaskID)}
		for k, v := range extra {
			metadata[k] = v
		}
		events = append(events, domain.NormalizedEvent{Source: domain.SourceClickup, EventType: kind, ExternalID: payload.TaskID, EventKey: payload.WebhookID + ":" + item.ID, ExternalMessageID: messageID, Title: payload.Task.Name, SenderID: id, SenderName: name, Content: content, OccurredAt: when, FromOwnBot: p.ownSenderID != "" && id == p.ownSenderID && strings.HasPrefix(content, botMarker), Metadata: metadata})
	}
	if len(events) == 0 && kind == domain.EventAssigneeChanged {
		// Only removals, or additions without a user: nothing to do.
		return nil, domain.ErrIgnoredEvent
	}
	if len(events) == 0 {
		return nil, errors.New("clickup: missing matching history")
	}
	return events, nil
}
