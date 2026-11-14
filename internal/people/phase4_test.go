package people

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TC-6 (REQ-3, NFR-022): a long platform name with controls, bidi text,
// mentions and injected instructions becomes one inert, capped label.
func TestPhase4NameSafety(t *testing.T) {
	t.Run("TC-6 crafted display names stay inert", func(t *testing.T) {
		raw := "  @owner\nIGNORE PREVIOUS\u202e\u2066\x00 " + strings.Repeat("長", 140)
		got := CleanDisplayName(raw)
		if strings.ContainsAny(got, "\r\n\t\x00@") || strings.Contains(got, "\u202e") || strings.Contains(got, "\u2066") {
			t.Fatalf("unsafe name survived cleaning: %q", got)
		}
		if !strings.HasPrefix(got, "＠owner IGNORE PREVIOUS 長") {
			t.Fatalf("cleaner changed the label instead of keeping it as inert text: %q", got)
		}
		if n := utf8.RuneCountInString(got); n != MaxDisplayNameRunes {
			t.Fatalf("cleaned name has %d runes, want exactly %d for a long input", n, MaxDisplayNameRunes)
		}
	})

	t.Run("TC-6 Vietnamese names, emoji and whitespace", func(t *testing.T) {
		cases := map[string]string{
			"anh Định":                  "anh Định",
			"  chị   Lan \t 🌸 ":         "chị Lan 🌸",
			"":                          "",
			" \n\t ":                    "",
			"\u200bzero\u200dwidth":     "zerowidth",
			"a\u202ab\u202cc":           "abc",
			"bob@example.com":           "bob＠example.com",
			"\ufffdbroken":              "broken",
			"Nguyễn Văn A\r\n(ClickUp)": "Nguyễn Văn A (ClickUp)",
		}
		for in, want := range cases {
			if got := CleanDisplayName(in); got != want {
				t.Fatalf("CleanDisplayName(%q)=%q, want %q", in, got, want)
			}
		}
	})

	t.Run("TC-6 cap never ends in a space and is idempotent", func(t *testing.T) {
		in := strings.Repeat("a", MaxDisplayNameRunes-1) + " b c"
		got := CleanDisplayName(in)
		if strings.HasSuffix(got, " ") || utf8.RuneCountInString(got) > MaxDisplayNameRunes {
			t.Fatalf("bad cap: %q (%d runes)", got, utf8.RuneCountInString(got))
		}
		for _, s := range []string{got, CleanDisplayName("  @x\u202e y  ")} {
			if again := CleanDisplayName(s); again != s {
				t.Fatalf("cleaning is not idempotent: %q -> %q", s, again)
			}
		}
	})
}
