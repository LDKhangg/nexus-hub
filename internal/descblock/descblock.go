// Package descblock finds, replaces and removes the managed blocks the
// behavior engine writes into a ClickUp task description. A block is the
// text between a start and an end marker line that carry the block name:
//
//	⟦nexus:<block>:start⟧
//	...body...
//	⟦nexus:<block>:end⟧
//
// The markers are plain text so they survive ClickUp's markdown
// round trip (an HTML comment may be dropped by the markdown renderer).
// When reading, a marker line is recognised after trimming spaces and
// dropping markdown backslash escapes, and the HTML comment form
// "<!-- nexus:<block>:start -->" is recognised too. Text outside every
// block is never changed.
package descblock

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
)

// ErrMarkers means the description's markers are missing one side,
// duplicated, nested or out of order. Nothing may be written then.
var ErrMarkers = errors.New("block_markers_invalid")

// ErrBody means a block body contains a marker line.
var ErrBody = errors.New("block_body_invalid")

var (
	bracketRe = regexp.MustCompile(`^⟦nexus:([^⟦⟧]{1,63}):(start|end)⟧$`)
	commentRe = regexp.MustCompile(`^<!--\s*nexus:(.{1,63}?):(start|end)\s*-->$`)
	escapeRe  = regexp.MustCompile("\\\\([!-/:-@\\[-`{-~])")
)

// StartMarker and EndMarker are the marker lines the engine writes.
func StartMarker(block string) string { return "⟦nexus:" + block + ":start⟧" }
func EndMarker(block string) string   { return "⟦nexus:" + block + ":end⟧" }

// marker reports whether a line is a marker, with its block and side.
func marker(line string) (block string, start, ok bool) {
	l := strings.TrimSpace(escapeRe.ReplaceAllString(line, "$1"))
	m := bracketRe.FindStringSubmatch(l)
	if m == nil {
		m = commentRe.FindStringSubmatch(l)
	}
	if m == nil {
		return "", false, false
	}
	return strings.TrimSpace(m[1]), m[2] == "start", true
}

// IsMarker reports whether a line is a marker line of any block.
func IsMarker(line string) bool {
	_, _, ok := marker(line)
	return ok
}

// span is one block: the line index of its start and end markers.
type span struct {
	name       string
	start, end int
}

func splitLines(desc string) []string {
	return strings.Split(strings.ReplaceAll(desc, "\r\n", "\n"), "\n")
}

// parse returns every block of the description in order, or ErrMarkers
// when the markers are not well formed: each start is followed by the end
// of the same block before any other marker, and each block name appears
// at most once.
func parse(lines []string) ([]span, error) {
	var out []span
	seen := map[string]bool{}
	open := -1
	openName := ""
	for i, l := range lines {
		name, start, ok := marker(l)
		if !ok {
			continue
		}
		switch {
		case start && open >= 0:
			return nil, ErrMarkers // nested
		case start:
			if seen[name] {
				return nil, ErrMarkers // duplicated
			}
			seen[name] = true
			open, openName = i, name
		case open < 0 || name != openName:
			return nil, ErrMarkers // end without its start
		default:
			out = append(out, span{name: name, start: open, end: i})
			open, openName = -1, ""
		}
	}
	if open >= 0 {
		return nil, ErrMarkers // start without its end
	}
	return out, nil
}

// Validate checks that the description's markers are well formed.
func Validate(desc string) error {
	_, err := parse(splitLines(desc))
	return err
}

// Find returns the body of a block and whether the block exists.
func Find(desc, block string) (string, bool, error) {
	lines := splitLines(desc)
	spans, err := parse(lines)
	if err != nil {
		return "", false, err
	}
	for _, s := range spans {
		if s.name == block {
			return strings.Join(lines[s.start+1:s.end], "\n"), true, nil
		}
	}
	return "", false, nil
}

