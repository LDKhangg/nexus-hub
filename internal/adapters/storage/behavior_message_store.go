package storage

import (
	"context"
	"regexp"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// This file holds the BehaviorStore part for the engine's Telegram message
// log (behavior.MessageLog).

var messageKeyRe = regexp.MustCompile(`^[a-z0-9_:-]{1,100}$`)

var _ behavior.MessageLog = (*BehaviorStore)(nil)

// ClaimMessage claims key for the run once. A repeat returns claimed=false
// with the stored message ID (0 when the earlier send never finished).
func (s *BehaviorStore) ClaimMessage(ctx context.Context, workspaceID string, runID int64, key string) (bool, int64, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return false, 0, err
	}
	if !messageKeyRe.MatchString(key) || runID <= 0 {
		return false, 0, invalid("message key")
	}
	_, err = s.q.ClaimBehaviorMessage(ctx, sqlc.ClaimBehaviorMessageParams{WorkspaceID: ws, RunID: runID, MsgKey: key})
	if err == nil {
		return true, 0, nil
	}
	if !isNoRows(err) {
		return false, 0, err
	}
	row, err := s.q.GetBehaviorMessage(ctx, sqlc.GetBehaviorMessageParams{WorkspaceID: ws, RunID: runID, MsgKey: key})
	if err != nil {
		return false, 0, notFound(err)
	}
	return false, row.TelegramMessageID.Int64, nil
}

// SetMessageID records the Telegram message ID of a claimed key once.
func (s *BehaviorStore) SetMessageID(ctx context.Context, workspaceID string, runID int64, key string, messageID int64) error {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return err
	}
	if !messageKeyRe.MatchString(key) || messageID <= 0 {
		return invalid("message")
	}
	_, err = s.q.SetBehaviorMessageSent(ctx, sqlc.SetBehaviorMessageSentParams{MessageID: messageID, WorkspaceID: ws, RunID: runID, MsgKey: key})
	return err
}

// ReleaseMessage drops an unsent claim so the send may be tried again.
func (s *BehaviorStore) ReleaseMessage(ctx context.Context, workspaceID string, runID int64, key string) error {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return err
	}
	if !messageKeyRe.MatchString(key) {
		return invalid("message key")
	}
	_, err = s.q.ReleaseBehaviorMessage(ctx, sqlc.ReleaseBehaviorMessageParams{WorkspaceID: ws, RunID: runID, MsgKey: key})
	return err
}
