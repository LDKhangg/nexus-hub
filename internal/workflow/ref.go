package workflow

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// FormatRef returns the owner-facing reference for an execution.
func FormatRef(id int64) string { return "WF-" + strconv.FormatInt(id, 10) }

// ParseRef parses an owner-facing workflow reference. Bare execution IDs are
// deliberately not accepted; callers that support them must opt in.
func ParseRef(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 4 || !strings.EqualFold(s[:3], "WF-") {
		return 0, false
	}
	id, err := strconv.ParseInt(s[3:], 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// FindRefs extracts at most ten distinct, positive, word-bounded workflow
// references in their first-seen order.
func FindRefs(text string) []int64 {
	var out []int64
	seen := make(map[int64]bool)
	for i := 0; i+3 < len(text) && len(out) < 10; i++ {
		if i > 0 {
			before, _ := lastRune(text[:i])
			if isRefWord(before) {
				continue
			}
		}
		if !strings.EqualFold(text[i:min(i+3, len(text))], "WF-") {
			continue
		}
		end := i + 3
		for end < len(text) && text[end] >= '0' && text[end] <= '9' {
			end++
		}
		after, _ := utf8.DecodeRuneInString(text[end:])
		if end == i+3 || end < len(text) && isRefWord(after) {
			continue
		}
		if id, ok := ParseRef(text[i:end]); ok && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
		i = end - 1
	}
	return out
}

func lastRune(s string) (rune, int) {
	r, size := utf8.DecodeLastRuneInString(s)
	return r, len(s) - size
}

func isRefWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }
