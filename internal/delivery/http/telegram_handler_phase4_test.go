package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/labstack/echo/v4"
)

type assistantStagerFake struct {
	mu    sync.Mutex
	texts []string
	ws    []string
}

func (f *assistantStagerFake) StageAssistantText(workspaceID, text string, _ time.Time) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.texts = append(f.texts, text)
	f.ws = append(f.ws, workspaceID)
	return "k0123456789abcdef", nil
}

type noOpenAnswerFake struct{}

func (noOpenAnswerFake) StageAnswer(context.Context, string, int64, string, time.Time) (string, error) {
	return "", behavior.ErrNotFound
}

// TC-29 (REQ-12) and TC-8 (REQ-4): only the owner's plain, non-command,
// non-reply text is handed to the assistant, in memory; the stored update
// carries an opaque key, never the text. Other senders are ignored.
func TestTelegramWebhookPhase4AssistantRouting(t *testing.T) {
	const ws = "019a6690-abcd-7000-8000-000000000001"
	integration := tenant.WebhookIntegration{ID: testIntegrationID, WorkspaceID: ws, Platform: "telegram", Role: tenant.IntegrationRoleControl, IsEnabled: true}
	lookup := lookupFunc(func(context.Context, string) (tenant.WebhookIntegration, error) { return integration, nil })
	post := func(t *testing.T, h *TelegramHandler, body string) *httptest.ResponseRecorder {
		t.Helper()
		e := echo.New()
		e.POST("/webhook/telegram/:integrationID", h.Handle)
		req := httptest.NewRequest(http.MethodPost, "/webhook/telegram/"+testIntegrationID, strings.NewReader(body))
		req.Header.Set(TelegramSecretHeader, "webhook-secret")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}
	newHandler := func(withAssistant bool) (*TelegramHandler, *telegramInboxFake, *assistantStagerFake) {
		inbox, stager := &telegramInboxFake{}, &assistantStagerFake{}
		h := NewTelegramHandler(lookup, testResolver{value: "webhook-secret", ok: true}, inbox, noOpenAnswerFake{}, 77)
		if withAssistant {
			h.WithAssistant(stager)
		}
		return h, inbox, stager
	}
	const secretText = "đặt tên clickup:12345 là chị Lan BÍ-MẬT"

	t.Run("TC-29 owner plain text is staged in memory only", func(t *testing.T) {
		h, inbox, stager := newHandler(true)
		rec := post(t, h, `{"update_id":201,"message":{"message_id":9,"chat":{"id":77},"from":{"id":77},"text":"  `+secretText+`  "}}`)
		if rec.Code != 200 || len(inbox.updates) != 1 {
			t.Fatalf("code=%d updates=%+v", rec.Code, inbox.updates)
		}
		u := inbox.updates[0]
		if u.Kind != behavior.UpdateText || u.Args != behavior.AssistantTextPrefix+"k0123456789abcdef" || u.Command != "" || u.Note != "" {
			t.Fatalf("stored update=%+v", u)
		}
		if strings.Contains(u.Args+u.Command+u.Note, "BÍ-MẬT") {
			t.Fatalf("text in stored update: %+v", u)
		}
		if len(stager.texts) != 1 || stager.texts[0] != secretText || stager.ws[0] != ws {
			t.Fatalf("staged=%q ws=%v", stager.texts, stager.ws)
		}
	})

	t.Run("TC-29 commands, replies and long text are not routed", func(t *testing.T) {
		h, inbox, stager := newHandler(true)
		post(t, h, `{"update_id":202,"message":{"message_id":10,"chat":{"id":77},"from":{"id":77},"text":"/contacts Lan"}}`)
		post(t, h, `{"update_id":203,"message":{"message_id":11,"chat":{"id":77},"from":{"id":77},"text":"tên mới","reply_to_message":{"message_id":5}}}`)
		post(t, h, `{"update_id":204,"message":{"message_id":12,"chat":{"id":77},"from":{"id":77},"text":"`+strings.Repeat("a", 4001)+`"}}`)
		post(t, h, `{"update_id":205,"message":{"message_id":13,"chat":{"id":77},"from":{"id":77},"text":"   "}}`)
		if len(stager.texts) != 0 {
			t.Fatalf("non-plain text staged: %q", stager.texts)
		}
		if len(inbox.updates) != 4 {
			t.Fatalf("updates=%+v", inbox.updates)
		}
		if u := inbox.updates[0]; u.Kind != behavior.UpdateCommand || u.Command != "/contacts" || u.Args != "Lan" {
			t.Fatalf("command=%+v", u)
		}
		if u := inbox.updates[1]; u.Note != behavior.NoteNoOpenReply || strings.HasPrefix(u.Args, behavior.AssistantTextPrefix) {
			t.Fatalf("reply=%+v", u)
		}
		if u := inbox.updates[2]; u.Note != behavior.NoteTextTooLong || u.Args != "" {
			t.Fatalf("long text=%+v", u)
		}
		if u := inbox.updates[3]; u.Args != "" {
			t.Fatalf("blank text=%+v", u)
		}
	})

	t.Run("TC-8 TC-29 other senders are ignored", func(t *testing.T) {
		h, inbox, stager := newHandler(true)
		for i, body := range []string{
			`{"update_id":301,"message":{"message_id":1,"chat":{"id":88},"from":{"id":88},"text":"xin chào bot"}}`,
			`{"update_id":302,"message":{"message_id":2,"chat":{"id":88},"from":{"id":88},"text":"/alias clickup:1 Boss"}}`,
			`{"update_id":303,"message":{"message_id":3,"chat":{"id":88},"from":{"id":88},"text":"/contacts"}}`,
			`{"update_id":304,"message":{"message_id":4,"chat":{"id":77},"from":{"id":88},"text":"/prefs detailed"}}`,
			`{"update_id":305,"message":{"message_id":5,"chat":{"id":77},"text":"không có người gửi"}}`,
		} {
			if rec := post(t, h, body); rec.Code != 200 || !strings.Contains(rec.Body.String(), "ignored") {
				t.Fatalf("body %d: %d %s", i, rec.Code, rec.Body)
			}
		}
		if len(inbox.updates) != 0 || len(stager.texts) != 0 {
			t.Fatalf("foreign updates stored=%+v staged=%q", inbox.updates, stager.texts)
		}
	})

	t.Run("TC-39 assistant off keeps plain text without content", func(t *testing.T) {
		h, inbox, stager := newHandler(false)
		post(t, h, `{"update_id":401,"message":{"message_id":1,"chat":{"id":77},"from":{"id":77},"text":"`+secretText+`"}}`)
		if len(inbox.updates) != 1 || inbox.updates[0].Kind != behavior.UpdateText || inbox.updates[0].Args != "" || len(stager.texts) != 0 {
			t.Fatalf("assistant off: updates=%+v staged=%q", inbox.updates, stager.texts)
		}
	})
}
