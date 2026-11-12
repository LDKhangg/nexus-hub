package http

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

const testIntegrationID = "019a6690-abcd-7000-8000-000000000011"

type lookupFunc func(context.Context, string) (tenant.WebhookIntegration, error)

func (f lookupFunc) LookupWebhookIntegration(ctx context.Context, id string) (tenant.WebhookIntegration, error) {
	return f(ctx, id)
}

type acceptFunc func(context.Context, []domain.NormalizedEvent) (relay.AcceptResult, error)

func (f acceptFunc) Accept(ctx context.Context, events []domain.NormalizedEvent) (relay.AcceptResult, error) {
	return f(ctx, events)
}

func TestVerifiedWebhook(t *testing.T) {
	body := `{"event":"taskCommentPosted","webhook_id":"w1","task_id":"t1","history_items":[{"id":"h1","field":"comment","comment":{"id":"c1","text_content":"Hello"}},{"id":"h2","field":"comment","comment":{"id":"c2","text_content":"World"}}]}`
	for _, tc := range []struct {
		name string
		code int
	}{
		{"queued", 200}, {"duplicate", 200}, {"ignored", 200}, {"invalid_id", 400}, {"unknown", 404}, {"disabled", 404}, {"platform", 404}, {"control_role", 404}, {"signature", 401}, {"webhook_mismatch", 401}, {"malformed", 400}, {"lookup_failure", 503}, {"accept_failure", 503}, {"missing_secret", 503}, {"oversize", 413}, {"streaming_oversize", 413}, {"canceled", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			integration := tenant.WebhookIntegration{ID: testIntegrationID, WorkspaceID: "019a6690-abcd-7000-8000-000000000001", Platform: "clickup", Role: tenant.IntegrationRoleSource, IsEnabled: true, Config: map[string]any{"clickup_team_id": "team1", "webhook_id": "w1", "webhook_secret_env": "TEST_WEBHOOK_SECRET", "target_integration_id": "019a6690-abcd-7000-8000-000000000012"}}
			switch tc.name {
			case "disabled":
				integration.IsEnabled = false
			case "platform":
				integration.Platform = "github"
			case "control_role":
				integration.Role = tenant.IntegrationRoleControl
			case "webhook_mismatch":
				integration.Config["webhook_id"] = "other"
			}
			lookup := lookupFunc(func(ctx context.Context, id string) (tenant.WebhookIntegration, error) {
				if tc.name == "unknown" {
					return tenant.WebhookIntegration{}, tenant.ErrIntegrationNotFound
				}
				if tc.name == "lookup_failure" {
					return tenant.WebhookIntegration{}, errors.New("private DB detail")
				}
				if err := ctx.Err(); err != nil {
					return tenant.WebhookIntegration{}, err
				}
				return integration, nil
			})
			called := false
			accept := acceptFunc(func(ctx context.Context, events []domain.NormalizedEvent) (relay.AcceptResult, error) {
				called = true
				if len(events) != 2 || events[0].WorkspaceID != integration.WorkspaceID || events[0].IntegrationID != integration.ID {
					t.Fatalf("untrusted/missing events: %+v", events)
				}
				if tc.name == "accept_failure" {
					return relay.AcceptResult{}, errors.New("private DB detail")
				}
				if tc.name == "duplicate" {
					return relay.AcceptResult{Duplicates: 2}, nil
				}
				return relay.AcceptResult{Accepted: 2}, nil
			})
			factory := NewVerifiedParserFactory(testResolver{value: "test-secret", ok: tc.name != "missing_secret"})
			h := NewWebhookHandler(lookup, factory, accept)
			e := echo.New()
			if tc.name == "streaming_oversize" {
				e.Use(middleware.BodyLimit("1M"))
			}
			e.POST("/webhook/:source/:integrationID", h.HandleWebhook)
			requestBody := body
			if tc.name == "ignored" {
				requestBody = `{"event":"taskUpdated"}`
			}
			if tc.name == "malformed" {
				requestBody = "{"
			}
			if tc.name == "oversize" || tc.name == "streaming_oversize" {
				requestBody = strings.Repeat("x", maxBodyBytes+1)
			}
			id := testIntegrationID
			if tc.name == "invalid_id" {
				id = "invalid"
			}
			req := httptest.NewRequest("POST", "/webhook/clickup/"+id, strings.NewReader(requestBody))
			if tc.name == "streaming_oversize" {
				req.ContentLength = -1
				req.TransferEncoding = []string{"chunked"}
			}
			mac := hmac.New(sha256.New, []byte("test-secret"))
			mac.Write([]byte(requestBody))
			req.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
			if tc.name == "signature" {
				req.Header.Set("X-Signature", "bad")
			}
			if tc.name == "canceled" {
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
			}
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code != tc.code {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
			}
			wantCalled := tc.name == "queued" || tc.name == "duplicate" || tc.name == "accept_failure"
			if called != wantCalled {
				t.Fatalf("accept called=%v", called)
			}
			if strings.Contains(rec.Body.String(), "private") || strings.Contains(rec.Body.String(), "test-secret") {
				t.Fatal("leaked internal error")
			}
			legacy := httptest.NewRecorder()
			e.ServeHTTP(legacy, httptest.NewRequest("POST", "/webhook/clickup", nil))
			if legacy.Code != 404 {
				t.Fatal("unscoped route accepted")
			}
		})
	}
}

type testResolver struct {
	value string
	ok    bool
}

func (r testResolver) Resolve(context.Context, string, string) (string, error) {
	if !r.ok {
		return "", errors.New("missing_credentials")
	}
	return r.value, nil
}

type recordingResolver struct{ ws, ref string }

func (r *recordingResolver) Resolve(_ context.Context, ws, ref string) (string, error) {
	r.ws, r.ref = ws, ref
	return "secret", nil
}

func TestParserFactory_UsesResolver(t *testing.T) {
	rec := &recordingResolver{}
	f := NewVerifiedParserFactory(rec)
	integration := tenant.WebhookIntegration{ID: testIntegrationID, WorkspaceID: "019a6690-abcd-7000-8000-000000000001", Platform: "clickup", Role: tenant.IntegrationRoleSource, IsEnabled: true,
		Config: map[string]any{"clickup_team_id": "1", "webhook_id": "w", "webhook_secret_ref": "clickup-webhook", "target_integration_id": "019a6690-abcd-7000-8000-000000000012"}}
	if _, err := f.ForIntegration(integration); err != nil {
		t.Fatal(err)
	}
	if rec.ws != integration.WorkspaceID || rec.ref != "clickup-webhook" {
		t.Fatalf("%+v", rec)
	}
	if _, err := NewVerifiedParserFactory(testResolver{}).ForIntegration(integration); err == nil {
		t.Fatal("missing secret accepted")
	}
}
