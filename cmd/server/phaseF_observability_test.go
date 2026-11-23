package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

// TC-F-3 (REQ-1): the actual panic middleware used by the server emits the
// same required JSON envelope as startup, request and shutdown records.
func TestPhaseFRecoveredPanicUsesStructuredLogEnvelope(t *testing.T) {
	e := echo.New()
	var logs bytes.Buffer
	e.Logger.SetOutput(&logs)
	e.Use(middleware.Recover())
	e.GET("/panic", func(echo.Context) error { panic("phase-f-test-panic") })
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panic", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	line := strings.TrimSpace(logs.String())
	var record map[string]any
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		t.Fatalf("panic log is not one JSON record: %q: %v", line, err)
	}
	for _, key := range []string{"time", "level", "msg", "component"} {
		if record[key] == nil || record[key] == "" {
			t.Errorf("panic log missing %s: %#v", key, record)
		}
	}
}
