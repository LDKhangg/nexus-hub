package memory

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Rune budgets for the sanitized context packet.
const (
	SummaryBudget   = 800
	MessageBudget   = 300
	BatchBudget     = 1200
	RecentCount     = 3
	TruncatedMarker = " [truncated]"
)

// ContextPacket is the sanitized, budgeted view of a thread given to the LLM.
type ContextPacket struct {
	Title, TaskURL, Summary string
	Recent, NewBatch        []Message
}

// BuildPacket selects the new external_user batch (messages after
// lastTriagedID), the last RecentCount earlier messages, and applies clean
// and the rune budgets to every part. history must be ordered by ID.
func BuildPacket(title, taskURL, summary string, history []Message, lastTriagedID int64, clean func(string) string) ContextPacket {
	p := ContextPacket{
		Title:   clean(title),
		TaskURL: clean(taskURL),
		Summary: truncateRunes(clean(summary), SummaryBudget),
	}
	var earlier, batch []Message
	for _, m := range history {
		m.Content = clean(m.Content)
		m.SenderName = clean(m.SenderName)
		switch {
		case m.ID <= lastTriagedID:
			earlier = append(earlier, m)
		case m.SenderRole == SenderRoleExternalUser:
			batch = append(batch, m)
		}
	}
	if len(earlier) > RecentCount {
		earlier = earlier[len(earlier)-RecentCount:]
	}
	for i := range earlier {
		earlier[i].Content = truncateRunes(earlier[i].Content, MessageBudget)
	}
	p.Recent = earlier
	p.NewBatch = budgetBatch(batch)
	return p
}

// budgetBatch keeps the newest messages that fit BatchBudget; the newest
// message is always kept, truncated if it alone exceeds the budget.
func budgetBatch(batch []Message) []Message {
	if len(batch) == 0 {
		return nil
	}
	remaining := BatchBudget
	start := len(batch)
	for i := len(batch) - 1; i >= 0; i-- {
		n := utf8.RuneCountInString(batch[i].Content)
		if i == len(batch)-1 && n > remaining {
			batch[i].Content = truncateRunes(batch[i].Content, remaining)
			start = i
			break
		}
		if n > remaining {
			break
		}
		remaining -= n
		start = i
	}
	return batch[start:]
}

// truncateRunes clips s to at most max runes including TruncatedMarker,
// never splitting a rune.
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	keep := max - utf8.RuneCountInString(TruncatedMarker)
	if keep < 0 {
		keep = 0
	}
	runes := []rune(s)
	return strings.TrimRight(string(runes[:keep]), " ") + TruncatedMarker
}

var closingTag = regexp.MustCompile(`(?i)</ticket_content>`)

func escapeTag(s string) string { return closingTag.ReplaceAllString(s, "</ticket_content_>") }

// Render formats the packet as one <ticket_content> block. Closing tags in
// untrusted content are escaped so content cannot end the block.
func (p ContextPacket) Render() string {
	var b strings.Builder
	b.WriteString("<ticket_content>\n")
	b.WriteString("Title: " + escapeTag(p.Title) + "\n")
	if p.TaskURL != "" {
		b.WriteString("URL: " + escapeTag(p.TaskURL) + "\n")
	}
	if p.Summary != "" {
		b.WriteString("Summary: " + escapeTag(p.Summary) + "\n")
	}
	if len(p.Recent) > 0 {
		b.WriteString("Earlier messages:\n")
		for _, m := range p.Recent {
			b.WriteString("- [" + string(m.SenderRole) + "] " + escapeTag(m.SenderName) + ": " + escapeTag(m.Content) + "\n")
		}
	}
	b.WriteString("New messages:\n")
	for _, m := range p.NewBatch {
		b.WriteString("- " + escapeTag(m.SenderName) + ": " + escapeTag(m.Content) + "\n")
	}
	b.WriteString("</ticket_content>\n")
	return b.String()
}
