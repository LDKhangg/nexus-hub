package http

import (
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// Phase 10 Batch A, W1 (REQ-7) and TD-NT-1: only the owner's presses and replies reach the worker. A press on
// a ClickUp draft's buttons, an edit reply to its preview and the "open task topic" button from anyone else
// are dropped before anything is stored.

func p10Callback(id, chat, from int64, data string) string {
	return `{"update_id":` + itoa(id) + `,"callback_query":{"id":"cb` + itoa(id) + `","from":{"id":` + itoa(from) +
		`},"message":{"message_id":9,"chat":{"id":` + itoa(chat) + `}},"data":"` + data + `"}}`
}

func p10Reply(id, chat, from int64, text string) string {
	return `{"update_id":` + itoa(id) + `,"message":{"message_id":12,"chat":{"id":` + itoa(chat) + `},"from":{"id":` + itoa(from) +
		`},"reply_to_message":{"message_id":9},"text":"` + text + `"}}`
}

// TC-D4, TC-TD-NT-1 (REQ-7, abuse): a non-owner press on Đăng, Sửa, Huỷ or the topic button has no effect.
func TestP10Handler_NonOwnerPressHasNoEffect(t *testing.T) {
	const group, owner, stranger = int64(-100777), int64(42), int64(4242)
	nonce := strings.Repeat("a", 22)
	for name, data := range map[string]string{
		"Đăng":       string(behavior.CallbackApprove) + ":" + nonce,
		"Sửa":        string(behavior.CallbackFreeText) + ":" + nonce,
		"Huỷ":        string(behavior.CallbackReject) + ":" + nonce,
		"topic open": "tt:86abc",
	} {
		t.Run(name, func(t *testing.T) {
			h, inbox, in := p6bHandler(map[string]any{"owner_chat_id": float64(group)}, 77, owner)
			p6bPost(t, h, in, p10Callback(1, group, stranger, data))
			p6bPost(t, h, in, p10Callback(2, 77, owner, data)) // the owner in another chat
			if len(inbox.updates) != 0 {
				t.Fatalf("a press by someone else was stored: %+v", inbox.updates)
			}
			p6bPost(t, h, in, p10Callback(3, group, owner, data))
			if len(inbox.updates) != 1 || inbox.updates[0].Kind != behavior.UpdateCallback || inbox.updates[0].Command != data {
				t.Fatalf("the owner's press was not stored as is: %+v", inbox.updates)
			}
		})
	}
}

// TC-D4 (REQ-7, abuse): a non-owner reply to a draft preview (an edit) is dropped; the owner's reply is recorded.
func TestP10Handler_NonOwnerEditReplyHasNoEffect(t *testing.T) {
	const group, owner, stranger = int64(-100777), int64(42), int64(4242)
	h, inbox, in := p6bHandler(map[string]any{"owner_chat_id": float64(group)}, 77, owner)
	p6bPost(t, h, in, p10Reply(1, group, stranger, "nội dung của người lạ"))
	p6bPost(t, h, in, p10Reply(2, 77, owner, "nội dung từ chat khác"))
	if len(inbox.updates) != 0 {
		t.Fatalf("a reply by someone else was stored: %+v", inbox.updates)
	}
	p6bPost(t, h, in, p10Reply(3, group, owner, "nội dung mới"))
	if len(inbox.updates) != 1 || inbox.updates[0].ReplyToMessageID != 9 {
		t.Fatalf("the owner's reply: %+v", inbox.updates)
	}
}
