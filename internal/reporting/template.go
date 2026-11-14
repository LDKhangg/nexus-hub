package reporting

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxTemplateChars = 1000
	maxTemplateLines = 12
)

var placeholderRE = regexp.MustCompile(`\{[^{}]*\}`)

var allowedPlaceholders = map[string]bool{
	"contact": true, "contact_platform": true,
	"pr_number": true, "pr_repo": true, "pr_title": true, "prs": true,
	"verdict": true, "confidence": true, "ai_summary": true, "items": true,
	"outcome": true, "reason": true,
	"task": true, "channel": true, "link": true,
	"behavior": true, "run_id": true,
}

// Values is the complete, fixed set of values an owner template may read.
// Deliberately absent fields (including triggering text and credentials) can
// never be reached by a template.
type Values struct {
	Contact         string
	ContactPlatform string
	PRNumber        string
	PRRepo          string
	PRTitle         string
	PRs             string
	Verdict         string
	Confidence      string
	AISummary       string
	// Items is the reported content of a notify, FYI, mention or digest
	// report: one item per line. It is distinct from the AI summary.
	Items    string
	Outcome  string
	Reason   string
	Task     string
	Channel  string
	Link     string
	Behavior string
	RunID    string
}

func (v Values) fields() map[string]string {
	return map[string]string{
		"contact": v.Contact, "contact_platform": v.ContactPlatform,
		"pr_number": v.PRNumber, "pr_repo": v.PRRepo, "pr_title": v.PRTitle, "prs": v.PRs,
		"verdict": v.Verdict, "confidence": v.Confidence, "ai_summary": v.AISummary, "items": v.Items,
		"outcome": v.Outcome, "reason": v.Reason,
		"task": v.Task, "channel": v.Channel, "link": v.Link,
		"behavior": v.Behavior, "run_id": v.RunID,
	}
}

// Validate rejects a template with one of the owner-facing reasons required
// by the product contract. Newlines delimit lines and are the sole permitted
// control character.
func Validate(body string) error {
	switch {
	case utf8.RuneCountInString(body) > maxTemplateChars:
		return errors.New("more than 1,000 characters")
	case strings.Count(body, "\n")+1 > maxTemplateLines:
		return errors.New("more than 12 lines")
	}
	for _, r := range body {
		if r != '\n' && (unicode.IsControl(r) || isBidiControl(r)) {
			return errors.New("control characters")
		}
	}
	for _, token := range placeholderRE.FindAllString(body, -1) {
		if !allowedPlaceholders[token[1:len(token)-1]] {
			return errors.New("unknown placeholder")
		}
	}
	// Braces that are not a complete named placeholder are not free-form
	// syntax and therefore count as an unknown placeholder too.
	stripped := placeholderRE.ReplaceAllString(body, "")
	if strings.ContainsAny(stripped, "{}") {
		return errors.New("unknown placeholder")
	}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "?{") {
			continue
		}
		end := strings.IndexByte(line, '}')
		if end < 3 || !allowedPlaceholders[line[2:end]] {
			return errors.New("unknown placeholder")
		}
	}
	return nil
}

func isBidiControl(r rune) bool {
	return (r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069') || r == '\u200e' || r == '\u200f'
}

// cleanValue makes a value safe for plain-text substitution. It preserves
// line breaks for list placeholders, removes controls and bidi formatting,
// and breaks @-mentions without changing their visible text.
func cleanValue(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteRune(r)
		case unicode.IsControl(r) || isBidiControl(r):
			b.WriteRune(' ')
		case r == '@':
			b.WriteString("@\u200b")
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// Render validates and renders one owner template in Vietnamese. See
// RenderLanguage.
func Render(body string, kind Kind, values Values) (string, error) {
	return RenderLanguage(body, kind, values, "vi")
}

// RenderLanguage validates and renders one owner template. Optional lines
// begin with ?{name}; the marker is removed, and the whole line is omitted
// when name is empty. Each line is scanned once and every {name} token is
// replaced by its cleaned value; inserted values are never rescanned.
// Failure, skipped-approval and verdict-unavailable reports cannot suppress
// their reason: when no kept line substituted {reason}, a reason line is
// appended. Another value that happens to contain the reason text does not
// count.
func RenderLanguage(body string, kind Kind, values Values, language string) (string, error) {
	if err := Validate(body); err != nil {
		return "", err
	}
	fields := values.fields()
	for name, value := range fields {
		fields[name] = cleanValue(value)
	}
	lines := make([]string, 0, strings.Count(body, "\n")+1)
	reasonShown := false
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "?{") {
			end := strings.IndexByte(line, '}')
			name := line[2:end]
			if fields[name] == "" {
				continue
			}
			line = line[end+1:]
		}
		line = placeholderRE.ReplaceAllStringFunc(line, func(token string) string {
			name := token[1 : len(token)-1]
			if name == "reason" && fields[name] != "" {
				reasonShown = true
			}
			return fields[name]
		})
		lines = append(lines, strings.TrimRight(line, " \t"))
	}
	out := strings.TrimSpace(strings.Join(lines, "\n"))
	if reason := fields["reason"]; reason != "" && needsReason(kind) && !reasonShown {
		if out != "" {
			out += "\n"
		}
		out += reasonPrefix(language) + reason
	}
	return out, nil
}

func needsReason(kind Kind) bool {
	return kind == KindFailure || kind == KindPRSkipped || kind == KindVerdictUnavailable
}

func reasonPrefix(language string) string {
	if strings.HasPrefix(language, "en") {
		return "Reason: "
	}
	return "Lý do: "
}

// Preview renders a template with fake, non-sensitive values. With no body,
// it previews the built-in shape for kind; P4b may pass the owner's body as
// the optional second argument.
func Preview(kind Kind, body ...string) (string, error) {
	template := "{outcome}\n?{verdict}AI: {verdict} ({confidence})\n?{link}{link}"
	if needsReason(kind) {
		template += "\n{reason}"
	}
	if len(body) > 0 {
		template = body[0]
	}
	return Render(template, kind, Values{
		Contact: "Sample Contact", ContactPlatform: "clickup", PRNumber: "1601", PRRepo: "org/repo",
		PRTitle: "Sample pull request", PRs: "org/repo#1601", Verdict: "approve_request", Confidence: "0.94",
		AISummary: "Sample summary", Items: "• Sample item", Outcome: "✅ Approved PR #1601 for Sample Contact", Reason: "Sample reason",
		Task: "TASK-123", Channel: "project-chat", Link: "https://github.com/org/repo/pull/1601",
		Behavior: "sample-behavior", RunID: "42",
	})
}
