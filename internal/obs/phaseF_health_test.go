package obs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func phaseFReadyDeps(now time.Time) ReadinessDeps {
	return ReadinessDeps{
		PingDB: func(context.Context) error { return nil },
		MigrationVersion: func(context.Context) (int64, bool, error) {
			return ExpectedMigrationVersion, true, nil
		},
		MasterKeyLoaded:  true,
		WorkflowLastPoll: func() time.Time { return now.Add(-time.Second) },
		WorkflowInterval: time.Second,
		GitSandboxReady:  true,
		Now:              func() time.Time { return now },
	}
}

func phaseFServeReadiness(t *testing.T, r *Readiness) (int, ReadinessReport, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var report ReadinessReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("readiness is not JSON: %v: %s", err, rec.Body.String())
	}
	return rec.Code, report, rec.Body.String()
}

// TC-F-11 (REQ-4): informational git availability never blocks required
// dependencies, and the private endpoint uses only closed status values.
func TestPhaseFReadinessRequiredAndInformationalChecks(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	deps := phaseFReadyDeps(now)
	deps.GitSandboxReady = false
	code, report, _ := phaseFServeReadiness(t, NewReadiness(deps))
	if code != http.StatusOK || report.Status != "ready" || report.Checks.GitSandbox != "unavailable" {
		t.Fatalf("code=%d report=%+v", code, report)
	}
	want := ReadinessChecks{DB: "ok", Migrations: "ok", MasterKey: "ok", WorkflowWorker: "ok", GitSandbox: "unavailable"}
	if report.Checks != want {
		t.Fatalf("checks=%+v, want %+v", report.Checks, want)
	}
}

// TC-F-12 (REQ-4): every required dependency independently controls 503,
// including an unapplied or unexpected migration version.
func TestPhaseFReadinessEachRequiredFailureReturns503(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	tests := map[string]func(*ReadinessDeps){
		"db": func(d *ReadinessDeps) { d.PingDB = func(context.Context) error { return errors.New("down") } },
		"migration_error": func(d *ReadinessDeps) {
			d.MigrationVersion = func(context.Context) (int64, bool, error) { return 0, false, errors.New("down") }
		},
		"migration_unapplied": func(d *ReadinessDeps) {
			d.MigrationVersion = func(context.Context) (int64, bool, error) { return ExpectedMigrationVersion, false, nil }
		},
		"migration_mismatch": func(d *ReadinessDeps) {
			d.MigrationVersion = func(context.Context) (int64, bool, error) { return ExpectedMigrationVersion - 1, true, nil }
		},
		"master_key": func(d *ReadinessDeps) { d.MasterKeyLoaded = false },
		"workflow":   func(d *ReadinessDeps) { d.WorkflowLastPoll = func() time.Time { return time.Time{} } },
	}
	for name, breakDep := range tests {
		t.Run(name, func(t *testing.T) {
			deps := phaseFReadyDeps(now)
			breakDep(&deps)
			code, report, _ := phaseFServeReadiness(t, NewReadiness(deps))
			if code != http.StatusServiceUnavailable || report.Status != "not_ready" {
				t.Fatalf("code=%d report=%+v", code, report)
			}
		})
	}
}

// TC-F-13 (REQ-4, abuse): a dependency that hangs until cancellation stays
// bounded by its private deadline, and its credential-bearing error is absent.
func TestPhaseFReadinessHangingDBIsBoundedAndPrivate(t *testing.T) {
	now := time.Now()
	deps := phaseFReadyDeps(now)
	const secret = "postgres://owner:do-not-print@127.0.0.1/nexus_storage_test"
	deps.PingDB = func(ctx context.Context) error {
		<-ctx.Done()
		return errors.New(secret)
	}
	started := time.Now()
	code, report, body := phaseFServeReadiness(t, NewReadiness(deps))
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("readiness took %v, want database deadline near 1s", elapsed)
	}
	if code != http.StatusServiceUnavailable || report.Checks.DB != "fail" || strings.Contains(body, secret) || strings.Contains(body, "do-not-print") {
		t.Fatalf("code=%d report=%+v body=%s", code, report, body)
	}
	for _, value := range []string{report.Status, report.Checks.DB, report.Checks.Migrations, report.Checks.MasterKey, report.Checks.WorkflowWorker, report.Checks.GitSandbox} {
		allowed := map[string]bool{"ready": true, "not_ready": true, "ok": true, "fail": true, "stale": true, "unavailable": true}
		if !allowed[value] {
			t.Errorf("open-vocabulary value %q", value)
		}
	}
}

// TC-F-14 (REQ-4): worker freshness starts stale, stays ready through exactly
// three poll intervals, and becomes stale immediately after the window.
func TestPhaseFReadinessWorkerFreshnessUsesFakeClock(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	last := time.Time{}
	deps := phaseFReadyDeps(now)
	deps.WorkflowInterval = 10 * time.Second
	deps.WorkflowLastPoll = func() time.Time { return last }
	deps.Now = func() time.Time { return now }
	if report := NewReadiness(deps).Check(context.Background()); report.Checks.WorkflowWorker != "stale" {
		t.Fatalf("before first poll=%+v", report)
	}
	last = now.Add(-30 * time.Second)
	if report := NewReadiness(deps).Check(context.Background()); report.Status != "ready" || report.Checks.WorkflowWorker != "ok" {
		t.Fatalf("at freshness boundary=%+v", report)
	}
	now = now.Add(time.Nanosecond)
	if report := NewReadiness(deps).Check(context.Background()); report.Status != "not_ready" || report.Checks.WorkflowWorker != "stale" {
		t.Fatalf("after freshness boundary=%+v", report)
	}
}

// TC-F-17 (REQ-4): generated readiness expectation tracks the highest numbered
// migration, so a migration cannot silently make every process unready.
func TestPhaseFExpectedMigrationVersionMatchesMigrationFiles(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "db", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("migration files: %v, count=%d", err, len(files))
	}
	var highest int64
	for _, file := range files {
		prefix, _, ok := strings.Cut(filepath.Base(file), "_")
		if !ok {
			continue
		}
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err == nil && version > highest {
			highest = version
		}
	}
	if ExpectedMigrationVersion != highest {
		t.Fatalf("ExpectedMigrationVersion=%d, highest migration=%d", ExpectedMigrationVersion, highest)
	}
	if _, err := os.Stat(filepath.Join("..", "..", "db", "migrations", "00042_inbound_correlation.sql")); err != nil {
		t.Fatalf("correlation migration missing: %v", err)
	}
}
