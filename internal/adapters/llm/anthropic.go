package llm

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/config"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

type Anthropic struct {
	client       *anthropic.Client
	defaultModel string
}

func NewAnthropic(cfg config.AnthropicConfig, sharedHttp *http.Client) (*Anthropic, error) {
	apiKey := cfg.APIKey
	if apiKey == "" {
		return nil, fmt.Errorf("anthropics is null")
	}
	if sharedHttp == nil {
		timeout := cfg.Timeout
		if timeout == 0 {
			timeout = 20 * time.Second
		}
		sharedHttp = &http.Client{Timeout: timeout}
	}

	maxRetries := cfg.MaxRetries
	if maxRetries == 0 {
		maxRetries = 5
	}

	opts := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithHTTPClient(sharedHttp),
		option.WithMaxRetries(maxRetries),
	}

	if cfg.BaseUrl != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseUrl))
	}

	c := anthropic.NewClient(opts...)
	return &Anthropic{
		client:       &c,
		defaultModel: cfg.DefaultModel,
	}, nil
}

func (a *Anthropic) Generate(ctx context.Context, req *domain.LLMRequest) (*domain.LLMResponse, error) {
	if req.Model == "" {
		req.Model = a.defaultModel
	}
	aMess := make([]anthropic.MessageParam, 0, len(req.Messages))
	for _, m := range req.Messages {
		switch m.Role {
		case domain.UserRole:
			aMess = append(aMess, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Text)))
		case domain.AIRole:
			aMess = append(aMess, anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Text)))
		default:
			continue
		}
	}

	response, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     req.Model,
		MaxTokens: int64(req.MaxToken),
		Messages:  aMess,
	})
	if err != nil {
		return nil, fmt.Errorf("loi roi")
	}

	var text strings.Builder
	for _, block := range response.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}

	return &domain.LLMResponse{Text: text.String()}, nil
}

var _ domain.LLMProvider = (*Anthropic)(nil)
