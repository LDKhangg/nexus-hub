package domain

import (
	"encoding/json"
	"errors"
)

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
	Text  string
	Usage Usage
}

// Usage is token accounting for one or more LLM calls.
type Usage struct {
	Input, Output, CacheRead, CacheWrite int64
}

// Add accumulates o into u.
func (u *Usage) Add(o Usage) {
	u.Input += o.Input
	u.Output += o.Output
	u.CacheRead += o.CacheRead
	u.CacheWrite += o.CacheWrite
}

// ToolDef describes a tool to the model.
type ToolDef struct {
	Name, Description string
	InputSchema       json.RawMessage
}

// ToolCall is a tool invocation requested by the model.
type ToolCall struct {
	ID, Name string
	Input    json.RawMessage
}

// ToolResult answers one ToolCall.
type ToolResult struct {
	CallID, Content string
	IsError         bool
}

// Turn is one conversation turn: user text and/or tool results, or
// assistant text and/or tool calls.
type Turn struct {
	Role    Role
	Text    string
	Calls   []ToolCall
	Results []ToolResult
}

// StopReason is why the model stopped.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopRefusal   StopReason = "refusal"
)

// ToolRequest is a tool-calling request. CacheStatic marks the system
// prompt and the tool list as cacheable.
type ToolRequest struct {
	Model, System string
	MaxTokens     int64
	Tools         []ToolDef
	Turns         []Turn
	CacheStatic   bool
}

// ToolResponse is the model's reply.
type ToolResponse struct {
	Text  string
	Calls []ToolCall
	Stop  StopReason
	Usage Usage
}

// ErrLLMRefused is returned when the provider's safety system refuses.
var ErrLLMRefused = errors.New("llm: request refused")

// Safe LLM failure codes shared by every provider.
const (
	// LLMCodeError: the provider failed (bad output, error reply, non-zero
	// exit).
	LLMCodeError = "llm_error"
	// LLMCodeUnavailable: rate limited or overloaded; try again later.
	LLMCodeUnavailable = "llm_unavailable"
	// LLMCodeUnconfigured: missing or expired sign-in or credential.
	LLMCodeUnconfigured = "llm_unconfigured"
	// LLMCodeTimeout: the call ran longer than the provider timeout.
	LLMCodeTimeout = "llm_timeout"
)

// LLMError is a provider failure with a safe code. It never carries
// prompt, context or answer text.
type LLMError struct {
	Code      string
	Retryable bool
}

func (e *LLMError) Error() string { return "llm: " + e.Code }

// AsLLMError returns the LLMError in err's chain, if any.
func AsLLMError(err error) (*LLMError, bool) {
	var le *LLMError
	if errors.As(err, &le) && le != nil {
		return le, true
	}
	return nil, false
}
