package agent

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestCleanInput(t *testing.T) {
	in := "a\x00b\x07c\u0085d\u200be\u200cf\u200dg\u2060h\ufeffi\u202ej\u2066k\u2069l\tm\nn"
	if got, want := CleanInput(in), "abcdefghijkl\tm\nn"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	vi := "Xin chào thế giới 🚀👍🏽 — đường dẫn"
	if got := CleanInput(vi); got != vi {
		t.Fatalf("vietnamese/emoji altered: %q", got)
	}
	if got := CleanInput("a\n\n\n\nb\r\n\r\n\r\nc"); got != "a\n\nb\n\nc" {
		t.Fatalf("blank lines not collapsed: %q", got)
	}
}

func TestRedactSecrets(t *testing.T) {
	r := NewRedactor([]string{"supersecretvalue", "abc"})
	cases := []struct {
		name, in, mustNot, must string
	}{
		{"ghp", "token ghp_" + strings.Repeat("a", 36) + " end", "ghp_aaaa", "end"},
		{"gho", "gho_" + strings.Repeat("B", 36), "gho_BBBB", ""},
		{"ghs", "ghs_" + strings.Repeat("c", 36), "ghs_cccc", ""},
		{"github_pat", "github_pat_11ABCDEFG0" + strings.Repeat("x", 59), "github_pat_11", ""},
		{"anthropic", "key sk-ant-api03-" + strings.Repeat("Z", 40), "sk-ant-api03", "key"},
		{"slack", "xoxb-123456-abcdefABCDEF", "xoxb-123456", ""},
		{"aws", "AKIAIOSFODNN7EXAMPLE", "AKIAIOSFODNN7", ""},
		{"telegram", "bot 123456789:AAH" + strings.Repeat("k", 32), "123456789:AAH", "bot"},
		{"pem", "x\n-----BEGIN RSA PRIVATE KEY-----\nMIIEow\nabc\n-----END RSA PRIVATE KEY-----\ny", "MIIEow", "x"},
		{"pem generic", "-----BEGIN PRIVATE KEY-----\nQQQ\n-----END PRIVATE KEY-----", "QQQ", ""},
		{"postgres keeps user", "postgres://nexus:hunter2pass@localhost:5432/db", "hunter2pass", "postgres://nexus:"},
		{"authorization", "Authorization: Bearer abc.def.ghi", "abc.def.ghi", "Authorization:"},
		{"git url token", "https://x-access-token:abc123tok@github.com/o/r.git", "abc123tok", "github.com/o/r.git"},
		{"exact value", "the supersecretvalue leaked", "supersecretvalue", "leaked"},
		{"short ignored", "abc stays", "", "abc stays"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.RedactSecrets(tc.in)
			if tc.mustNot != "" && strings.Contains(got, tc.mustNot) {
				t.Fatalf("secret survived: %q", got)
			}
			if tc.must != "" && !strings.Contains(got, tc.must) {
				t.Fatalf("expected %q kept in %q", tc.must, got)
			}
		})
	}
}

func TestCleanOutboundMentions(t *testing.T) {
	r := NewRedactor(nil)
	got := r.CleanOutbound("ping @LDKhangg and mail a.b@example.com\u200b", true)
	if got != "ping `@LDKhangg` and mail a.b@example.com" {
		t.Fatalf("got %q", got)
	}
	if got := r.CleanOutbound("ping @LDKhangg", false); got != "ping @LDKhangg" {
		t.Fatalf("got %q", got)
	}
	if got := r.CleanOutbound("x ghp_"+strings.Repeat("a", 36), false); strings.Contains(got, "ghp_a") {
		t.Fatalf("not redacted: %q", got)
	}
}

func TestRedactorConcurrentAdd(t *testing.T) {
	r := NewRedactor(nil)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) { defer wg.Done(); r.Add(fmt.Sprintf("secret-value-%03d", i)) }(i)
		go func() { defer wg.Done(); _ = r.RedactSecrets("secret-value-001") }()
	}
	wg.Wait()
	if got := r.RedactSecrets("x secret-value-042 y"); strings.Contains(got, "secret-value-042") {
		t.Fatalf("not redacted: %q", got)
	}
}
