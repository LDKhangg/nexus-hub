package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	httpdelivery "github.com/LDKhangg/nexus-hub/internal/delivery/http"
	"github.com/LDKhangg/nexus-hub/internal/obs"
	"github.com/labstack/echo/v4"
)

// TC-F-3 (REQ-1): the actual panic middleware used by the server emits the
// same required JSON envelope as startup, request and shutdown records.
func TestPhaseFRecoveredPanicUsesStructuredLogEnvelope(t *testing.T) {
	e := echo.New()
	var logs bytes.Buffer
	oldDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(oldDefault) })
	obs.Setup("info", &logs)
	e.Use(httpdelivery.CorrelationMiddleware, httpdelivery.Recover)
	const planted = "phase-f-planted-panic-token"
	e.GET("/panic", func(echo.Context) error { panic(planted) })
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panic", nil))
	if rec.Code != http.StatusInternalServerError || rec.Body.Len() != 0 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	line := strings.TrimSpace(logs.String())
	if strings.Count(line, "\n") != 0 {
		t.Fatalf("panic emitted more than one log record: %q", line)
	}
	if strings.Contains(line, planted) || strings.Contains(line, `"stack"`) || strings.Contains(line, "runtime/debug") {
		t.Fatalf("panic value or stack leaked: %q", line)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		t.Fatalf("panic log is not one JSON record: %q: %v", line, err)
	}
	for _, key := range []string{"time", "level", "msg", "component", "correlation_id"} {
		if record[key] == nil || record[key] == "" {
			t.Errorf("panic log missing %s: %#v", key, record)
		}
	}
	if record["msg"] != "http handler panic" || record["component"] != "http" || record["error_code"] != "panic" ||
		record["method"] != http.MethodGet || record["route"] != "/panic" || record["status"] != float64(http.StatusInternalServerError) {
		t.Fatalf("panic log shape: %#v", record)
	}
}
