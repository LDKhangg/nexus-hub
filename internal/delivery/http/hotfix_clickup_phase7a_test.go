package http

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/labstack/echo/v4"
)

// TD-CA-1 (FR-018): a signed, real-shaped taskCommentPosted (before null, after a
// comment id string) is accepted with 200, not 400, and reaches the acceptor once.
func TestHotfixCA1_RealShapedCommentThroughHandlerIs200(t *testing.T) {
	body := `{"event":"taskCommentPosted","history_items":[{"id":"4415872","type":1,"date":"1763300000000","field":"comment","parent_id":"901100","data":{},"source":null,"user":{"id":183,"username":"Teammate"},"before":null,"after":"90110012345","comment":{"id":"90110012345","comment":[{"text":"Please check "},{"type":"tag","user":{"id":77,"username":"Owner"}},{"text":" thanks"}],"text_content":"Please check @Owner thanks","user":{"id":183,"username":"Teammate"}}}],"task_id":"86abc","webhook_id":"w1"}`
	integration := tenant.WebhookIntegration{ID: testIntegrationID, WorkspaceID: "019a6690-abcd-7000-8000-000000000001", Platform: "clickup", Role: tenant.IntegrationRoleSource, IsEnabled: true,
		Config: map[string]any{"clickup_team_id": "team1", "webhook_id": "w1", "webhook_secret_env": "TEST_WEBHOOK_SECRET", "target_integration_id": "019a6690-abcd-7000-8000-000000000012"}}
	lookup := lookupFunc(func(context.Context, string) (tenant.WebhookIntegration, error) { return integration, nil })
	var got []domain.NormalizedEvent
	accept := acceptFunc(func(_ context.Context, events []domain.NormalizedEvent) (relay.AcceptResult, error) {
		got = append(got, events...)
		return relay.AcceptResult{Accepted: len(events)}, nil
	})
	h := NewWebhookHandler(lookup, NewVerifiedParserFactory(testResolver{value: "test-secret", ok: true}), accept)
	e := echo.New()
	e.POST("/webhook/:source/:integrationID", h.HandleWebhook)
	mac := hmac.New(sha256.New, []byte("test-secret"))
	mac.Write([]byte(body))
	req := httptest.NewRequest("POST", "/webhook/clickup/"+testIntegrationID, strings.NewReader(body))
	req.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("real-shaped callback answered %d: %s", rec.Code, rec.Body.String())
	}
	if len(got) != 1 || got[0].EventType != domain.EventCommandPosted || got[0].Metadata["tagged_user_ids"] != "77" ||
		got[0].WorkspaceID != integration.WorkspaceID || got[0].IntegrationID != integration.ID {
		t.Fatalf("accepted events: %+v", got)
	}
	// a tampered body is still refused
	req = httptest.NewRequest("POST", "/webhook/clickup/"+testIntegrationID, strings.NewReader(strings.Replace(body, "86abc", "evil", 1)))
	req.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != 401 || len(got) != 1 {
		t.Fatalf("tampered body: %d, events %d", rec.Code, len(got))
	}
}
