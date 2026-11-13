package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

// CheckResult is the outcome of one health-check call. It never holds
// prompt or answer text.
type CheckResult struct {
	OK           bool
	Code         string // empty when OK
	Latency      time.Duration
	InputTokens  int64
	OutputTokens int64
	CacheTokens  int64 // cache reads and writes
	CostUSD      float64
}

const (
	checkSystem = "You are a health check. Reply only with the JSON object requested."
	checkPrompt = `Reply with this JSON object and nothing else: {"ok": true}`
	checkSchema = `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`
	// CodeInvalidOutput: the check answer was not {"ok": true}.
	CodeInvalidOutput = "invalid_output"
)

// Check runs one tiny structured call through p: the native structured
// call when p has one, otherwise a plain call whose answer must parse as
// the same JSON object.
func Check(ctx context.Context, p domain.LLMProvider, model string) CheckResult {
	start := time.Now()
	var res CheckResult
	var answer []byte
	if sp, ok := p.(StructuredProvider); ok {
		r, err := sp.GenerateStructured(ctx, &StructuredRequest{Model: model, System: checkSystem, Prompt: checkPrompt, Schema: json.RawMessage(checkSchema)})
		res.Latency = time.Since(start)
		if err != nil {
			res.Code = ErrorCode(err)
			return res
		}
		res.InputTokens, res.OutputTokens, res.CacheTokens, res.CostUSD = r.Usage.Input, r.Usage.Output, r.Usage.CacheRead+r.Usage.CacheWrite, r.CostUSD
		answer = r.JSON
	} else {
		r, err := p.Generate(ctx, &domain.LLMRequest{Model: model, System: checkSystem, MaxTokens: 50, Messages: []domain.Message{{Role: domain.UserRole, Text: checkPrompt}}})
		res.Latency = time.Since(start)
		if err != nil {
			res.Code = ErrorCode(err)
			return res
		}
		res.InputTokens, res.OutputTokens, res.CacheTokens = r.Usage.Input, r.Usage.Output, r.Usage.CacheRead+r.Usage.CacheWrite
		answer = []byte(stripFence(r.Text))
	}
	var v struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal(answer, &v) != nil || !v.OK {
		res.Code = CodeInvalidOutput
		return res
	}
	res.OK = true
	return res
}

// stripFence removes a Markdown code fence around a JSON answer.
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return strings.TrimSpace(s)
}

// ErrorCode maps any provider error to a safe code.
func ErrorCode(err error) string {
	if le, ok := domain.AsLLMError(err); ok {
		return le.Code
	}
	var ae *APIError
	switch {
	case errors.Is(err, ErrLLMUnconfigured):
		return domain.LLMCodeUnconfigured
	case errors.Is(err, domain.ErrLLMRefused):
		return "llm_refused"
	case errors.Is(err, context.DeadlineExceeded):
		return domain.LLMCodeTimeout
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.As(err, &ae):
		switch {
		case ae.StatusCode == 401 || ae.StatusCode == 403:
			return domain.LLMCodeUnconfigured
		case ae.StatusCode == 429 || ae.StatusCode == 529:
			return domain.LLMCodeUnavailable
		}
	}
	return domain.LLMCodeError
}
