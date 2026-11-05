package config

import "time"

type LLMConfig struct {
	DefaultProvider string
	Timeout         time.Duration
	Anthropic       AnthropicConfig
	OpenAI          OpenAIConfig
}

type AnthropicConfig struct {
	APIKey           string
	DefaultModel     string
	DefaultMaxTokens int64
	MaxRetries       int
	BaseURL          string
}

type OpenAIConfig struct {
	APIKey  string
	BaseURL string
	Model   string
}
