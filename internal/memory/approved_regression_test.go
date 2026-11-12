package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// TC-42 (REQ-F5): all compressor channels, including generated output, use
// the supplied redaction boundary. No dependency on the agent package.
func TestApproved_CompressorRedactsInputAndStoredOutput(t *testing.T) {
	secret := "private-compressor-password"
	s := &sumStore{summary: "Tóm tắt cũ " + secret, cursor: 9, cands: cands(5)}
	for i := range s.cands {
		s.cands[i].SenderName = "Tên " + secret
		s.cands[i].Content = "Dữ liệu Việt 🧠 " + secret
	}
	l := &sumLLM{text: "Kết luận 🔎 " + secret}
	clean := func(v string) string { return strings.ReplaceAll(v, secret, "[REDACTED]") }
	c := NewCompressor(s, func(context.Context, string) (domain.LLMProvider, error) { return l, nil }, "fake-model", clean)
	did, _, err := c.Compress(context.Background(), "ws", "thread")
	if err != nil || !did || s.saves != 1 {
		t.Fatal("compressor did not save valid summary")
	}
	for _, text := range []string{l.req.System, l.req.Messages[0].Text, s.saved} {
		if strings.Contains(text, secret) {
			t.Error("compressor leaked secret")
		}
	}
	if !strings.Contains(l.req.Messages[0].Text, "Dữ liệu Việt 🧠") || s.saved != "Kết luận 🔎 [REDACTED]" || s.savedFrom != 9 || s.savedTo != 14 {
		t.Error("redaction damaged business content or summary cursor")
	}
}
