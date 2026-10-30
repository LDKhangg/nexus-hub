package domain

import "context"

type SourcePort interface {
	Platform() ChannelSource
	Acknowledge(ctx context.Context, externalID string) error
	PostReply(ctx context.Context, externalID string, message string) error
	UpdateState(ctx context.Context, externalID string, newState string) error
}

type ControlPlanePort interface {
	EnsureThread(ctx context.Context, event NormalizedEvent) (controlThreadID string, err error)
	NotifyHuman(ctx context.Context, controlThreadID string, summary string, drafter string) error
}

type ThreadRepository interface {
	GetControlThreadID(ctx context.Context, workspaceID string, source ChannelSource, externalID string) (string, error)
	SaveMapping(ctx context.Context, workspaceID string, source ChannelSource, externalID string, controlThreadID string) error
}
