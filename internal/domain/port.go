package domain

import (
	"context"
)

type SourcePort interface {
	Platform() ChannelSource
	Acknowledge(ctx context.Context, externalID string) error
	PostReply(ctx context.Context, externalID string, message string) error
	UpdateState(ctx context.Context, externalID string, newState string) error
}

type ControlPlanePort interface {
	EnsureThread(ctx context.Context, event NormalizedEvent) (string, error)
	NotifyHuman(ctx context.Context, controlThreadID string, summary string, drafter string) error
}

// type interface cho LLM
type LLMProvider interface {
	Generate(ctx context.Context, req *LLMRequest) (*LLMResponse, error)
}

// ToolCallingProvider runs one tool-calling step.
type ToolCallingProvider interface {
	GenerateWithTools(ctx context.Context, req *ToolRequest) (*ToolResponse, error)
}
