package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/anthropics/anthropic-sdk-go/v2"
)

// APIError is an Anthropic HTTP failure with the response body withheld:
// bodies may echo prompt content.
type APIError struct {
	StatusCode int
	Retryable  bool
}

func (e *APIError) Error() string { return fmt.Sprintf("anthropic: http %d", e.StatusCode) }

// safeErr strips upstream bodies from SDK errors.
func safeErr(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		code := apiErr.StatusCode
		return &APIError{StatusCode: code, Retryable: code == 408 || code == 409 || code == 429 || code >= 500}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &APIError{Retryable: true}
}

func toolParam(d domain.ToolDef) (anthropic.ToolParam, error) {
	var schema map[string]any
	if err := json.Unmarshal(d.InputSchema, &schema); err != nil {
		return anthropic.ToolParam{}, fmt.Errorf("tool %s: invalid input schema", d.Name)
	}
	in := anthropic.ToolInputSchemaParam{Properties: schema["properties"], ExtraFields: map[string]any{}}
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				in.Required = append(in.Required, s)
			}
		}
	}
	for k, v := range schema {
		if k != "properties" && k != "required" && k != "type" {
			in.ExtraFields[k] = v
		}
	}
	p := anthropic.ToolParam{Name: d.Name, InputSchema: in}
	if d.Description != "" {
		p.Description = anthropic.String(d.Description)
	}
	return p, nil
}

// GenerateWithTools runs one tool-calling step. With CacheStatic, the system
// block and the last tool carry an ephemeral cache breakpoint.
func (a *Anthropic) GenerateWithTools(ctx context.Context, req *domain.ToolRequest) (*domain.ToolResponse, error) {
	if req == nil || len(req.Turns) == 0 {
		return nil, errors.New("anthropic tools: empty request")
	}
	model := req.Model
	if model == "" {
		model = a.defaultModel
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = a.defaultMaxTokens
	}
	params := anthropic.MessageNewParams{Model: model, MaxTokens: maxTokens}
	if req.System != "" {
		sys := anthropic.TextBlockParam{Text: req.System}
		if req.CacheStatic {
			sys.CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
		params.System = []anthropic.TextBlockParam{sys}
	}
	for i, d := range req.Tools {
		p, err := toolParam(d)
		if err != nil {
			return nil, err
		}
		if req.CacheStatic && i == len(req.Tools)-1 {
			p.CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: &p})
	}
	for i, t := range req.Turns {
		var blocks []anthropic.ContentBlockParamUnion
		for _, r := range t.Results {
			blocks = append(blocks, anthropic.NewToolResultBlock(r.CallID, r.Content, r.IsError))
		}
		if t.Text != "" {
			blocks = append(blocks, anthropic.NewTextBlock(t.Text))
		}
		for _, c := range t.Calls {
			var input any = json.RawMessage(c.Input)
			if len(c.Input) == 0 {
				input = map[string]any{}
			}
			blocks = append(blocks, anthropic.NewToolUseBlock(c.ID, input, c.Name))
		}
		if len(blocks) == 0 {
			return nil, fmt.Errorf("anthropic tools: turn %d is empty", i)
		}
		switch t.Role {
		case domain.UserRole:
			params.Messages = append(params.Messages, anthropic.NewUserMessage(blocks...))
		case domain.AIRole:
			params.Messages = append(params.Messages, anthropic.NewAssistantMessage(blocks...))
		default:
			return nil, fmt.Errorf("anthropic tools: turn %d has unknown role", i)
		}
	}

	resp, err := a.client.Messages.New(ctx, params)
	if err != nil {
		return nil, safeErr(err)
	}
	out := &domain.ToolResponse{Usage: usageOf(resp.Usage)}
	var text strings.Builder
	for _, block := range resp.Content {
		switch b := block.AsAny().(type) {
		case anthropic.TextBlock:
			text.WriteString(b.Text)
		case anthropic.ToolUseBlock:
			out.Calls = append(out.Calls, domain.ToolCall{ID: b.ID, Name: b.Name, Input: b.Input})
		}
	}
	out.Text = text.String()
	switch resp.StopReason {
	case anthropic.StopReasonToolUse:
		out.Stop = domain.StopToolUse
	case anthropic.StopReasonMaxTokens:
		out.Stop = domain.StopMaxTokens
	case anthropic.StopReasonRefusal:
		out.Stop = domain.StopRefusal
	default:
		out.Stop = domain.StopEndTurn
	}
	return out, nil
}

var _ domain.ToolCallingProvider = (*Anthropic)(nil)
