package http

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
)

// Phase 9 Batch B (REQ-10): a button press by someone other than the owner is
// ignored and answered with a short toast in the webhook reply; the owner's
// press of the same button is stored.

func p9Press(id int64, chat, from int64, data string) string {
	return `{"update_id":` + itoa(id) + `,"callback_query":{"id":"cb-` + itoa(id) + `","from":{"id":` + itoa(from) +
		`},"message":{"message_id":9,"chat":{"id":` + itoa(chat) + `}},"data":"` + data + `"}}`
}

// TC-PR-4 (REQ-10, abuse): a stranger's press of Duyệt answers a toast and stores nothing; the owner's press is queued.
func TestP9Handler_NonOwnerPressGetsToastAndIsIgnored(t *testing.T) {
	data := string(behavior.CallbackApprove) + ":" + strings.Repeat("a", 22)
	inbox := &telegramInboxFake{}
	h := NewTelegramHandler(p7Lookup(), testResolver{value: "webhook-secret", ok: true}, inbox, answerFake{}, 77)

	rec := p7Post(t, h, "webhook-secret", []byte(p9Press(901, 77, 88, data))) // owner chat, other sender
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	body := p7Body(t, rec)
	if body["status"] != UpdateIgnored || body["method"] != "answerCallbackQuery" || body["callback_query_id"] != "cb-901" || body["text"] != msgNotOwnerPress {
		t.Fatalf("reply %v", body)
	}
	if strings.Contains(rec.Body.String(), strings.Repeat("a", 22)) {
		t.Fatal("the reply echoes the button data")
	}
	// A press from a chat that is not the owner's is the same.
	rec = p7Post(t, h, "webhook-secret", []byte(p9Press(902, 99, 99, data)))
	if body := p7Body(t, rec); body["method"] != "answerCallbackQuery" || body["callback_query_id"] != "cb-902" {
		t.Fatalf("other chat reply %v", body)
	}
	if len(inbox.updates) != 0 {
		t.Fatalf("a press by someone else was stored: %+v", inbox.updates)
	}
	// A plain message by a stranger gets no toast (nothing to answer).
	rec = p7Post(t, h, "webhook-secret", []byte(p7OtherChat))
	if body := p7Body(t, rec); body["status"] != UpdateIgnored || body["method"] != "" {
		t.Fatalf("message reply %v", body)
	}
	// A callback id that Telegram could not have sent gets no toast.
	rec = p7Post(t, h, "webhook-secret", []byte(strings.Replace(p9Press(903, 77, 88, data), `"cb-903"`, `"cb 903;x"`, 1)))
	if body := p7Body(t, rec); body["method"] != "" {
		t.Fatalf("bad callback id reply %v", body)
	}
	// The owner's own press is queued as it came.
	rec = p7Post(t, h, "webhook-secret", []byte(p9Press(904, 77, 77, data)))
	if body := p7Body(t, rec); body["status"] != UpdateQueued || body["method"] != "" {
		t.Fatalf("owner reply %v", body)
	}
	if len(inbox.updates) != 1 || inbox.updates[0].Kind != behavior.UpdateCallback || inbox.updates[0].Command != data {
		t.Fatalf("stored %+v", inbox.updates)
	}
}
