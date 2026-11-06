package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Server  ServerConfig
	Relay   RelayConfig
	ClickUp ClickUpConfig
	LLM     LLMConfig
}

type ServerConfig struct {
	Port            string
	BodyLimit       string
	ShutdownTimeout time.Duration
}

type RelayConfig struct {
	DebounceWindow time.Duration
	MaxWait        time.Duration
}

type ClickUpConfig struct {
	APIKey        string
	PublicBaseURL string
}

func Load() Config {
	return Config{
		Server: ServerConfig{
			Port:            ":" + getEnv("PORT", "8080"),
			BodyLimit:       getEnv("BODY_LIMIT", "1M"),
			ShutdownTimeout: getDuration("SHUTDOWN_TIMEOUT", 10*time.Second),
		},
		Relay: RelayConfig{
			DebounceWindow: getDuration("DEBOUNCE_WINDOW", 8*time.Second),
			MaxWait:        getDuration("DEBOUNCE_MAX_WAIT", 30*time.Second),
		},
		ClickUp: ClickUpConfig{
			APIKey:        os.Getenv("CLICKUP_API"),
			PublicBaseURL: os.Getenv("PUBLIC_BASE_URL"),
		},
		LLM: LLMConfig{
			DefaultProvider: getEnv("LLM_DEFAULT_PROVIDER", "anthropic"),
			Timeout:         getDuration("LLM_TIMEOUT", 60*time.Second),
			Anthropic: AnthropicConfig{
				APIKey:           os.Getenv("ANTHROPIC_API_KEY"),
				DefaultModel:     getEnv("ANTHROPIC_MODEL", "claude-opus-5-5"),
				DefaultMaxTokens: int64(getInt("ANTHROPIC_MAX_TOKENS", 16000)),
				MaxRetries:       getInt("ANTHROPIC_MAX_RETRIES", 2),
				BaseURL:          os.Getenv("ANTHROPIC_BASE_URL"),
			},
		},
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func getInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
