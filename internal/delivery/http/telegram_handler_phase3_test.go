package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/labstack/echo/v4"
)

type telegramInboxFake struct {
	seen    map[int64]bool
	updates []behavior.InboundUpdate
}

func (f *telegramInboxFake) RecordUpdate(_ context.Context, _ string, u behavior.InboundUpdate) (bool, error) {
	if f.seen == nil {
		f.seen = map[int64]bool{}
	}
	if f.seen[u.UpdateID] {
		return false, nil
	}
	f.seen[u.UpdateID] = true
	f.updates = append(f.updates, u)
	return true, nil
}
func (*telegramInboxFake) ClaimUpdate(context.Context, time.Time, time.Duration) (*behavior.InboundUpdate, error) {
	return nil, nil
}
func (*telegramInboxFake) FinishUpdate(context.Context, int64, string, string) error { return nil }

type answerFake struct{}

func (answerFake) StageAnswer(context.Context, string, int64, string, time.Time) (string, error) {
	return "", nil
}

func TestTelegramWebhookPhase3Lifecycle(t *testing.T) {
	integration := tenant.WebhookIntegration{ID: testIntegrationID, WorkspaceID: "019a6690-abcd-7000-8000-000000000001", Platform: "telegram", Role: tenant.IntegrationRoleControl, IsEnabled: true}
	lookup := lookupFunc(func(context.Context, string) (tenant.WebhookIntegration, error) { return integration, nil })

	request := func(t *testing.T, handler *TelegramHandler, secret, body string) *httptest.ResponseRecorder {
		t.Helper()
		e := echo.New()
		e.POST("/webhook/telegram/:integrationID", handler.Handle)
		req := httptest.NewRequest(http.MethodPost, "/webhook/telegram/"+testIntegrationID, strings.NewReader(body))
		if secret != "" {
			req.Header.Set(TelegramSecretHeader, secret)
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}

	t.Run("TC-7/authenticated owner command queues once", func(t *testing.T) {
		inbox := &telegramInboxFake{}
		h := NewTelegramHandler(lookup, testResolver{value: "webhook-secret", ok: true}, inbox, answerFake{}, 77)
		body := `{"update_id":101,"message":{"message_id":5,"chat":{"id":77},"from":{"id":77},"text":"/help"}}`
		first := request(t, h, "webhook-secret", body)
		second := request(t, h, "webhook-secret", body)
		if first.Code != 200 || !strings.Contains(first.Body.String(), "queued") || second.Code != 200 || !strings.Contains(second.Body.String(), "duplicate") {
			t.Fatalf("first=%d %s second=%d %s", first.Code, first.Body, second.Code, second.Body)
		}
		if len(inbox.updates) != 1 || inbox.updates[0].Command != "/help" || inbox.updates[0].Kind != behavior.UpdateCommand {
			t.Fatalf("updates=%#v", inbox.updates)
		}
	})

	t.Run("TC-8/wrong secret and foreign sender have no effect", func(t *testing.T) {
		inbox := &telegramInboxFake{}
		h := NewTelegramHandler(lookup, testResolver{value: "webhook-secret", ok: true}, inbox, answerFake{}, -100).WithOwnerUser(77)
		foreign := `{"update_id":102,"message":{"message_id":6,"chat":{"id":-100},"from":{"id":88},"text":"/undo 1"}}`
		if rec := request(t, h, "wrong", foreign); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong secret=%d", rec.Code)
		}
		if rec := request(t, h, "webhook-secret", foreign); rec.Code != 200 || !strings.Contains(rec.Body.String(), "ignored") {
			t.Fatalf("foreign=%d %s", rec.Code, rec.Body)
		}
		if len(inbox.updates) != 0 {
			t.Fatalf("foreign update stored: %#v", inbox.updates)
		}
	})
}
