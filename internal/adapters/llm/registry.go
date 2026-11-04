package llm

import (
	"fmt"

	"github.com/LDKhangg/nexus-hub/internal/domain"
)

type ProviderType string

const (
	ProviderAnthropic ProviderType = "Anthorpic"
	ProviderOpenAI    ProviderType = "OpenAI"
	ProviderGoogle    ProviderType = "Google"
)

func ParseProviderType(s string) (ProviderType, error) {
	pt := ProviderType(s)
	switch pt {
	case ProviderAnthropic, ProviderOpenAI:
		return pt, nil
	default:
		return "", fmt.Errorf("nhà cung cấp LLM không hợp lệ hoặc chưa hỗ trợ: %s", s)
	}
}

type Registry struct {
	providers  map[ProviderType]domain.LLMProvider
	defaultkey string
}

func NewRegisty(key string) *Registry {
	return &Registry{
		providers:  make(map[ProviderType]domain.LLMProvider),
		defaultkey: key,
	}
}

func (r *Registry) Get(provider ProviderType) (domain.LLMProvider, error) {
	if provider == "" {
		return nil, fmt.Errorf("invalid provider")
	}
	res, existing := r.providers[provider]
	if !existing {
		return nil, fmt.Errorf("cant not find provider %s", provider)
	}
	return res, nil
}
