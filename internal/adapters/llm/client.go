package llm

import (
	"fmt"
	"net/http"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/config"
	"github.com/LDKhangg/nexus-hub/internal/domain"
)

const defaultHTTPTimeout = 60 * time.Second

// Client giữ các LLM provider đã cấu hình và provider mặc định.
type Client struct {
	providers       map[ProviderType]domain.LLMProvider
	defaultProvider ProviderType
}

func NewClient(cfg config.LLMConfig, sharedHTTPClient *http.Client) (*Client, error) {
	if sharedHTTPClient == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = defaultHTTPTimeout
		}
		sharedHTTPClient = &http.Client{Timeout: timeout}
	}

	providers := make(map[ProviderType]domain.LLMProvider)

	if cfg.Anthropic.APIKey != "" {
		anthropicClient, err := NewAnthropic(cfg.Anthropic, sharedHTTPClient)
		if err != nil {
			return nil, fmt.Errorf("init anthropic client: %w", err)
		}
		providers[ProviderAnthropic] = anthropicClient
	}

	if len(providers) == 0 {
		return nil, fmt.Errorf("không có LLM provider nào được cấu hình")
	}

	def, err := ParseProviderType(cfg.DefaultProvider)
	if err != nil {
		return nil, err
	}
	if _, ok := providers[def]; !ok {
		return nil, fmt.Errorf("provider mặc định %q chưa được cấu hình", def)
	}

	return &Client{providers: providers, defaultProvider: def}, nil
}

func (c *Client) Get(provider ProviderType) (domain.LLMProvider, error) {
	p, ok := c.providers[provider]
	if !ok {
		return nil, fmt.Errorf("cannot find provider: %s", provider)
	}
	return p, nil
}

func (c *Client) Default() domain.LLMProvider {
	return c.providers[c.defaultProvider]
}
