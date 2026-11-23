// Package redact removes secret values and secret-shaped text from strings.
package redact

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

const marker = "[REDACTED]"
const minExactSecretLen = 6

type pattern struct {
	re   *regexp.Regexp
	repl string
}

var secretPatterns = []pattern{
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), marker},
	{regexp.MustCompile(`(?i)(authorization:[ \t]*)[^\r\n]+`), "${1}" + marker},
	{regexp.MustCompile(`x-access-token:[^@\s]+@`), "x-access-token:" + marker + "@"},
	{regexp.MustCompile(`(postgres(?:ql)?://[^:/@\s]+:)[^@\s]+@`), "${1}" + marker + "@"},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`), marker},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`), marker},
	{regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{10,}`), marker},
	{regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), marker},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), marker},
	{regexp.MustCompile(`\b\d{6,12}:[A-Za-z0-9_-]{30,}`), marker},
}

// Redactor removes known secret values and secret-shaped patterns.
// It is safe for concurrent use.
type Redactor struct {
	mu     sync.RWMutex
	values []string
}

// New seeds a redactor with exact secret values.
func New(values []string) *Redactor {
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
		s = strings.ReplaceAll(s, v, marker)
	}
	r.mu.RUnlock()
	for _, p := range secretPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}
