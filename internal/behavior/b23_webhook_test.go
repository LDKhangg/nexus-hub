package behavior_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	delivery "github.com/LDKhangg/nexus-hub/internal/delivery/http"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/labstack/echo/v4"
)

const b23IntegrationID = "019458ab-1234-7000-8000-000000000001"

type b23Lookup struct{}

func (b23Lookup) LookupWebhookIntegration(context.Context, string) (tenant.WebhookIntegration, error) {
	return tenant.WebhookIntegration{ID: b23IntegrationID, WorkspaceID: "ws", Platform: "telegram", Role: tenant.IntegrationRoleControl, IsEnabled: true}, nil
}

type b23Secrets struct{ t *testing.T }

func (s b23Secrets) Resolve(_ context.Context, ws, ref string) (string, error) {
	s.t.Helper()
	if ws != "ws" || ref != behavior.CredentialTelegramWebhookSecret {
		s.t.Fatal("wrong credential scope")
	}
	return "fake-webhook-secret", nil
}

type b23Inbox struct {
	behavior.InboxStore
	rows []behavior.InboundUpdate
	seen map[int64]bool
}

func (s *b23Inbox) RecordUpdate(_ context.Context, ws string, u behavior.InboundUpdate) (bool, error) {
	if ws != "ws" {
		return false, behavior.ErrInvalid
	}
	if s.seen[u.UpdateID] {
		return false, nil
	}
	s.seen[u.UpdateID] = true
	s.rows = append(s.rows, u)
	return true, nil
}

type b23Stager struct {
	calls   int
	answer  string
	message int64
}

func (s *b23Stager) StageAnswer(_ context.Context, _ string, msg int64, answer string, _ time.Time) (string, error) {
	s.calls++
	s.answer = answer
	s.message = msg
	return "abcdefghijklmnop", nil
}

func b23Webhook(t *testing.T, h *delivery.TelegramHandler, secret, body string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/webhook/telegram/"+b23IntegrationID, strings.NewReader(body))
	req.Header.Set(delivery.TelegramSecretHeader, secret)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("integrationID")
	c.SetParamValues(b23IntegrationID)
	start := time.Now()
	if err := h.Handle(c); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("inbound acknowledgement exceeded REQ-5 two-second bound with immediate fakes")
	}
	return rec
}
func b23Message(id, chat, user int64, text string, reply int64) string {
	m := map[string]any{"message_id": 91, "chat": map[string]any{"id": chat}, "from": map[string]any{"id": user}, "text": text}
	if reply > 0 {
		m["reply_to_message"] = map[string]any{"message_id": reply}
	}
	raw, _ := json.Marshal(map[string]any{"update_id": id, "message": m})
	return string(raw)
}

// P3-03 (REQ-5): a wrong secret and a non-owner have no stored or staged effect;
// accepted commands are normalized once without calling a real bot.
func TestB23WebhookRejectsSecretAndOwnerBypassAndDeduplicatesCommand(t *testing.T) {
	s := &b23Inbox{seen: map[int64]bool{}}
	stager := &b23Stager{}
	h := delivery.NewTelegramHandler(b23Lookup{}, b23Secrets{t}, s, stager, 42)
	for _, secret := range []string{"", "wrong-secret"} {
		rec := b23Webhook(t, h, secret, b23Message(1, 42, 42, "/review abc", 0))
		if rec.Code != http.StatusUnauthorized || len(s.rows) != 0 || stager.calls != 0 {
			t.Fatal("unauthorized update had an effect")
		}
	}
	for _, ids := range [][2]int64{{99, 42}, {42, 99}, {-100, 99}} {
		rec := b23Webhook(t, h, "fake-webhook-secret", b23Message(2, ids[0], ids[1], "private answer", 123))
		if rec.Code != http.StatusOK || len(s.rows) != 0 || stager.calls != 0 {
			t.Fatal("foreign chat/sender staged content or got rejected rather than ignored")
		}
	}
	for i := 0; i < 2; i++ {
		rec := b23Webhook(t, h, "fake-webhook-secret", b23Message(3, 42, 42, "  /REVIEW@OwnerBot  abc  ", 0))
		if rec.Code != http.StatusOK {
			t.Fatal("valid owner command rejected")
		}
	}
	if len(s.rows) != 1 || s.rows[0].Command != "/review" || s.rows[0].Args != "abc" || s.rows[0].Kind != behavior.UpdateCommand || s.rows[0].IntegrationID != b23IntegrationID {
		t.Fatal("command was not normalized once")
	}
}

