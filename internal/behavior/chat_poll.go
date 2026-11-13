package behavior

import (
	"context"
	"time"
)

// ChatPollState is the stored ClickUp Chat poll state of one ClickUp source
// integration: identifiers, the cursor, counters and times only.
type ChatPollState struct {
	WorkspaceID   string
	IntegrationID string
	// TeamID is the ClickUp workspace the cursor belongs to.
	TeamID string
	// CursorMS is the newest message date (Unix ms) fully processed.
	CursorMS      int64
	InitializedAt time.Time
	// ErrorStreak counts consecutive failed polls of any code.
	ErrorStreak int
	// UnauthorizedStreak counts consecutive chat_unauthorized results.
	UnauthorizedStreak int
	LastErrorCode      string
	// BackoffUntil is when the next poll may run; nil when not backing off.
	BackoffUntil *time.Time
	// UnauthorizedNotifiedAt is set once the owner was told about the
	// current unauthorized streak.
	UnauthorizedNotifiedAt *time.Time
	UpdatedAt              time.Time
}

// ChatPollStore persists the ClickUp Chat poller's cursor and error state,
// one row per ClickUp source integration. Message deduplication is not
// kept here: it is the run dedupe of RunStore.CreateRun.
type ChatPollStore interface {
	// InitChatPoll returns the poll state, creating it with the cursor at
	// nowMS when none exists, or starting it again at nowMS when the stored
	// state belongs to another ClickUp workspace. fresh is true when the
	// cursor was (re)started by this call: the caller must not process
	// older messages then.
	InitChatPoll(ctx context.Context, workspaceID, integrationID, teamID string, nowMS int64) (state ChatPollState, fresh bool, err error)
	// GetChatPoll returns the poll state; ErrNotFound when none exists.
	GetChatPoll(ctx context.Context, workspaceID, integrationID string) (ChatPollState, error)
	// AdvanceChatCursor moves the cursor forward to cursorMS and returns the
	// stored cursor; it never moves backwards. ErrNotFound when no state
	// exists for teamID (for example it was started again for another
	// ClickUp workspace).
	AdvanceChatCursor(ctx context.Context, workspaceID, integrationID, teamID string, cursorMS int64) (int64, error)
	// RecordChatPollError counts one failed poll with code and sets the
	// backoff to base doubled for each earlier consecutive error, capped at
	// max, from now. A code other than chat_unauthorized ends the
	// unauthorized streak. ErrNotFound when no state exists.
	RecordChatPollError(ctx context.Context, workspaceID, integrationID, code string, now time.Time, base, max time.Duration) (ChatPollState, error)
	// ResetChatPollErrors clears the error state after a successful poll.
	ResetChatPollErrors(ctx context.Context, workspaceID, integrationID string) error
	// ClaimChatUnauthorizedNotice claims the one owner notice of the
	// current unauthorized streak; true only once per streak and only when
	// the streak reached minStreak.
	ClaimChatUnauthorizedNotice(ctx context.Context, workspaceID, integrationID string, minStreak int, now time.Time) (bool, error)
}

// ChatRunStarter turns ClickUp Chat message events into behavior runs. The
// chat poller calls it for each batch of new messages before it advances
// its cursor past them.
type ChatRunStarter interface {
	// StartChatRuns matches each TriggerChatMessage event (built with
	// ChatSubject(MessageID) as Key and ExtractPRRefs for PRs) against the
	// enabled behavior versions, with the owner's ClickUp user ID from
	// settings, and stores the runs in one transaction. Each run's subject
	// and revision are ChatSubject(MessageID), so a message re-read after a
	// restart starts no second run of the same version. created counts new
	// runs only.
	StartChatRuns(ctx context.Context, workspaceID string, events []Event) (created int, err error)
}
