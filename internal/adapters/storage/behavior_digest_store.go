package storage

import (
	"context"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/storage/sqlc"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/jackc/pgx/v5/pgtype"
)

// This file holds the BehaviorStore part for queued digest and FYI
// messages (behavior.DigestStore).

var _ behavior.DigestStore = (*BehaviorStore)(nil)

func digestKey(key string) bool {
	return key == behavior.DigestKeyNotify || key == behavior.DigestKeyFYI
}

// QueueDigest claims the run's message key and queues the message in one
// transaction; false when the key was claimed before (sent or queued).
func (s *BehaviorStore) QueueDigest(ctx context.Context, workspaceID string, runID int64, key string, chat behavior.ChatRef, due time.Time) (bool, error) {
	ws, err := parseDBID(workspaceID)
	if err != nil {
		return false, err
	}
	integration, err := parseDBID(chat.IntegrationID)
	if err != nil || !digestKey(key) || runID <= 0 || chat.ChatID == 0 || due.IsZero() || chat.WorkspaceID != workspaceID {
		return false, invalid("digest entry")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	if _, err := q.ClaimBehaviorMessage(ctx, sqlc.ClaimBehaviorMessageParams{WorkspaceID: ws, RunID: runID, MsgKey: key}); err != nil {
		if isNoRows(err) {
			return false, nil // claimed before; the run must exist for the claim
		}
		return false, err
	}
	if _, err := q.QueueDigestEntry(ctx, sqlc.QueueDigestEntryParams{
		MsgKey: key, IntegrationID: integration, ChatID: chat.ChatID, DueAt: ts(due), WorkspaceID: ws, RunID: runID,
	}); err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, err
	}
	return true, tx.Commit(ctx)
}

// ClaimDigestBatch batches the due entries of the chat with the oldest due
// entry; nil when none is due.
func (s *BehaviorStore) ClaimDigestBatch(ctx context.Context, now time.Time, limit int) (*behavior.DigestBatch, error) {
	tok, token, err := newToken()
	if err != nil {
		return nil, err
	}
	at := ts(now)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	chat, err := q.SelectDueDigestChat(ctx, at)
	if isNoRows(err) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	id, err := q.InsertDigestBatch(ctx, sqlc.InsertDigestBatchParams{
		WorkspaceID: chat.WorkspaceID, IntegrationID: chat.IntegrationID, ChatID: chat.ChatID, Token: token,
	})
	if err != nil {
		return nil, err
	}
	rows, err := q.AssignDigestEntries(ctx, sqlc.AssignDigestEntriesParams{
		BatchID: id, WorkspaceID: chat.WorkspaceID, IntegrationID: chat.IntegrationID, ChatID: chat.ChatID,
		AtTime: at, MaxRows: int32(clamp(limit, 1, 200)),
	})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	b := &behavior.DigestBatch{
		ID: id, ClaimToken: tok,
		Chat: behavior.ChatRef{WorkspaceID: uuidString(chat.WorkspaceID), IntegrationID: uuidString(chat.IntegrationID), ChatID: chat.ChatID},
	}
	for _, r := range rows {
		b.Entries = append(b.Entries, behavior.DigestEntry{RunID: r.RunID, Key: r.MsgKey})
	}
	return b, nil
}

// FinishDigestBatch records the batch's message once, on the batch and on
// each entry's run message key.
func (s *BehaviorStore) FinishDigestBatch(ctx context.Context, batchID int64, token string, messageID int64) error {
	t, err := claimToken(token)
	if err != nil {
		return err
	}
	if messageID <= 0 {
		return invalid("message")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	n, err := q.FinishDigestBatch(ctx, sqlc.FinishDigestBatchParams{MessageID: messageID, ID: batchID, Token: t})
	if err != nil {
		return err
	}
	if n == 0 {
		return behavior.ErrStaleClaim
	}
	if _, err := q.SetDigestBatchMessages(ctx, sqlc.SetDigestBatchMessagesParams{MessageID: messageID, BatchID: batchID}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReleaseDigestBatch returns an unsent batch's entries to the queue (due
// again at due when it is not zero) and drops the batch.
func (s *BehaviorStore) ReleaseDigestBatch(ctx context.Context, batchID int64, token string, due time.Time) error {
	t, err := claimToken(token)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)
	if _, err := q.LockDigestBatch(ctx, sqlc.LockDigestBatchParams{ID: batchID, Token: t}); err != nil {
		if isNoRows(err) {
			return behavior.ErrStaleClaim
		}
		return err
	}
	var d pgtype.Timestamptz
	if !due.IsZero() {
		d = ts(due)
	}
	if _, err := q.ReleaseDigestEntries(ctx, sqlc.ReleaseDigestEntriesParams{DueAt: d, BatchID: batchID}); err != nil {
		return err
	}
	if _, err := q.DeleteDigestBatch(ctx, batchID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
