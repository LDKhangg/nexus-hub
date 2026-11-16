package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/labstack/echo/v4"
)

// TC-6A50 (REQ-8): "/deep <text>" stages the text in memory like free text;
// the stored update holds only the key; text over 4,000 runes is refused
// with text_too_long; without an assistant nothing is staged.
func TestTelegramWebhookPhase6aDeepStagesText(t *testing.T) {
	const ws = "019a6690-abcd-7000-8000-000000000001"
	integration := tenant.WebhookIntegration{ID: testIntegrationID, WorkspaceID: ws, Platform: "telegram", Role: tenant.IntegrationRoleControl, IsEnabled: true}
	lookup := lookupFunc(func(context.Context, string) (tenant.WebhookIntegration, error) { return integration, nil })
	post := func(t *testing.T, h *TelegramHandler, text string) {
		t.Helper()
		e := echo.New()
		e.POST("/webhook/telegram/:integrationID", h.Handle)
		body := `{"update_id":601,"message":{"message_id":9,"chat":{"id":77},"from":{"id":77},"text":"` + text + `"}}`
		req := httptest.NewRequest(http.MethodPost, "/webhook/telegram/"+testIntegrationID, strings.NewReader(body))
		req.Header.Set(TelegramSecretHeader, "webhook-secret")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
	}
	newHandler := func(withAssistant bool) (*TelegramHandler, *telegramInboxFake, *assistantStagerFake) {
		inbox, stager := &telegramInboxFake{}, &assistantStagerFake{}
		h := NewTelegramHandler(lookup, testResolver{value: "webhook-secret", ok: true}, inbox, noOpenAnswerFake{}, 77)
		if withAssistant {
			h.WithAssistant(stager)
		}
		return h, inbox, stager
	}
	const secret = "phân tích kiến trúc BÍ-MẬT 🙂"

	t.Run("text is staged, update has only the key", func(t *testing.T) {
		h, inbox, stager := newHandler(true)
		post(t, h, "/deep   "+secret+"  ")
		if len(inbox.updates) != 1 || len(stager.texts) != 1 || stager.texts[0] != secret || stager.ws[0] != ws {
			t.Fatalf("updates=%+v staged=%q", inbox.updates, stager.texts)
		}
		u := inbox.updates[0]
		if u.Kind != behavior.UpdateCommand || u.Command != "/deep" || u.Args != behavior.AssistantTextPrefix+"k0123456789abcdef" || u.Note != "" {
			t.Fatalf("update=%+v", u)
		}
		if strings.Contains(u.Args+u.Note, "BÍ-MẬT") {
			t.Fatalf("text stored in the update: %+v", u)
		}
	})

	t.Run("longer than 512 but within 4000 runes is staged", func(t *testing.T) {
		h, inbox, stager := newHandler(true)
		text := strings.Repeat("ả", 4000)
		post(t, h, "/deep "+text)
		if len(stager.texts) != 1 || stager.texts[0] != text || inbox.updates[0].Note != "" {
			t.Fatalf("note=%q staged=%d", inbox.updates[0].Note, len(stager.texts))
		}
	})

	t.Run("over 4000 runes is text_too_long, not staged", func(t *testing.T) {
		h, inbox, stager := newHandler(true)
		post(t, h, "/deep "+strings.Repeat("ả", 4001))
		if len(stager.texts) != 0 || len(inbox.updates) != 1 {
			t.Fatalf("staged=%d updates=%d", len(stager.texts), len(inbox.updates))
		}
		if u := inbox.updates[0]; u.Note != behavior.NoteTextTooLong || u.Args != "" || u.Command != "/deep" {
			t.Fatalf("update=%+v", u)
		}
	})

	t.Run("bare /deep has no args", func(t *testing.T) {
		h, inbox, stager := newHandler(true)
		post(t, h, "/deep")
		if len(stager.texts) != 0 || inbox.updates[0].Args != "" || inbox.updates[0].Command != "/deep" {
			t.Fatalf("update=%+v staged=%q", inbox.updates[0], stager.texts)
		}
	})

	t.Run("assistant off stores no text", func(t *testing.T) {
		h, inbox, _ := newHandler(false)
		post(t, h, "/deep "+secret)
		if len(inbox.updates) != 1 || inbox.updates[0].Args != "" || strings.Contains(inbox.updates[0].Note, "BÍ") {
			t.Fatalf("update=%+v", inbox.updates)
		}
	})
}
