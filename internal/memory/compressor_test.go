package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

type sumStore struct {
	summary   string
	cursor    int64
	cands     []Message
	saved     string
	savedFrom int64
	savedTo   int64
	saves     int
}

func (s *sumStore) SummaryCandidates(context.Context, string, string) (string, int64, []Message, error) {
	return s.summary, s.cursor, s.cands, nil
}
func (s *sumStore) SaveSummary(_ context.Context, _, _ string, expected, through int64, summary string) error {
	s.saves++
	s.savedFrom, s.savedTo, s.saved = expected, through, summary
	return nil
}

type sumLLM struct {
	req  *domain.LLMRequest
	text string
	err  error
}

func (l *sumLLM) Generate(_ context.Context, r *domain.LLMRequest) (*domain.LLMResponse, error) {
	l.req = r
	if l.err != nil {
		return nil, l.err
	}
	return &domain.LLMResponse{Text: l.text, Usage: domain.Usage{Input: 900, Output: 120}}, nil
}

func cands(n int) []Message {
	out := make([]Message, n)
	for i := range out {
		out[i] = Message{ID: int64(10 + i), SenderRole: SenderRoleExternalUser, SenderName: "Alice", Content: fmt.Sprintf("message %d %s", i, strings.Repeat("x", 500))}
	}
	return out
}

func newComp(s *sumStore, l *sumLLM) *Compressor {
	return NewCompressor(s, func(context.Context, string) (domain.LLMProvider, error) { return l, nil }, "claude-haiku-5-5", func(s string) string { return s })
}

func TestCompressor_BelowThresholdNoCall(t *testing.T) {
	s := &sumStore{cands: cands(4)}
	l := &sumLLM{text: "x"}
	did, u, err := newComp(s, l).Compress(context.Background(), "ws", "th")
	if did || err != nil || l.req != nil || s.saves != 0 || u != (domain.Usage{}) {
		t.Fatalf("%v %v %+v", did, err, l.req)
	}
}

func TestCompressor_SummarizesAndSaves(t *testing.T) {
	s := &sumStore{summary: "earlier: login issue reported", cursor: 9, cands: cands(6)}
	l := &sumLLM{text: strings.Repeat("s", 2000)}
	did, u, err := newComp(s, l).Compress(context.Background(), "ws", "th")
	if !did || err != nil || u.Input != 900 {
		t.Fatalf("%v %v %+v", did, err, u)
	}
	if l.req.Model != "claude-haiku-5-5" || l.req.MaxTokens != SummaryMaxTokens || !strings.Contains(l.req.System, "untrusted") {
		t.Fatalf("%+v", l.req)
	}
	user := l.req.Messages[0].Text
	if !strings.Contains(user, "earlier: login issue reported") || !strings.Contains(user, "message 5") || strings.Contains(user, strings.Repeat("x", MessageBudget+1)) {
		t.Fatalf("user prompt:\n%s", user)
	}
	if s.saves != 1 || s.savedFrom != 9 || s.savedTo != 15 || len([]rune(s.saved)) > SummaryMaxRunes {
		t.Fatalf("save %d %d %d %d", s.saves, s.savedFrom, s.savedTo, len([]rune(s.saved)))
	}
}

func TestCompressor_LLMErrorNoSave(t *testing.T) {
	s := &sumStore{cands: cands(6)}
	_, _, err := newComp(s, &sumLLM{err: errors.New("boom")}).Compress(context.Background(), "ws", "th")
	if err == nil || s.saves != 0 {
		t.Fatalf("%v saves=%d", err, s.saves)
	}
	_, _, err = newComp(s, &sumLLM{text: "   "}).Compress(context.Background(), "ws", "th")
	if err == nil || s.saves != 0 {
		t.Fatalf("empty summary saved: %v", err)
	}
}
