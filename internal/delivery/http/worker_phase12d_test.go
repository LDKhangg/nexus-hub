package http

import (
	"context"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/labstack/echo/v4"
)

type phase12dWorkerBackend struct {
	authCalls atomic.Int32
}

func (b *phase12dWorkerBackend) Enroll(context.Context, string, string, []string) (behavior.WorkerDevice, string, error) {
	return behavior.WorkerDevice{}, "", behavior.ErrNotFound
}

func (b *phase12dWorkerBackend) Authenticate(context.Context, string) (behavior.WorkerDevice, error) {
	b.authCalls.Add(1)
	return behavior.WorkerDevice{}, behavior.ErrNotFound
}

// TC-D-14 (REQ-15): enroll stays public, while poll, every job endpoint, and
// unknown worker paths reject unauthenticated callers before backend access.
func TestPhase12dEveryWorkerJobRouteRequiresAuthentication(t *testing.T) {
	backend := &phase12dWorkerBackend{}
	e := echo.New()
	NewWorkerHandler(backend, backend).Register(e)
	for _, path := range []string{
		"/worker/v1/poll",
		"/worker/v1/jobs/job-1/heartbeat",
		"/worker/v1/jobs/job-1/progress",
		"/worker/v1/jobs/job-1/result",
		"/worker/v1/unknown",
	} {
		req := httptest.NewRequest(stdhttp.MethodPost, path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != stdhttp.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"error":"unauthorized"`) {
			t.Errorf("%s: status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
	if backend.authCalls.Load() != 0 {
		t.Fatalf("malformed authentication reached backend %d times", backend.authCalls.Load())
	}
	req := httptest.NewRequest(stdhttp.MethodPost, "/worker/v1/enroll", strings.NewReader(`{"code":"AAAA-BBBB-CCCC","name":"worker"}`))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code == stdhttp.StatusUnauthorized {
		t.Fatalf("enroll incorrectly required device authentication: %s", rec.Body.String())
	}
}
