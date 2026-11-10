package telegram

import (
	"fmt"
	"strings"
	"unicode/utf16"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

func clip(s string, budget int) string {
	var b strings.Builder
	for _, r := range s {
		n := utf16.RuneLen(r)
		if budget < n {
			break
		}
		b.WriteRune(r)
		budget -= n
	}
	return b.String()
}

// FormatNotice uses plain text: user content cannot introduce Telegram formatting.
func FormatNotice(e domain.NormalizedEvent) string {
	title := e.Title
	if title == "" {
		title = e.ExternalID
	}
	actor := e.SenderName
	if actor == "" {
		actor = "Unknown user"
	}
	kind := "Comment"
	if e.EventType == domain.EventStatusChanged {
		kind = "Status changed"
	}
	url := "https://app.clickup.com/t/" + e.ExternalID
	if e.Metadata["task_url"] != "" {
		url = e.Metadata["task_url"]
	}
	header := fmt.Sprintf("ClickUp · %s\n%s\nBy %s\n", kind, clip(title, 300), clip(actor, 200))
	footer := "\n" + clip(url, 500)
	budget := 4000 - len(utf16.Encode([]rune(header+footer)))
	body := e.Content
	if len(utf16.Encode([]rune(body))) > budget {
		body = clip(body, budget-12) + " [truncated]"
	}
	return header + body + footer
}
