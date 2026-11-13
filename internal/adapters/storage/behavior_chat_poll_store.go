package storage

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/jackc/pgx/v5/pgconn"
)

// This file holds the BehaviorStore part for the ClickUp Chat poll cursor
// and error state (behavior.ChatPollStore).

var _ behavior.ChatPollStore = (*BehaviorStore)(nil)

var chatPollCodeRe = regexp.MustCompile(`^[a-z_]{1,64}$`)

func mapChatPoll(r sqlc.ClickupChatPoll) behavior.ChatPollState {
	return behavior.ChatPollState{
		WorkspaceID: uuidString(r.WorkspaceID), IntegrationID: uuidString(r.IntegrationID), TeamID: r.ClickupTeamID,
		CursorMS: r.CursorMs, InitializedAt: r.InitializedAt.Time, ErrorStreak: int(r.ErrorStreak),
		UnauthorizedStreak: int(r.UnauthorizedStreak), LastErrorCode: r.LastErrorCode.String,
		BackoffUntil: tsPtr(r.BackoffUntil), UnauthorizedNotifiedAt: tsPtr(r.UnauthorizedNotifiedAt),
		UpdatedAt: r.UpdatedAt.Time,
	}
}

// isForeignKeyViolation reports a missing referenced row, here an unknown
// integration or one of another workspace.
func isForeignKeyViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23503"
}

func validTeamID(teamID string) bool {
	return strings.TrimSpace(teamID) != "" && len(teamID) <= 64
}

// InitChatPoll creates or restarts the poll state at nowMS, or returns the
// stored state unchanged; see behavior.ChatPollStore.
func (s *BehaviorStore) InitChatPoll(ctx context.Context, workspaceID, integrationID, teamID string, nowMS int64) (behavior.ChatPollState, bool, error) {
	ws, integration, err := parseScopedIDs(workspaceID, integrationID)
	if err != nil {
		return behavior.ChatPollState{}, false, err
	}
	if !validTeamID(teamID) {
		return behavior.ChatPollState{}, false, invalid("team_id")
	}
	if nowMS < 0 {
		return behavior.ChatPollState{}, false, invalid("cursor")
	}
	row, err := s.q.InitChatPoll(ctx, sqlc.InitChatPollParams{
		WorkspaceID: ws, IntegrationID: integration, ClickupTeamID: teamID, CursorMs: nowMS,
	})
	if err == nil {
		return mapChatPoll(row), true, nil
	}
	if isForeignKeyViolation(err) {
		return behavior.ChatPollState{}, false, behavior.ErrNotFound
	}
	if !isNoRows(err) {
		return behavior.ChatPollState{}, false, err
	}
	row, err = s.q.GetChatPoll(ctx, sqlc.GetChatPollParams{WorkspaceID: ws, IntegrationID: integration})
	if err != nil {
		return behavior.ChatPollState{}, false, notFound(err)
	}
	return mapChatPoll(row), false, nil
}

// GetChatPoll returns the stored poll state.
func (s *BehaviorStore) GetChatPoll(ctx context.Context, workspaceID, integrationID string) (behavior.ChatPollState, error) {
	ws, integration, err := parseScopedIDs(workspaceID, integrationID)
	if err != nil {
		return behavior.ChatPollState{}, err
	}
	row, err := s.q.GetChatPoll(ctx, sqlc.GetChatPollParams{WorkspaceID: ws, IntegrationID: integration})
	if err != nil {
		return behavior.ChatPollState{}, notFound(err)
	}
	return mapChatPoll(row), nil
}

// AdvanceChatCursor moves the cursor forward only and returns the stored
// cursor.
func (s *BehaviorStore) AdvanceChatCursor(ctx context.Context, workspaceID, integrationID, teamID string, cursorMS int64) (int64, error) {
	ws, integration, err := parseScopedIDs(workspaceID, integrationID)
	if err != nil {
		return 0, err
	}
	if !validTeamID(teamID) {
		return 0, invalid("team_id")
	}
	if cursorMS < 0 {
		return 0, invalid("cursor")
	}
	cur, err := s.q.AdvanceChatCursor(ctx, sqlc.AdvanceChatCursorParams{
		CursorMs: cursorMS, WorkspaceID: ws, IntegrationID: integration, ClickupTeamID: teamID,
	})
	if err != nil {
		return 0, notFound(err)
	}
	return cur, nil
}

// RecordChatPollError counts one failed poll and sets the doubling backoff.
func (s *BehaviorStore) RecordChatPollError(ctx context.Context, workspaceID, integrationID, code string, now time.Time, base, max time.Duration) (behavior.ChatPollState, error) {
	ws, integration, err := parseScopedIDs(workspaceID, integrationID)
	if err != nil {
		return behavior.ChatPollState{}, err
	}
	switch {
	case !chatPollCodeRe.MatchString(code):
		return behavior.ChatPollState{}, invalid("code")
	case now.IsZero():
		return behavior.ChatPollState{}, invalid("now")
	case base <= 0 || max < base:
		return behavior.ChatPollState{}, invalid("backoff")
	}
	row, err := s.q.RecordChatPollError(ctx, sqlc.RecordChatPollErrorParams{
		Code: code, AtTime: ts(now), MaxSecs: max.Seconds(), BaseSecs: base.Seconds(),
		WorkspaceID: ws, IntegrationID: integration,
	})
	if err != nil {
		return behavior.ChatPollState{}, notFound(err)
	}
	return mapChatPoll(row), nil
}

// ResetChatPollErrors clears the error state; a state without errors is
// left as it is.
func (s *BehaviorStore) ResetChatPollErrors(ctx context.Context, workspaceID, integrationID string) error {
	ws, integration, err := parseScopedIDs(workspaceID, integrationID)
	if err != nil {
		return err
	}
	return s.q.ResetChatPollErrors(ctx, sqlc.ResetChatPollErrorsParams{WorkspaceID: ws, IntegrationID: integration})
}

// ClaimChatUnauthorizedNotice claims the owner notice of the current
// unauthorized streak once.
func (s *BehaviorStore) ClaimChatUnauthorizedNotice(ctx context.Context, workspaceID, integrationID string, minStreak int, now time.Time) (bool, error) {
	ws, integration, err := parseScopedIDs(workspaceID, integrationID)
	if err != nil {
		return false, err
	}
	if minStreak < 1 || now.IsZero() {
		return false, invalid("notice")
	}
	n, err := s.q.ClaimChatUnauthorizedNotice(ctx, sqlc.ClaimChatUnauthorizedNoticeParams{
		AtTime: ts(now), WorkspaceID: ws, IntegrationID: integration, MinStreak: int32(clamp(minStreak, 1, 1000)),
	})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
