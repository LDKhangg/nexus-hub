package reporting

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/LDKhangg/nexus-hub/internal/people"
)

// Report is the safe, structured input shared by every report path.
type Report struct {
	Kind        Kind
	UserID      string
	WorkspaceID string
	Behavior    string
	Language    string
	Platform    string
	ExternalID  string
	Values      Values
	Reaction    string
	TraceHint   string
}

// Renderer applies preferences, current contact names and owner templates.
type Renderer struct {
	store  Store
	people people.Resolver
}

func NewRenderer(store Store, resolver people.Resolver) *Renderer {
	return &Renderer{store: store, people: resolver}
}

func (r *Renderer) contact(ctx context.Context, report Report) string {
	if report.Values.Contact != "" {
		return report.Values.Contact
	}
	if r != nil && r.people != nil && report.Platform != "" && report.ExternalID != "" {
		if name, ok := r.people.DisplayName(ctx, report.WorkspaceID, report.Platform, report.ExternalID); ok && strings.TrimSpace(name) != "" {
			return name
		}
	}
	if report.Platform != "" && report.ExternalID != "" {
		return report.Platform + " " + report.ExternalID
	}
	return ""
}

// Render applies an owner template when present, otherwise the selected
// built-in style. It is the sole report text renderer used by behavior.
func (r *Renderer) Render(ctx context.Context, report Report) (string, error) {
	prefs := DefaultOwnerPrefs()
	if r != nil && r.store != nil {
		var err error
		prefs, err = r.store.Prefs(ctx, report.UserID, report.WorkspaceID, report.Behavior)
		if err != nil {
			return "", err
		}
	}
	if prefs.Style != StyleShort && prefs.Style != StyleNormal && prefs.Style != StyleDetailed {
		prefs.Style = StyleShort
	}
	language := prefs.Language
	if language == "" {
		language = report.Language
	}
	v := report.Values
	v.Contact = r.contact(ctx, report)
	if v.ContactPlatform == "" {
		v.ContactPlatform = report.Platform
	}
	if v.Behavior == "" {
		v.Behavior = report.Behavior
	}
	if v.Outcome == "" {
		v.Outcome = defaultOutcome(report.Kind, v, language)
	}
	if r != nil && r.store != nil {
		body, ok, err := r.store.Template(ctx, report.UserID, report.WorkspaceID, report.Behavior, report.Kind)
		if err != nil {
			return "", err
		}
		if ok {
			return RenderLanguage(body, report.Kind, v, language)
		}
	}
	return renderDefault(report, v, prefs, language), nil
}

func defaultOutcome(kind Kind, v Values, language string) string {
	en := strings.HasPrefix(language, "en")
	switch kind {
	case KindPRApprove:
		pr := v.PRNumber
		if pr != "" {
			pr = " #" + pr
		}
		if en {
			return strings.TrimSpace("✅ Approved PR" + pr + contactSuffix(v.Contact, "for"))
		}
		return strings.TrimSpace("✅ Approved PR" + pr + contactSuffix(v.Contact, "cho"))
	case KindPRSkipped:
		if en {
			return "⏭️ PR approval skipped" + contactSuffix(v.Contact, "for")
		}
		return "⏭️ Bỏ qua approve PR" + contactSuffix(v.Contact, "cho")
	case KindVerdictUnavailable:
		if en {
			return "⚠️ AI verdict unavailable" + contactSuffix(v.Contact, "for")
		}
		return "⚠️ AI không đưa ra được đánh giá" + contactSuffix(v.Contact, "cho")
	case KindFailure:
		if en {
			return "⚠️ Behavior run failed"
		}
		return "⚠️ Behavior chạy lỗi"
	case KindNotify, KindMention:
		if en {
			return "ℹ️ For your information" + contactSuffix(v.Contact, "from")
		}
		return "ℹ️ Thông tin" + contactSuffix(v.Contact, "từ")
	case KindFYI:
		if en {
			return "✅ Done on its own" + contactSuffix(v.Contact, "for")
		}
		return "✅ Đã tự xử lý" + contactSuffix(v.Contact, "cho")
	case KindDigest:
		if en {
			return "🗂 Summary"
		}
		return "🗂 Tổng hợp"
	}
	return v.Outcome
}

