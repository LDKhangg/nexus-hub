package http

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/obs"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/labstack/echo/v4"
)

// TC-F-7 (REQ-3, abuse): webhook failure logs expose fixed categories only.
// Request payloads, auth headers, prompts, tokens and storage errors stay out.
func TestPhaseFWebhookFailureLogsExcludePrivateInputs(t *testing.T) {
	oldDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(oldDefault) })
	var logs bytes.Buffer
	obs.Setup("info", &logs)
	const token = "github_pat_123456789012345678901234567890"
	const dsn = "postgres://owner:private-password@127.0.0.1/nexus_storage_test"
	body := `{"event":"taskCommentPosted","webhook_id":"w1","task_id":"t1","history_items":[{"id":"h1","field":"comment","comment":{"id":"c1","text_content":"PRIVATE-PROMPT ` + token + `"}}]}`
	integration := tenant.WebhookIntegration{ID: testIntegrationID, WorkspaceID: "019a6690-abcd-7000-8000-000000000001", Platform: "clickup", Role: tenant.IntegrationRoleSource, IsEnabled: true,
		Config: map[string]any{"clickup_team_id": "team1", "webhook_id": "w1", "webhook_secret_env": "TEST_WEBHOOK_SECRET", "target_integration_id": "019a6690-abcd-7000-8000-000000000012"}}
	h := NewWebhookHandler(
		lookupFunc(func(context.Context, string) (tenant.WebhookIntegration, error) { return integration, nil }),
		NewVerifiedParserFactory(testResolver{value: "test-secret", ok: true}),
		acceptFunc(func(context.Context, []domain.NormalizedEvent) (relay.AcceptResult, error) {
			return relay.AcceptResult{}, errors.New(dsn + " Authorization: Bearer " + token)
		}),
	)
	e := echo.New()
	e.Use(CorrelationMiddleware)
	e.POST("/webhook/:source/:integrationID", h.HandleWebhook)
	req := httptest.NewRequest(stdhttp.MethodPost, "/webhook/clickup/"+testIntegrationID, strings.NewReader(body))
	mac := hmac.New(sha256.New, []byte("test-secret"))
	_, _ = mac.Write([]byte(body))
	req.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != stdhttp.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	raw := logs.String() + rec.Body.String()
	for _, forbidden := range []string{"PRIVATE-PROMPT", token, "private-password", dsn, "Authorization: Bearer"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("private input %q leaked: %s", forbidden, raw)
		}
	}
	for _, required := range []string{`"error_code":"store_failed"`, `"correlation_id":`} {
		if !strings.Contains(logs.String(), required) {
			t.Errorf("safe diagnostic %s absent: %s", required, logs.String())
		}
	}
}
