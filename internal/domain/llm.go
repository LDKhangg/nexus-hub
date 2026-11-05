package domain

type Role string

const (
	UserRole Role = "user"
	AIRole   Role = "assistant"
)

type Message struct {
	Role Role
	Text string
}

type LLMRequest struct {
	Model     string
	System    string
	MaxTokens int64
	Messages  []Message
}

type LLMResponse struct {
	Text string
}
