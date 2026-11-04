package domain

type Role string

const (
	UserRole Role = "UserRole"
	AIRole   Role = "AiRole"
)

type Message struct {
	Role Role
	Text string
}

type LLMRequest struct {
	Model    string
	MaxToken int32
	Messages []Message
}

type LLMResponse struct {
	Text string
}
