package llm

import (
	"context"
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

var ErrRefused = errors.New("anthropic: request refused by safety classifier")

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

	msgs := make([]anthropic.MessageParam, 0, len(req.Messages))
	for i, m := range req.Messages {
		switch m.Role {
		case domain.UserRole:
			msgs = append(msgs, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Text)))
		case domain.AIRole:
			msgs = append(msgs, anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Text)))
		default:
			return nil, fmt.Errorf("anthropic generate: message %d has unknown role %q", i, m.Role)
		}
	}
	if len(msgs) == 0 {
		return nil, errors.New("anthropic generate: no messages")
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
		return nil, fmt.Errorf("anthropic generate: %w", err)
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
	return &domain.LLMResponse{Text: text.String()}, nil
}

var _ domain.LLMProvider = (*Anthropic)(nil)
