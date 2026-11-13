package reporting

import (
	"context"
	"fmt"
	"strconv"
	"strings"

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
			return Render(body, report.Kind, v)
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
	if v.Reason != "" && (report.Kind == KindFailure || report.Kind == KindPRSkipped || report.Kind == KindVerdictUnavailable) {
		prefix := "Lý do: "
		if strings.HasPrefix(language, "en") {
			prefix = "Reason: "
		}
		lines = append(lines, prefix+cleanValue(v.Reason))
	}
	if prefs.ShowLinks && v.Link != "" {
		lines = append(lines, cleanValue(v.Link))
	}
	if prefs.Style == StyleShort && len(lines) > 4 {
		lines = lines[:4]
	}
	return strings.TrimSpace(strings.Join(nonEmpty(lines), "\n"))
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
