package llm

import (
	"fmt"
	"net/http"

	"github.com/LDKhangg/nexus-hub/internal/config"
	"github.com/LDKhangg/nexus-hub/internal/domain"
)

type Client struct {
	providers map[ProviderType]domain.LLMProvider
}

func NewClient(cfg config.LLMConfig, sharedHttpclient *http.Client) (*Client, error) {
	if sharedHttpclient == nil {
		sharedHttpclient = &http.Client{Timeout: cfg.Timeout}
	}

	providers := make(map[ProviderType]domain.LLMProvider)

	if cfg.Anthropic.APIKey != "" {
		anthropicClient, err := NewAnthropic(cfg.Anthropic, sharedHttpclient)
		if err != nil {
			return nil, fmt.Errorf("loi trong qua trinh int anthropic client")
		}
		providers[ProviderAnthropic] = anthropicClient
	}

	if len(providers) == 0 {
		return nil, fmt.Errorf("không có LLM provider nào được cấu hình")
	}
	return &Client{
		providers: providers,
	}, nil
}

func (c *Client) Get(provider ProviderType) (domain.LLMProvider, error) {
	p, existing := c.providers[provider]
	if !existing {
		return nil, fmt.Errorf("cant find provider: %s", provider)
	}
	return p, nil
}
