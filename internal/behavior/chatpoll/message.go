package chatpoll

import (
	"context"
	"errors"

	"github.com/LDKhangg/nexus-hub/internal/adapters/sources/clickupchat"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// DefaultReaderPages is how many pages of a channel's messages the
// MessageReader reads to find one message.
const DefaultReaderPages = 5

// MessageReader implements behavior.ChatMessageReader: it reads the text
// of one ClickUp Chat message for a behavior run's model step. ClickUp
// Chat v3 has no endpoint that returns one message by ID, so it reads the
// channel's message pages (newest first in practice; every page is
// checked) and picks the message by ID. A run reads its message soon after
// the poller saw it, so the first page almost always holds it. The text is
// returned to the run executor only; it is never logged or put in an
// error.
type MessageReader struct {
	Chat    ChatAPI
	Secrets Secrets
	// Pages caps the message pages read; 0 means DefaultReaderPages.
	Pages int
}

var _ behavior.ChatMessageReader = (*MessageReader)(nil)

// ChatMessageText implements behavior.ChatMessageReader.
func (r *MessageReader) ChatMessageText(ctx context.Context, workspaceID, tokenRef, teamID, channelID, messageID string) (string, error) {
	if r == nil || r.Chat == nil || r.Secrets == nil {
		return "", &behavior.ChatReadError{Code: behavior.NoteNotImplemented}
	}
	if tokenRef == "" {
		return "", &behavior.ChatReadError{Code: clickupchat.CodeMissingCredentials}
	}
	token, err := r.Secrets.Resolve(ctx, workspaceID, tokenRef)
	if err != nil || token == "" {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", &behavior.ChatReadError{Code: clickupchat.CodeMissingCredentials}
	}
	pages := r.Pages
	if pages <= 0 {
		pages = DefaultReaderPages
	}
	cursor := ""
	for page := 0; page < pages; page++ {
		pg, err := r.Chat.Messages(ctx, teamID, token, channelID, cursor)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			var ce *clickupchat.Error
			if errors.As(err, &ce) {
				return "", &behavior.ChatReadError{Code: ce.Code, Transient: ce.Retryable}
			}
			return "", &behavior.ChatReadError{Code: clickupchat.CodeUnavailable, Transient: true}
		}
		for _, m := range pg.Items {
			if m.ID != messageID {
				continue
			}
			if m.ChannelID != "" && m.ChannelID != channelID {
				return "", &behavior.ChatReadError{Code: clickupchat.CodeMessageNotFound}
			}
			return m.Content, nil
		}
		if pg.NextCursor == "" || pg.NextCursor == cursor {
			break
		}
		cursor = pg.NextCursor
	}
	return "", &behavior.ChatReadError{Code: clickupchat.CodeMessageNotFound}
}
