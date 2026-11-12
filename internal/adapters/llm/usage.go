package llm

import (
	"context"
	"errors"
	"net/http"

	"github.com/LDKhangg/nexus-hub/internal/config"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/settings"
	"github.com/anthropics/anthropic-sdk-go/v2"
)

func usageOf(u anthropic.Usage) domain.Usage {
	return domain.Usage{Input: u.InputTokens, Output: u.OutputTokens, CacheRead: u.CacheReadInputTokens, CacheWrite: u.CacheCreationInputTokens}
}

// ErrLLMUnconfigured means no LLM credential is available; the agent falls
// back to notices instead of failing startup.
var ErrLLMUnconfigured = errors.New("llm: unconfigured")

// SecretResolver resolves credential references (secrets.Resolver).
type SecretResolver interface {
	Resolve(ctx context.Context, workspaceID, ref string) (string, error)
}

// NewClientFromSecret builds an Anthropic client whose API key comes from
// the credential named by settings.anthropic_credential.
func NewClientFromSecret(ctx context.Context, resolver SecretResolver, workspaceID string, s settings.Settings, httpc *http.Client) (*Client, error) {
	if resolver == nil {
		return nil, ErrLLMUnconfigured
	}
	key, err := resolver.Resolve(ctx, workspaceID, s.AnthropicCredential)
	if err != nil || key == "" {
		return nil, ErrLLMUnconfigured
	}
	return NewClient(config.LLMConfig{DefaultProvider: string(ProviderAnthropic), Anthropic: config.AnthropicConfig{APIKey: key, DefaultModel: s.ActorModel, MaxRetries: 2}}, httpc)
}

// ToolCalling returns the default provider if it supports tool calling.
func (c *Client) ToolCalling() domain.ToolCallingProvider {
	if tc, ok := c.Default().(domain.ToolCallingProvider); ok {
		return tc
	}
	return nil
}
