package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

type fakeLLM struct {
	req  *domain.LLMRequest
	resp *domain.LLMResponse
	err  error
}

func (f *fakeLLM) Generate(_ context.Context, r *domain.LLMRequest) (*domain.LLMResponse, error) {
	f.req = r
	return f.resp, f.err
}

const okTriage = `{"category":"investigate","urgency":"high","confidence":0.9,"summary":"login broken","goal":"find cause"}`

func TestTier1Router_Request(t *testing.T) {
	f := &fakeLLM{resp: &domain.LLMResponse{Text: okTriage}}
	d, _, err := NewTier1Router(f, "claude-haiku-5-5").Triage(context.Background(), packet(), "Be brief.", true)
	if err != nil || d.Category != CategoryInvestigate || d.Goal != "find cause" {
		t.Fatalf("%+v %v", d, err)
	}
	if f.req.Model != "claude-haiku-5-5" || f.req.MaxTokens != TriageMaxTokens {
		t.Fatalf("%+v", f.req)
	}
	for _, want := range []string{"untrusted", "JSON only", `"category"`, "investigate", "Be brief."} {
		if !strings.Contains(f.req.System, want) {
			t.Fatalf("system lacks %q:\n%s", want, f.req.System)
		}
	}
	if len(f.req.Messages) != 1 || f.req.Messages[0].Role != domain.UserRole || f.req.Messages[0].Text != packet().Render() {
		t.Fatalf("messages %+v", f.req.Messages)
	}
}

func TestTier1Router_NoToolsOmitsInvestigate(t *testing.T) {
	f := &fakeLLM{resp: &domain.LLMResponse{Text: `{"category":"notify","confidence":0.5,"summary":"s"}`}}
	if _, _, err := NewTier1Router(f, "m").Triage(context.Background(), packet(), "", false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.req.System, "investigate") {
		t.Fatalf("system mentions investigate:\n%s", f.req.System)
	}
}

func TestTier1Router_Errors(t *testing.T) {
	code := func(err error) (string, bool) {
		var te *TriageError
		if !errors.As(err, &te) {
			return "", false
		}
		return te.Code, te.Retryable
	}
	_, _, err := NewTier1Router(nil, "m").Triage(context.Background(), packet(), "", true)
	if c, r := code(err); c != CodeLLMUnconfigured || r {
		t.Fatalf("nil llm: %v", err)
	}
	_, _, err = NewTier1Router(&fakeLLM{err: domain.ErrLLMRefused}, "m").Triage(context.Background(), packet(), "", true)
	if c, r := code(err); c != CodeLLMRefused || r {
		t.Fatalf("refused: %v", err)
	}
	_, _, err = NewTier1Router(&fakeLLM{err: errors.New("http 500 with body")}, "m").Triage(context.Background(), packet(), "", true)
	if c, r := code(err); c != CodeLLMError || !r || strings.Contains(err.Error(), "body") {
		t.Fatalf("provider: %v", err)
	}
	_, _, err = NewTier1Router(&fakeLLM{resp: &domain.LLMResponse{Text: "sorry, no"}}, "m").Triage(context.Background(), packet(), "", true)
	if c, r := code(err); c != CodeInvalidDecision || !r {
		t.Fatalf("invalid: %v", err)
	}
}

func TestTier1Router_ReturnsUsage(t *testing.T) {
	u := domain.Usage{Input: 400, Output: 60, CacheRead: 10}
	f := &fakeLLM{resp: &domain.LLMResponse{Text: okTriage, Usage: u}}
	_, got, err := NewTier1Router(f, "m").Triage(context.Background(), packet(), "", true)
	if err != nil || got != u {
		t.Fatalf("%+v %v", got, err)
	}
	f = &fakeLLM{resp: &domain.LLMResponse{Text: "garbage", Usage: u}}
	if _, got, _ := NewTier1Router(f, "m").Triage(context.Background(), packet(), "", true); got != u {
		t.Fatalf("usage lost on invalid decision: %+v", got)
	}
}
