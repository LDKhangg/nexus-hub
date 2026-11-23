package http

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/obs"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

// TC-F-4 (REQ-2, abuse): public callers cannot choose or reuse correlation
// IDs, even during a rapid request burst.
func TestPhaseFPublicCorrelationAlwaysFreshUUIDv7(t *testing.T) {
	const requests = 32
	e := echo.New()
	e.Use(CorrelationMiddleware)
	ids := make(chan string, requests)
	e.GET("/probe", func(c echo.Context) error {
		ids <- obs.Correlation(c.Request().Context())
		return c.NoContent(stdhttp.StatusNoContent)
	})

	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(stdhttp.MethodGet, "/probe", nil)
			req.Header.Set("X-Correlation-Id", "019a6690-abcd-7000-8000-000000000099")
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code != stdhttp.StatusNoContent {
				t.Errorf("status=%d", rec.Code)
			}
		}()
	}
	wg.Wait()
	close(ids)
	seen := make(map[string]bool, requests)
	for id := range ids {
		parsed, err := uuid.Parse(id)
		if err != nil || parsed.Version() != 7 {
			t.Errorf("correlation_id=%q, want UUIDv7", id)
		}
		if seen[id] {
			t.Errorf("duplicate correlation_id=%q", id)
		}
		seen[id] = true
	}
	if len(seen) != requests {
		t.Fatalf("unique IDs=%d, want %d", len(seen), requests)
	}
}

// TC-F-4 (REQ-2): the ID assigned at the public boundary is copied into each
// accepted inbound event before storage.
func TestPhaseFWebhookStoresBoundaryCorrelation(t *testing.T) {
	body := `{"event":"taskCommentPosted","webhook_id":"w1","task_id":"t1","history_items":[{"id":"h1","field":"comment","comment":{"id":"c1","text_content":"hello"}}]}`
	integration := tenant.WebhookIntegration{ID: testIntegrationID, WorkspaceID: "019a6690-abcd-7000-8000-000000000001", Platform: "clickup", Role: tenant.IntegrationRoleSource, IsEnabled: true,
		Config: map[string]any{"clickup_team_id": "team1", "webhook_id": "w1", "webhook_secret_env": "TEST_WEBHOOK_SECRET", "target_integration_id": "019a6690-abcd-7000-8000-000000000012"}}
	var stored []domain.NormalizedEvent
	h := NewWebhookHandler(
		lookupFunc(func(context.Context, string) (tenant.WebhookIntegration, error) { return integration, nil }),
		NewVerifiedParserFactory(testResolver{value: "test-secret", ok: true}),
		acceptFunc(func(_ context.Context, events []domain.NormalizedEvent) (relay.AcceptResult, error) {
			stored = append(stored, events...)
			return relay.AcceptResult{Accepted: len(events)}, nil
		}),
	)
	e := echo.New()
	e.Use(CorrelationMiddleware)
	e.POST("/webhook/:source/:integrationID", h.HandleWebhook)
	req := httptest.NewRequest(stdhttp.MethodPost, "/webhook/clickup/"+testIntegrationID, strings.NewReader(body))
	mac := hmac.New(sha256.New, []byte("test-secret"))
	_, _ = mac.Write([]byte(body))
	req.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-Correlation-Id", "019a6690-abcd-7000-8000-000000000099")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != stdhttp.StatusOK || len(stored) != 1 {
		t.Fatalf("status=%d stored=%+v body=%s", rec.Code, stored, rec.Body.String())
	}
	parsed, err := uuid.Parse(stored[0].CorrelationID)
	if err != nil || parsed.Version() != 7 || stored[0].CorrelationID == req.Header.Get("X-Correlation-Id") {
		t.Fatalf("stored correlation_id=%q", stored[0].CorrelationID)
	}
}

type phaseFWorkerAuth struct{}

func (phaseFWorkerAuth) Enroll(context.Context, string, string, []string) (behavior.WorkerDevice, string, error) {
	return behavior.WorkerDevice{}, "", behavior.ErrNotFound
}

func (phaseFWorkerAuth) Authenticate(context.Context, string) (behavior.WorkerDevice, error) {
	return behavior.WorkerDevice{ID: "device-1", Name: "laptop"}, nil
}

// TC-F-5 (REQ-2): only an authenticated worker may retain a parseable caller
// correlation ID; invalid and absent values become fresh UUIDv7 identifiers.
func TestPhaseFAuthenticatedWorkerCorrelationRules(t *testing.T) {
	backend := phaseFWorkerAuth{}
	h := NewWorkerHandler(backend, backend)
	valid := "019a6690-abcd-7000-8000-000000000055"
	for _, tc := range []struct {
		name, header string
		retained     bool
	}{
		{name: "valid", header: valid, retained: true},
		{name: "invalid", header: "same-for-every-job"},
		{name: "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			var got string
			e.GET("/private", h.authenticate(func(c echo.Context) error {
				got = obs.Correlation(c.Request().Context())
				return c.NoContent(stdhttp.StatusNoContent)
			}))
			req := httptest.NewRequest(stdhttp.MethodGet, "/private", nil)
			req.Header.Set("Authorization", "Bearer nhw_"+strings.Repeat("a", 43))
			if tc.header != "" {
				req.Header.Set("X-Correlation-Id", tc.header)
			}
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code != stdhttp.StatusNoContent {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if tc.retained {
				if got != valid {
					t.Fatalf("correlation=%q, want retained %q", got, valid)
				}
				return
			}
			parsed, err := uuid.Parse(got)
			if err != nil || parsed.Version() != 7 || got == tc.header {
				t.Fatalf("correlation=%q, want fresh UUIDv7", got)
			}
		})
	}
}
