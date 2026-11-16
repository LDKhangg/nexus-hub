package workflow

import (
	"context"
	"fmt"
	"time"
)

// Conversation identifies one workspace chat without retaining its messages.
type Conversation struct {
	ID               int64
	WorkspaceID      string
	IntegrationID    string
	ChatID           int64
	ThreadID         int64 // Telegram topic; 0 is the chat itself (General)
	UserID           string
	Summary          string
	SummaryUpdatedAt *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Message records message routing metadata. It deliberately has no body field.
type Message struct {
	ID                int64
	WorkspaceID       string
	ConversationID    int64
	Direction         string
	Kind              string
	TelegramMessageID int64
	ReplyToMessageID  int64
	ExecutionID       int64
	CreatedAt         time.Time
}

// ResourceBinding identifies an external resource related to an execution.
type ResourceBinding struct {
	WorkspaceID string
	ExecutionID int64
	Kind        string
	ExternalID  string
}

// TelegramMessageRef scopes a Telegram message to the integration that owns
// the chat. Different bots may receive the same chat and message IDs.
func TelegramMessageRef(integrationID string, chatID, messageID int64) string {
	return fmt.Sprintf("%s:%d:%d", integrationID, chatID, messageID)
}

// Instruction is one immutable, explicitly supplied workflow instruction.
type Instruction struct {
	WorkspaceID      string
	ExecutionID      int64
	Version          int64
	Text             string
	ConversationID   int64
	TelegramUpdateID int64
	CreatedAt        time.Time
}

// InstructionRequest carries the source identifiers for an instruction.
type InstructionRequest struct {
	WorkspaceID      string
	ExecutionID      int64
	Text             string
	ConversationID   int64
	TelegramUpdateID int64
}

// ConversationStore persists conversation metadata and workflow associations.
type ConversationStore interface {
	EnsureConversation(context.Context, Conversation) (Conversation, error)
	RecordMessage(context.Context, Message) (Message, error)
	BindWorkflow(context.Context, string, int64, int64, string) error
	ListBoundWorkflows(context.Context, string, int64) ([]int64, error)
	SetSummary(context.Context, string, int64, string) error
	GetSummary(context.Context, string, int64) (string, error)
	BindResource(context.Context, ResourceBinding) error
	ExecutionsByResource(context.Context, string, string, string) ([]int64, error)
	AppendInstruction(context.Context, InstructionRequest) (Instruction, error)
	ListInstructions(context.Context, string, int64) ([]Instruction, error)
}

// ResourceLister is the optional read side used by operator status views.
// It is separate from ConversationStore so lightweight callers need not
// implement a query they do not use.
type ResourceLister interface {
	ListResources(context.Context, string, int64) ([]ResourceBinding, error)
}
