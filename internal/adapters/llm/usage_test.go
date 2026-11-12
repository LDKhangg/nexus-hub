package llm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/secrets"
	"github.com/LDKhangg/nexus-hub/internal/settings"
)

func TestAnthropic_GenerateUsage(t *testing.T) {
	a := toolServer(t, 200, endTurnResp, nil)
	r, err := a.Generate(context.Background(), &domain.LLMRequest{Model: "m", MaxTokens: 300, Messages: []domain.Message{{Role: domain.UserRole, Text: "hi"}}})
	if err != nil || r.Text != "final answer" || r.Usage != (domain.Usage{Input: 120, Output: 30, CacheRead: 80, CacheWrite: 40}) {
		t.Fatalf("%+v %v", r, err)
	}
	bad := toolServer(t, 400, `{"type":"error","error":{"type":"invalid_request_error","message":"echo SECRET-PROMPT"}}`, nil)
	_, err = bad.Generate(context.Background(), &domain.LLMRequest{Messages: []domain.Message{{Role: domain.UserRole, Text: "hi"}}})
	if err == nil || strings.Contains(err.Error(), "SECRET-PROMPT") {
		t.Fatalf("err %v", err)
	}
	refused := toolServer(t, 200, strings.Replace(endTurnResp, `"end_turn"`, `"refusal"`, 1), nil)
	if _, err := refused.Generate(context.Background(), &domain.LLMRequest{Messages: []domain.Message{{Role: domain.UserRole, Text: "hi"}}}); !errors.Is(err, domain.ErrLLMRefused) {
		t.Fatalf("refusal %v", err)
	}
}

type stubResolver struct {
	val string
	err error
	ref string
}

func (s *stubResolver) Resolve(_ context.Context, _, ref string) (string, error) {
	s.ref = ref
	return s.val, s.err
}

func TestNewClientFromSecret_Missing(t *testing.T) {
	s := settings.Defaults()
	res := &stubResolver{err: &secrets.Error{Code: secrets.CodeMissingCredentials}}
	c, err := NewClientFromSecret(context.Background(), res, "ws", s, nil)
	if c != nil || !errors.Is(err, ErrLLMUnconfigured) || res.ref != "anthropic" {
		t.Fatalf("%v %v ref=%s", c, err, res.ref)
	}
	c, err = NewClientFromSecret(context.Background(), &stubResolver{val: "sk-ant-test"}, "ws", s, nil)
	if err != nil || c == nil || c.Default() == nil || c.ToolCalling() == nil {
		t.Fatalf("%v %v", c, err)
	}
}
