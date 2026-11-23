package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/config"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

const (
	defaultAnthropicModel     = "claude-opus-5-5"
	defaultAnthropicMaxTokens = 16000
)

// ErrRefused aliases domain.ErrLLMRefused.
var ErrRefused = domain.ErrLLMRefused

type Anthropic struct {
	client           *anthropic.Client
	defaultModel     string
	defaultMaxTokens int64
}

func NewAnthropic(cfg config.AnthropicConfig, httpClient *http.Client) (*Anthropic, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("anthropic api key is empty")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}

	opts := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithHTTPClient(httpClient),
	}
	if cfg.MaxRetries > 0 {
		opts = append(opts, option.WithMaxRetries(cfg.MaxRetries))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}

	model := cfg.DefaultModel
	if model == "" {
		model = defaultAnthropicModel
	}
	maxTokens := cfg.DefaultMaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultAnthropicMaxTokens
	}

	c := anthropic.NewClient(opts...)
	return &Anthropic{client: &c, defaultModel: model, defaultMaxTokens: maxTokens}, nil
}

func (a *Anthropic) Generate(ctx context.Context, req *domain.LLMRequest) (*domain.LLMResponse, error) {
	if req == nil {
		return nil, errors.New("anthropic generate: nil request")
	}

	// Không sửa trực tiếp req của caller.
	model := req.Model
	if model == "" {
		model = a.defaultModel
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = a.defaultMaxTokens
	}

	msgs, err := anthropicMessages("anthropic generate", req.Messages, req.CacheHistory)
	if err != nil {
		return nil, err
	}

	params := anthropic.MessageNewParams{
		Model:     model,
		MaxTokens: maxTokens,
		Messages:  msgs,
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}

	resp, err := a.client.Messages.New(ctx, params)
	if err != nil {
		return nil, safeErr(err)
	}
	if resp.StopReason == anthropic.StopReasonRefusal {
		return nil, ErrRefused
	}

	var text strings.Builder
	for _, block := range resp.Content {
		if b, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(b.Text)
		}
	}
	return &domain.LLMResponse{Text: text.String(), Usage: usageOf(resp.Usage)}, nil
}

// anthropicMessages converts plain messages. With cache, the newest message
// carries an ephemeral cache breakpoint; content and order never change.
func anthropicMessages(op string, in []domain.Message, cache bool) ([]anthropic.MessageParam, error) {
	msgs := make([]anthropic.MessageParam, 0, len(in))
	for i, m := range in {
		block := anthropic.NewTextBlock(m.Text)
		if cache && i == len(in)-1 {
			block.OfText.CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
		switch m.Role {
		case domain.UserRole:
			msgs = append(msgs, anthropic.NewUserMessage(block))
		case domain.AIRole:
			msgs = append(msgs, anthropic.NewAssistantMessage(block))
		default:
			return nil, fmt.Errorf("%s: message %d has unknown role %q", op, i, m.Role)
		}
	}
	if len(msgs) == 0 {
		return nil, errors.New(op + ": no messages")
	}
	return msgs, nil
}

// GenerateJSON runs one structured-output call (output_config.format with a
// JSON schema), no tools. A refusal or a cut output is returned with its
// Stop reason and usage; the caller validates the JSON.
func (a *Anthropic) GenerateJSON(ctx context.Context, req *domain.JSONRequest) (*domain.JSONResponse, error) {
	if req == nil {
		return nil, errors.New("anthropic json: nil request")
	}
	var schema map[string]any
	if err := json.Unmarshal(req.Schema, &schema); err != nil || schema == nil {
		return nil, errors.New("anthropic json: invalid schema")
	}
	schema = anthropicOutputSchema(schema)
	model := req.Model
	if model == "" {
		model = a.defaultModel
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = a.defaultMaxTokens
	}
	msgs, err := anthropicMessages("anthropic json", req.Messages, req.CacheHistory)
	if err != nil {
		return nil, err
	}
	params := anthropic.MessageNewParams{
		Model:        model,
		MaxTokens:    maxTokens,
		Messages:     msgs,
		OutputConfig: anthropic.OutputConfigParam{Format: anthropic.JSONOutputFormatParam{Schema: schema}},
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}
	resp, err := a.client.Messages.New(ctx, params)
	if err != nil {
		return nil, safeErr(err)
	}
	var text strings.Builder
	for _, block := range resp.Content {
		if b, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(b.Text)
		}
	}
	out := &domain.JSONResponse{JSON: json.RawMessage(text.String()), Usage: usageOf(resp.Usage), Stop: domain.StopEndTurn}
	switch resp.StopReason {
	case anthropic.StopReasonMaxTokens:
		out.Stop = domain.StopMaxTokens
	case anthropic.StopReasonRefusal:
		out.Stop = domain.StopRefusal
	case anthropic.StopReasonToolUse:
		out.Stop = domain.StopToolUse
	}
	return out, nil
}

// anthropicOutputSchema copies a JSON schema and moves constraints unsupported
// by Anthropic structured output into descriptions. Callers retain the full
// schema for client-side validation.
func anthropicOutputSchema(schema map[string]any) map[string]any {
	return copyAnthropicSchemaValue(schema).(map[string]any)
}

func copyAnthropicSchemaValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, child := range value {
			out[key] = copyAnthropicSchemaValue(child)
		}

		constraints := []string{
			"minLength", "maxLength", "minimum", "maximum",
			"exclusiveMinimum", "exclusiveMaximum", "multipleOf", "maxItems",
		}
		var notes []string
		for _, key := range constraints {
			if constraint, ok := out[key]; ok {
				notes = append(notes, key+": "+schemaConstraintValue(constraint))
				delete(out, key)
			}
		}
		if constraint, ok := out["minItems"]; ok && schemaNumberGreaterThanOne(constraint) {
			notes = append(notes, "minItems: "+schemaConstraintValue(constraint))
			delete(out, "minItems")
		}
		if len(notes) > 0 {
			description, _ := out["description"].(string)
			if description != "" {
				description += "\n"
			}
			out["description"] = description + strings.Join(notes, "\n")
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, child := range value {
			out[i] = copyAnthropicSchemaValue(child)
		}
		return out
	default:
		return value
	}
}

func schemaConstraintValue(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

func schemaNumberGreaterThanOne(value any) bool {
	switch value := value.(type) {
	case float64:
		return value > 1
	case float32:
		return value > 1
	case int:
		return value > 1
	case int8:
		return value > 1
	case int16:
		return value > 1
	case int32:
		return value > 1
	case int64:
		return value > 1
	case uint:
		return value > 1
	case uint8:
		return value > 1
	case uint16:
		return value > 1
	case uint32:
		return value > 1
	case uint64:
		return value > 1
	default:
		return false
	}
}

var (
	_ domain.LLMProvider  = (*Anthropic)(nil)
	_ domain.JSONProvider = (*Anthropic)(nil)
)
