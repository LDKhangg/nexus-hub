package agent

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

const redactedMarker = "[REDACTED]"

// minExactSecretLen is the shortest exact value the redactor will track;
// shorter values would cause false-positive redactions everywhere.
const minExactSecretLen = 6

// CleanInput removes control characters (except newline and tab),
// zero-width and bidi control characters, and collapses runs of blank
// lines. It is applied to every untrusted string before it reaches the LLM.
func CleanInput(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if dropRune(r) {
			continue
		}
		b.WriteRune(r)
	}
	return blankLines.ReplaceAllString(b.String(), "\n\n")
}

var blankLines = regexp.MustCompile(`\n(?:[ \t]*\n){2,}`)

func dropRune(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case r < 0x20 || r == 0x7f:
		return true
	case r >= 0x80 && r <= 0x9f:
		return true
	case r >= 0x200b && r <= 0x200d, r == 0x2060, r == 0xfeff:
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	case r == 0xfffd:
		// Invalid UTF-8 decodes to U+FFFD; drop it rather than forward garbage.
		return true
	}
	return false
}

type redactPattern struct {
	re   *regexp.Regexp
	repl string
}

var secretPatterns = []redactPattern{
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), redactedMarker},
	{regexp.MustCompile(`(?i)(authorization:[ \t]*)[^\r\n]+`), "${1}" + redactedMarker},
	{regexp.MustCompile(`x-access-token:[^@\s]+@`), "x-access-token:" + redactedMarker + "@"},
	{regexp.MustCompile(`(postgres(?:ql)?://[^:/@\s]+:)[^@\s]+@`), "${1}" + redactedMarker + "@"},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`), redactedMarker},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`), redactedMarker},
	{regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{10,}`), redactedMarker},
	{regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), redactedMarker},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), redactedMarker},
	{regexp.MustCompile(`\b\d{6,12}:[A-Za-z0-9_-]{30,}`), redactedMarker},
}

// Redactor removes known secret values and secret-shaped patterns.
// It is safe for concurrent use.
type Redactor struct {
	mu     sync.RWMutex
	values []string // sorted longest first
}

// NewRedactor seeds a redactor with exact secret values. Values shorter
// than six characters are ignored.
func NewRedactor(values []string) *Redactor {
	r := &Redactor{}
	for _, v := range values {
		r.Add(v)
	}
	return r
}

// Add registers an exact secret value.
func (r *Redactor) Add(v string) {
	if len(v) < minExactSecretLen {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.values {
		if existing == v {
			return
		}
	}
	r.values = append(r.values, v)
	sort.SliceStable(r.values, func(i, j int) bool { return len(r.values[i]) > len(r.values[j]) })
}

// RedactSecrets replaces exact registered values, then secret patterns.
func (r *Redactor) RedactSecrets(s string) string {
	r.mu.RLock()
	for _, v := range r.values {
		s = strings.ReplaceAll(s, v, redactedMarker)
	}
	r.mu.RUnlock()
	for _, p := range secretPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

var mentionRe = regexp.MustCompile(`(^|[^A-Za-z0-9_.@` + "`" + `-])@([A-Za-z0-9](?:[A-Za-z0-9-]{0,38}))\b`)

// CleanOutbound prepares text that leaves Nexus-Hub (Telegram, GitHub
// comments, commit messages): CleanInput plus RedactSecrets, optionally
// wrapping @login mentions in backticks so nobody is pinged.
func (r *Redactor) CleanOutbound(s string, neutralizeMentions bool) string {
	s = r.RedactSecrets(CleanInput(s))
	if neutralizeMentions {
		s = mentionRe.ReplaceAllString(s, "${1}`@${2}`")
	}
	return s
}