// P3-03 (REQ-5): preserve the current legitimate owner-in-group gate; a group member cannot press the owner's buttons.
func TestB23WebhookGroupRequiresOwnerSenderForMessagesAndCallbacks(t *testing.T) {
	s := &b23Inbox{seen: map[int64]bool{}}
	h := delivery.NewTelegramHandler(b23Lookup{}, b23Secrets{t}, s, &b23Stager{}, -100).WithOwnerUser(42)
	for _, user := range []int64{99, 42} {
		rec := b23Webhook(t, h, "fake-webhook-secret", b23Message(user, -100, user, "/help", 0))
		if rec.Code != 200 {
			t.Fatal("group update failed")
		}
		callback := fmt.Sprintf(`{"update_id":%d,"callback_query":{"id":"cb1","from":{"id":%d},"message":{"message_id":123,"chat":{"id":-100}},"data":"a:abcdefghijklmnop"}}`, user+100, user)
		if rec := b23Webhook(t, h, "fake-webhook-secret", callback); rec.Code != 200 {
			t.Fatal("callback failed")
		}
	}
	if len(s.rows) != 2 || s.rows[0].UpdateID != 42 || s.rows[1].UpdateID != 142 || s.rows[1].Kind != behavior.UpdateCallback {
		t.Fatal("group member impersonated owner or owner was refused")
	}
}

// P3-03 (REQ-5, REQ-14): reply text is staged only for its bound prompt and not copied into the inbox;
// overlong Unicode replies/arguments produce note codes, not retained payloads.
func TestB23WebhookStagesBoundReplyAndDropsOversizedPayloads(t *testing.T) {
	s := &b23Inbox{seen: map[int64]bool{}}
	answers := &b23Stager{}
	h := delivery.NewTelegramHandler(b23Lookup{}, b23Secrets{t}, s, answers, 42)
	if rec := b23Webhook(t, h, "fake-webhook-secret", b23Message(1, 42, 42, "Câu trả lời ✅", 123)); rec.Code != 200 {
		t.Fatal("reply rejected")
	}
	if answers.calls != 1 || answers.message != 123 || answers.answer != "Câu trả lời ✅" || len(s.rows) != 1 || s.rows[0].Args != "abcdefghijklmnop" || strings.Contains(s.rows[0].Command, "Câu") {
		t.Fatal("reply binding/content retention incorrect")
	}
	for i, text := range []string{strings.Repeat("🙂", 4001), "/review " + strings.Repeat("đ", 513)} {
		if rec := b23Webhook(t, h, "fake-webhook-secret", b23Message(int64(i+2), 42, 42, text, 123)); rec.Code != 200 {
			t.Fatal("overlong input not acknowledged")
		}
	}
	if answers.calls != 1 || s.rows[1].Note != behavior.NoteTextTooLong || s.rows[2].Note != behavior.NoteArgsTooLong || s.rows[1].Args != "" || s.rows[2].Args != "" {
		t.Fatal("overlong user content retained or staged")
	}
}

// P3-03 (REQ-5): dedupe must cover FREE-TEXT effects too, not only command rows.
// A duplicate reply must not overwrite staged answer state a second time.
func TestB23WebhookDuplicateReplyHasOneStagingEffect(t *testing.T) {
	s := &b23Inbox{seen: map[int64]bool{}}
	answers := &b23Stager{}
	h := delivery.NewTelegramHandler(b23Lookup{}, b23Secrets{t}, s, answers, 42)
	for i := 0; i < 2; i++ {
		if rec := b23Webhook(t, h, "fake-webhook-secret", b23Message(1, 42, 42, "One answer", 123)); rec.Code != 200 {
			t.Fatal("reply failed")
		}
	}
	if len(s.rows) != 1 || answers.calls != 1 {
		t.Fatalf("duplicate reply effects: inbox rows=%d, answer staging calls=%d; want one each", len(s.rows), answers.calls)
	}
}
