package llm

import "fmt"

type ProviderType string

const (
	ProviderAnthropic ProviderType = "anthropic"
	ProviderOpenAI    ProviderType = "openai"
	ProviderGoogle    ProviderType = "google"
	// ProviderClaudeCLI runs the Claude Code CLI signed in on the host.
	ProviderClaudeCLI ProviderType = "claude_cli"
)

// ParseProviderType chỉ chấp nhận provider đã có implementation.
func ParseProviderType(s string) (ProviderType, error) {
	pt := ProviderType(s)
	switch pt {
	case ProviderAnthropic, ProviderClaudeCLI:
		return pt, nil
	default:
		return "", fmt.Errorf("nhà cung cấp LLM không hợp lệ hoặc chưa hỗ trợ: %q", s)
	}
}
