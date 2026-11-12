package telegram

import (
	"context"
	"unicode/utf16"

	"github.com/LDKhangg/nexus-hub/internal/relay"
)

// MaxTextUnits is Telegram's message limit, kept with a little headroom.
const MaxTextUnits = 4000

// SendText sends plain text (no parse mode) as the workspace bot. The text
// is clipped to the Telegram limit; the token is resolved per call.
func (c *Client) SendText(ctx context.Context, workspaceID, tokenRef, chatID string, threadID *int64, text string) (int64, error) {
	if c.resolver == nil {
		return 0, &relay.DeliveryError{Code: "missing_token"}
	}
	token, err := c.resolver.Resolve(ctx, workspaceID, tokenRef)
	if err != nil || token == "" {
		return 0, &relay.DeliveryError{Code: secretCode(err)}
	}
	if units(text) > MaxTextUnits {
		text = clip(text, MaxTextUnits-12) + " [truncated]"
	}
	r, err := c.call(ctx, token, chatID, threadID, text)
	return r.MessageID, err
}

func units(s string) int { return len(utf16.Encode([]rune(s))) }