func contactSuffix(contact, word string) string {
	if strings.TrimSpace(contact) == "" {
		return ""
	}
	return " " + word + " " + contact
}

func renderDefault(report Report, v Values, prefs Prefs, language string) string {
	lines := []string{cleanValue(v.Outcome)}
	verdict := ""
	if v.Verdict != "" {
		verdict = "AI: " + cleanValue(v.Verdict)
		if v.Confidence != "" {
			verdict += " (" + cleanValue(v.Confidence) + ")"
		}
		if prefs.ShowAISummary && v.AISummary != "" && prefs.Style != StyleShort {
			verdict += " – " + cleanValue(v.AISummary)
		}
	}
	if verdict != "" {
		lines = append(lines, verdict)
	} else if prefs.ShowAISummary && v.AISummary != "" && prefs.Style != StyleShort {
		lines = append(lines, cleanValue(v.AISummary))
	}
	if v.Items != "" {
		if prefs.Style == StyleShort {
			lines = append(lines, compactItems(cleanValue(v.Items)))
		} else {
			lines = append(lines, strings.Split(cleanValue(v.Items), "\n")...)
		}
	}
	if prefs.Style != StyleShort {
		if prefs.ShowPRTitles && v.PRs != "" {
			lines = append(lines, strings.Split(cleanValue(v.PRs), "\n")...)
		}
		if prefs.ShowReaction && report.Reaction != "" {
			lines = append(lines, cleanValue(report.Reaction))
		}
		if prefs.Style == StyleDetailed {
			if v.ContactPlatform != "" {
				lines = append(lines, "Contact: "+cleanValue(v.ContactPlatform))
			}
			if v.Behavior != "" || v.RunID != "" {
				lines = append(lines, "Behavior: "+cleanValue(v.Behavior)+" · run "+cleanValue(v.RunID))
			}
			if prefs.ShowTraceHint && report.TraceHint != "" {
				lines = append(lines, cleanValue(report.TraceHint))
			}
		}
	}
	reason := ""
	if v.Reason != "" && needsReason(report.Kind) {
		reason = reasonPrefix(language) + cleanValue(v.Reason)
		lines = append(lines, reason)
	}
	if prefs.ShowLinks && v.Link != "" {
		lines = append(lines, cleanValue(v.Link))
	}
	lines = nonEmpty(lines)
	if prefs.Style == StyleShort && len(lines) > 4 {
		keep := lines[:4]
		if reason != "" && !containsLine(keep, reason) {
			keep = append(lines[:3:3], reason)
		}
		lines = keep
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

const maxCompactItemRunes = 200

// compactItems folds an item list into one line for the short style: the
// first items, then "+N" for the ones that do not fit in 200 runes. Bullet
// lines are the items when present; other lines are headings.
func compactItems(items string) string {
	var all, bullets []string
	for _, line := range strings.Split(items, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		all = append(all, line)
		if strings.HasPrefix(line, "•") {
			bullets = append(bullets, strings.TrimSpace(strings.TrimPrefix(line, "•")))
		}
	}
	if len(bullets) > 0 {
		all = bullets
	}
	out := ""
	for i, item := range all {
		next := item
		if out != "" {
			next = out + " · " + item
		}
		rest := ""
		if left := len(all) - i - 1; left > 0 {
			rest = " +" + strconv.Itoa(left)
		}
		if utf8.RuneCountInString(next+rest) > maxCompactItemRunes {
			if out == "" {
				return truncRunes(item, maxCompactItemRunes-utf8.RuneCountInString(rest)-1) + "…" + rest
			}
			return out + " +" + strconv.Itoa(len(all)-i)
		}
		out = next
	}
	return out
}

func truncRunes(s string, n int) string {
	if n < 0 {
		n = 0
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

func nonEmpty(lines []string) []string {
	out := lines[:0]
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// Confidence formats a report confidence consistently.
func Confidence(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

// PRLink builds a GitHub pull-request link from already validated parts.
func PRLink(repo string, number int) string {
	if repo == "" || number <= 0 {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)
}
