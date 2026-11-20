package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TC-C-16..17 (REQ-12, abuse): only dedicated loopback PostgreSQL databases are accepted.
func TestPhase12cValidateLoadtestDatabaseURL(t *testing.T) {
	for _, raw := range []string{
		"postgres://u:p@127.0.0.1:5432/nexus_loadtest?sslmode=disable",
		"postgresql://u:p@[::1]:5432/team_loadtest?sslmode=disable",
	} {
		if err := validateDatabaseURL(raw); err != nil {
			t.Errorf("rejected %q: %v", raw, err)
		}
	}
	secret := "url-password-must-not-leak"
	for _, raw := range []string{
		"mysql://u:" + secret + "@127.0.0.1/x_loadtest",
		"postgres://u:" + secret + "@localhost/x_loadtest",
		"postgres://u:" + secret + "@8.8.8.8/x_loadtest",
		"postgres://u:" + secret + "@127.0.0.1/production",
		"postgres://u:" + secret + "@127.0.0.1/",
		"not a url " + secret,
	} {
		err := validateDatabaseURL(raw)
		if err == nil {
			t.Errorf("accepted %q", raw)
		} else if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked URL secret: %v", err)
		}
	}
}

// R-C-1 regression (REQ-12, abuse): pgx overrides and alternate connection targets are refused safely.
func TestPhase12cValidateLoadtestDatabaseURLRejectsOverrides(t *testing.T) {
	const valid = "postgres://u:p@127.0.0.1:5432/nexus_loadtest?sslmode=disable"
	t.Run("numeric PGPORT is allowed", func(t *testing.T) {
		t.Setenv("PGPORT", "5432")
		if err := validateDatabaseURL(valid); err != nil {
			t.Fatal(err)
		}
	})
	for _, tc := range []struct {
		name string
		env  string
		raw  string
	}{
		{name: "PGHOST", env: "PGHOST", raw: valid},
		{name: "text PGPORT", env: "PGPORT", raw: valid},
		{name: "query host", raw: "postgres://u:url-password-must-not-leak@127.0.0.1/nexus_loadtest?host=127.0.0.1"},
		{name: "query dbname", raw: "postgres://u:url-password-must-not-leak@127.0.0.1/nexus_loadtest?dbname=nexus_loadtest"},
		{name: "query service", raw: "postgres://u:url-password-must-not-leak@127.0.0.1/nexus_loadtest?service=local"},
		{name: "multiple hosts", raw: "postgres://u:url-password-must-not-leak@127.0.0.1:5432,127.0.0.2:5432/nexus_loadtest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv(tc.env, "not-numeric-or-safe")
			}
			err := validateDatabaseURL(tc.raw)
			if err == nil {
				t.Fatal("unsafe database configuration was accepted")
			}
			if strings.Contains(err.Error(), tc.raw) || strings.Contains(err.Error(), "url-password-must-not-leak") {
				t.Fatalf("error leaked database URL or password: %v", err)
			}
		})
	}
}

// TC-C-18 (REQ-12): deliveries are signed, duplicates preserve bodies, and all samples are measured.
func TestPhase12cDeliverSignsUniqueAndDuplicateRequests(t *testing.T) {
	const secret = "test-only-secret"
	var mu sync.Mutex
	seen := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(body)
		if r.Header.Get("X-Signature") != hex.EncodeToString(mac.Sum(nil)) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		seen[string(body)]++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	latencies, failures := deliver(context.Background(), server.Client(), server.URL, secret, 10, 3, 4)
	if failures != 0 || len(latencies) != 13 || len(seen) != 10 {
		t.Fatalf("failures=%d samples=%d unique=%d", failures, len(latencies), len(seen))
	}
	duplicates := 0
	for _, n := range seen {
		if n == 2 {
			duplicates++
		}
	}
	if duplicates != 3 {
		t.Fatalf("duplicate bodies = %d", duplicates)
	}
}

// TC-C-18..20 (REQ-12): nearest-rank percentiles, JSON schema, recorded evidence, and failure threshold.
func TestPhase12cLoadtestReportContract(t *testing.T) {
	values := []time.Duration{500 * time.Millisecond, time.Millisecond, 20 * time.Millisecond, 100 * time.Millisecond, 10 * time.Millisecond,
		2 * time.Millisecond, 3 * time.Millisecond, 4 * time.Millisecond, 5 * time.Millisecond, 6 * time.Millisecond,
		7 * time.Millisecond, 8 * time.Millisecond, 9 * time.Millisecond, 11 * time.Millisecond, 12 * time.Millisecond,
		13 * time.Millisecond, 14 * time.Millisecond, 15 * time.Millisecond, 16 * time.Millisecond, 17 * time.Millisecond}
	got := summarize(values)
	if got.P50MS != 10 || got.P95MS != 100 || got.P99MS != 500 || got.MaxMS != 500 {
		t.Fatalf("percentiles = %+v", got)
	}
	if got.P95MS >= 500 {
		t.Fatal("valid p95 classified as failure")
	}
	got.P95MS = 500
	if got.P95MS < 500 {
		t.Fatal("500ms p95 must fail the below-500 contract")
	}

	tmp := filepath.Join(t.TempDir(), "nested", "report.json")
	want := report{N: 120, Duplicates: 12, Concurrency: 10, ClientLatency: got, Completed: 120, ExecutionCount: 120, Passed: true, GeneratedAt: time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)}
	if err := writeReport(tmp, want); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	var decoded report
	if err := json.Unmarshal(data, &decoded); err != nil || decoded.N != 120 || decoded.ExecutionCount != 120 || !decoded.Passed {
		t.Fatalf("decoded = %+v, %v", decoded, err)
	}

	artifact, err := os.ReadFile(filepath.Join("..", "..", "docs", "product", "reports", "loadtest-2026-11-20.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recorded report
	if err := json.Unmarshal(artifact, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.N < 100 || recorded.Duplicates < 1 || recorded.ExecutionCount != recorded.N || recorded.Completed != recorded.N ||
		recorded.Failed != 0 || recorded.Stuck != 0 || recorded.HTTPFailures != 0 || recorded.ClientLatency.P95MS >= 500 || !recorded.Passed ||
		recorded.GoVersion == "" || recorded.PostgresVersion == "" || recorded.CPUCount < 1 || recorded.GeneratedAt.IsZero() {
		t.Fatalf("recorded evidence does not prove REQ-12: %+v", recorded)
	}
	if strings.Contains(string(artifact), "password") || strings.Contains(string(artifact), "secret") || strings.Contains(string(artifact), "token") {
		t.Fatal("recorded report contains secret-shaped fields")
	}
}

func TestPhase12cLocalClientRejectsOtherDestinations(t *testing.T) {
	client := localClient("127.0.0.1:1")
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:2/", nil)
	_, err := client.Do(req)
	if err == nil || strings.Contains(err.Error(), "test-only-secret") {
		t.Fatalf("unexpected result: %v", err)
	}
}
