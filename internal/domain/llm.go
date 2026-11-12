package domain

import "encoding/json"

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
