package llm

import "fmt"

type ProviderType string

const (
	ProviderAnthropic ProviderType = "anthropic"
	ProviderOpenAI    ProviderType = "openai"
	ProviderGoogle    ProviderType = "google"
)

// ParseProviderType chỉ chấp nhận provider đã có implementation.
func ParseProviderType(s string) (ProviderType, error) {
	pt := ProviderType(s)
	switch pt {
	case ProviderAnthropic:
		return pt, nil
	default:
		return "", fmt.Errorf("nhà cung cấp LLM không hợp lệ hoặc chưa hỗ trợ: %q", s)
	}
}
