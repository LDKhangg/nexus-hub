package storage

import (
	"context"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// This file holds the BehaviorStore part that trace and the run listing
// read (behavior.TraceStore, behavior.RunLister).

var (
	_ behavior.TraceStore = (*BehaviorStore)(nil)
	_ behavior.RunLister  = (*BehaviorStore)(nil)
)

// LatestRunByCorrelation returns the newest run with the correlation ID.
func (s *BehaviorStore) LatestRunByCorrelation(ctx context.Context, workspaceID, correlationID string) (behavior.Run, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return behavior.Run{}, err
	}
	corr, err := parseDBID(correlationID)
	if err != nil {
		return behavior.Run{}, invalid("correlation_id")
	}
	row, err := s.q.LatestBehaviorRunByCorrelation(ctx, sqlc.LatestBehaviorRunByCorrelationParams{WorkspaceID: ws, CorrelationID: corr})
	if err != nil {
		return behavior.Run{}, notFound(err)
	}
	return mapRun(row.BehaviorRun, row.BehaviorName), nil
}

// ListRunToolCalls returns the audit rows of a run without inputs or
// result text.
func (s *BehaviorStore) ListRunToolCalls(ctx context.Context, workspaceID string, runID int64) ([]behavior.ToolCallRecord, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListToolRunsForBehaviorRun(ctx, sqlc.ListToolRunsForBehaviorRunParams{WorkspaceID: ws, BehaviorRunID: runID})
	if err != nil {
		return nil, err
	}
	out := make([]behavior.ToolCallRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, behavior.ToolCallRecord{
			ID: r.ID, Step: int(r.Step), Tool: r.Tool, Tier: r.Tier, Status: r.Status, ErrorCode: r.ErrorCode,
			DurationMS: r.DurationMs, CreatedAt: r.CreatedAt.Time,
		})
	}
	return out, nil
}

// ListRecentRuns returns up to limit runs, newest first, optionally of one
// behavior, with item counts by class.
func (s *BehaviorStore) ListRecentRuns(ctx context.Context, workspaceID, behaviorName string, limit int) ([]behavior.RunSummary, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListRecentBehaviorRuns(ctx, sqlc.ListRecentBehaviorRunsParams{
		WorkspaceID: ws, BehaviorName: behaviorName, RowLimit: int32(clamp(limit, 1, behavior.MaxRunListLimit)),
	})
	if err != nil {
		return nil, err
	}
	out := make([]behavior.RunSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, behavior.RunSummary{
			ID: r.ID, Behavior: r.BehaviorName, Version: int(r.Version), TriggerKind: r.TriggerKind, Subject: r.Subject,
			Status: behavior.RunStatus(r.Status), ErrorCode: r.ErrorCode, CorrelationID: uuidString(r.CorrelationID),
			AutoItems: int(r.AutoItems), NotifyItems: int(r.NotifyItems), AskItems: int(r.AskItems), CreatedAt: r.CreatedAt.Time,
		})
	}
	return out, nil
}
