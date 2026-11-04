package config

import (
	"time"
)

type LLMConfig struct {
	DefaultProvider string
	Anthropic       AnthropicConfig
	OpenAI          OpenAIConfig
	Timeout         time.Duration
}

type AnthropicConfig struct {
	APIKey       string
	DefaultModel string
	MaxRetries   int
	Timeout      time.Duration
	BaseUrl      string
}

type OpenAIConfig struct {
	APIKey  string
	BaseURL string
	Model   string
}