// CheckBody rejects a body that holds a marker line.
func CheckBody(body string) error {
	for _, l := range splitLines(body) {
		if IsMarker(l) {
			return ErrBody
		}
	}
	return nil
}

// Replace sets the body of a block, appending a new block at the end of the
// description when it is absent. Only the lines between the block's
// markers change.
func Replace(desc, block, body string) (string, error) {
	if err := CheckBody(body); err != nil {
		return "", err
	}
	lines := splitLines(desc)
	spans, err := parse(lines)
	if err != nil {
		return "", err
	}
	bodyLines := splitLines(strings.TrimRight(body, "\n"))
	for _, s := range spans {
		if s.name == block {
			out := append([]string{}, lines[:s.start+1]...)
			out = append(out, bodyLines...)
			out = append(out, lines[s.end:]...)
			return strings.Join(out, "\n"), nil
		}
	}
	head := strings.TrimRight(strings.Join(lines, "\n"), "\n \t")
	parts := []string{StartMarker(block)}
	parts = append(parts, bodyLines...)
	parts = append(parts, EndMarker(block))
	added := strings.Join(parts, "\n")
	if strings.TrimSpace(head) == "" {
		return added, nil
	}
	return head + "\n\n" + added, nil
}

// Remove deletes a block and its markers; a missing block is no change.
func Remove(desc, block string) (string, error) {
	lines := splitLines(desc)
	spans, err := parse(lines)
	if err != nil {
		return "", err
	}
	for _, s := range spans {
		if s.name == block {
			out := append([]string{}, lines[:s.start]...)
			out = append(out, lines[s.end+1:]...)
			return strings.TrimRight(strings.Join(out, "\n"), "\n \t"), nil
		}
	}
	return desc, nil
}

// Outside returns the description without any managed block, normalized
// for comparison: markdown escapes dropped, trailing spaces trimmed, runs
// of blank lines collapsed and outer blank lines removed. Two descriptions
// whose human-written text is the same have the same Outside.
func Outside(desc string) (string, error) {
	lines := splitLines(desc)
	spans, err := parse(lines)
	if err != nil {
		return "", err
	}
	skip := make([]bool, len(lines))
	for _, s := range spans {
		for i := s.start; i <= s.end; i++ {
			skip[i] = true
		}
	}
	return normalize(lines, skip), nil
}

// OutsidePlain is Outside for a plain-text rendering of a description,
// where marker lines may be malformed by the renderer: lines between a
// start and the next end marker are dropped, and a stray marker line is
// dropped too. It never fails.
func OutsidePlain(lines []string) string {
	skip := make([]bool, len(lines))
	in := false
	for i, l := range lines {
		_, start, ok := marker(l)
		switch {
		case ok && start:
			in = true
			skip[i] = true
		case ok:
			in = false
			skip[i] = true
		case in:
			skip[i] = true
		}
	}
	return normalize(lines, skip)
}

func normalize(lines []string, skip []bool) string {
	var out []string
	blank := false
	for i, l := range lines {
		if skip[i] {
			continue
		}
		l = strings.TrimRight(escapeRe.ReplaceAllString(l, "$1"), " \t\r")
		if strings.TrimSpace(l) == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, l)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// Hash is the hex SHA-256 of s.
func Hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// OutsideHash is Hash(Outside(desc)).
func OutsideHash(desc string) (string, error) {
	o, err := Outside(desc)
	if err != nil {
		return "", err
	}
	return Hash(o), nil
}

// Neutralize rewrites text so no line of it can be read as a marker line.
func Neutralize(s string) string {
	lines := splitLines(s)
	for i, l := range lines {
		if IsMarker(l) {
			l = strings.ReplaceAll(l, "⟦", "[")
			l = strings.ReplaceAll(l, "⟧", "]")
			l = strings.ReplaceAll(l, "<!--", "< !--")
			lines[i] = l
		}
	}
	return strings.Join(lines, "\n")
}
