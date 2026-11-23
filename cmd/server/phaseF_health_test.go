package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/metrics"
	"github.com/LDKhangg/nexus-hub/internal/obs"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TC-F-10 (REQ-4, abuse): readiness cannot be enabled without a loopback-only
// listener, including a wildcard bind that could expose operational state.
func TestPhaseFReadinessListenerRejectsDisabledAndNonLoopbackAddresses(t *testing.T) {
	hub := metrics.New(metrics.Options{})
	ready := obs.NewReadiness(obs.ReadinessDeps{}).Handler()
	if got := listenMetrics("", hub, ready); got != nil {
		t.Fatal("empty ops address opened a listener")
	}
	if got := listenMetrics("0.0.0.0:0", hub, ready); got != nil {
		t.Fatal("non-loopback ops address opened a listener")
	}
}

// TC-F-16 (REQ-5): an unavailable database stops startup with a safe fixed
// diagnostic. The probe targets only the authorized loopback test database.
func TestPhaseFDatabaseUnavailableFailsStartupWithoutCredentialLeak(t *testing.T) {
	raw := os.Getenv("TEST_DATABASE_URL")
	cfg, err := pgxpool.ParseConfig(raw)
	if err != nil || raw == "" {
		t.Fatal("dedicated TEST_DATABASE_URL required")
	}
	if cfg.ConnConfig.Host != "127.0.0.1" || cfg.ConnConfig.Database != "nexus_storage_test" || len(cfg.ConnConfig.Fallbacks) != 0 {
		t.Fatal("unsafe test database target")
	}
	cfg.ConnConfig.Port = 1
	cfg.ConnConfig.Password = "phase-f-private-password"
	t.Setenv("DATABASE_URL", cfg.ConnString())
	oldDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(oldDefault) })
	var logs bytes.Buffer
	obs.Setup("info", &logs)
	if err := run(); err == nil {
		t.Fatal("startup succeeded with unavailable database")
	}
	if strings.Contains(logs.String(), "phase-f-private-password") || strings.Contains(logs.String(), cfg.ConnString()) {
		t.Fatalf("database credential leaked: %s", logs.String())
	}
	for _, required := range []string{`"component":"startup"`, `"check":"db"`, `"error_code":"database_unavailable"`} {
		if !strings.Contains(logs.String(), required) {
			t.Errorf("safe startup field %s absent: %s", required, logs.String())
		}
	}
}

// TC-F-11 (REQ-4): loopback ops serves the injected readiness report, while
// metrics remains a separate endpoint.
func TestPhaseFOpsReadinessRoute(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	readiness := obs.NewReadiness(obs.ReadinessDeps{
		PingDB: func(context.Context) error { return nil },
		MigrationVersion: func(context.Context) (int64, bool, error) {
			return obs.ExpectedMigrationVersion, true, nil
		},
		MasterKeyLoaded:  true,
		WorkflowLastPoll: func() time.Time { return now },
		WorkflowInterval: time.Second,
		GitSandboxReady:  false,
		Now:              func() time.Time { return now },
	})
	server := listenMetrics("127.0.0.1:0", metrics.New(metrics.Options{}), readiness.Handler())
	if server == nil {
		t.Fatal("loopback ops listener disabled")
	}
	t.Cleanup(func() { _ = server.listener.Close() })
	rec := httptest.NewRecorder()
	server.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var report obs.ReadinessReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("body=%q: %v", rec.Body.String(), err)
	}
	if rec.Code != http.StatusOK || report.Status != "ready" || report.Checks.GitSandbox != "unavailable" {
		t.Fatalf("status=%d report=%+v", rec.Code, report)
	}
}
