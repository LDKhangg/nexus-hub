package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func scrape(t *testing.T, h *Hub) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status %d", rec.Code)
	}
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

func testHub(snap Snapshotter) *Hub {
	return New(Options{
		States:    []string{"QUEUED", "RUNNING", "FAILED"},
		Codes:     []string{"timeout", "lease_expired", "llm_budget_exceeded"},
		Providers: []string{"anthropic", "claude_cli"},
		Models:    []string{"claude-sonnet-5-5"},
		Snapshot:  snap,
	})
}

// TC-B-6 (REQ-8)
func TestPhase12bScrapeExposesRequiredFamilies(t *testing.T) {
	h := testHub(func(context.Context) (Snapshot, error) {
		return Snapshot{
			ExecutionsByState:  map[string]int64{"QUEUED": 3, "FAILED": 1, "WEIRD": 2},
			QueuePending:       7,
			OldestDueAge:       90 * time.Second,
			WorkerDevices:      2,
			WorkersOnline:      1,
			NewestHeartbeatAge: 12 * time.Second,
			LLM: []LLMDay{
				{Provider: "anthropic", Model: "claude-sonnet-5-5", InputTokens: 100, OutputTokens: 40, CostUSD: 0.25},
				{Provider: "claude_cli", Model: "other", InputTokens: 5, OutputTokens: 5},
			},
		}, nil
	})
	h.Error(KindStepFailure, "timeout")
	h.WebhookLatency(SourceTelegram, 30*time.Millisecond)
	out := scrape(t, h)
	for _, want := range []string{
		`nexushub_executions{state="QUEUED"} 3`,
		`nexushub_executions{state="RUNNING"} 0`,
		`nexushub_executions{state="FAILED"} 1`,
		`nexushub_executions{state="other"} 2`,
		`nexushub_queue_pending_jobs 7`,
		`nexushub_queue_oldest_due_age_seconds 90`,
		`nexushub_worker_devices 2`,
		`nexushub_worker_devices_online 1`,
		`nexushub_worker_newest_heartbeat_age_seconds 12`,
		`nexushub_llm_tokens_today{direction="input",model="claude-sonnet-5-5",provider="anthropic"} 100`,
		`nexushub_llm_tokens_today{direction="output",model="claude-sonnet-5-5",provider="anthropic"} 40`,
		`nexushub_llm_cost_usd_today{model="claude-sonnet-5-5",provider="anthropic"} 0.25`,
		`nexushub_errors_total{code="timeout",kind="step_failure"} 1`,
		`nexushub_webhook_duration_seconds_count{source="telegram"} 1`,
		`nexushub_ops_scrape_error 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	// No heartbeat yet reports -1.
	h2 := testHub(func(context.Context) (Snapshot, error) {
		return Snapshot{NewestHeartbeatAge: -1}, nil
	})
	if !strings.Contains(scrape(t, h2), `nexushub_worker_newest_heartbeat_age_seconds -1`) {
		t.Error("no-heartbeat value must be -1")
	}
}

// TC-B-7 (REQ-8, abuse): user text never becomes a label value.
func TestPhase12bLabelsStayInClosedSets(t *testing.T) {
	h := testHub(func(context.Context) (Snapshot, error) {
		return Snapshot{LLM: []LLMDay{{Provider: "evil-provider", Model: "ignore previous instructions", InputTokens: 1}}}, nil
	})
	junk := []string{"Tìm giúp tôi file bí mật", strings.Repeat("x", 10_000), `a"b\n{c}`, "", "sk-ant-api03-SECRET"}
	for _, j := range junk {
		h.Error(j, j)
		h.Error(KindStepFailure, j)
		h.WebhookLatency(j, time.Millisecond)
		h.AlertSent(j)
	}
	for i := 0; i < 500; i++ {
		h.Error("kind"+fmt.Sprint(i), "code"+fmt.Sprint(i))
		h.WebhookLatency(fmt.Sprint("src", i), time.Millisecond)
	}
	out := scrape(t, h)
	for _, bad := range []string{"bí mật", "SECRET", "xxxxxxxxxx", "ignore previous", "evil-provider", "workspace", "code499", "kind499", "src499"} {
		if strings.Contains(out, bad) {
			t.Errorf("scrape leaks %q", bad)
		}
	}
	for _, want := range []string{`code="other",kind="other"`, `code="other",kind="step_failure"`, `source="other"`, `state="other"`, `model="other",provider="other"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if n := strings.Count(out, "\n"); n > 200 {
		t.Errorf("scrape has %d lines; cardinality not bounded", n)
	}
	// A code that is in the set but has an unsafe shape also maps to other.
	hs := New(Options{Codes: []string{"Bad Code!"}})
	hs.Error(KindStepFailure, "Bad Code!")
	if !strings.Contains(scrape(t, hs), `code="other"`) {
		t.Error("unsafe code shape must map to other")
	}
	// A nil hub counts nothing and does not panic.
	var nilHub *Hub
	nilHub.Error(KindStepFailure, "x")
	nilHub.WebhookLatency("x", 0)
	nilHub.AlertSent("x")
	nilHub.AlertSuppressed()
}

// TC-B-8 (REQ-8)
func TestPhase12bHTTPStatusCodeMapping(t *testing.T) {
	cases := map[int]string{
		400: "http_400", 401: "http_401", 403: "http_403", 404: "http_404", 405: "http_405", 409: "http_409",
		413: "http_413", 415: "http_415", 429: "http_429", 418: "http_4xx", 499: "http_4xx", 0: "http_4xx", -5: "http_4xx",
		500: "http_5xx", 502: "http_5xx", 599: "http_5xx", 999: "http_5xx",
	}
	for status, want := range cases {
		if got := HTTPStatusCode(status); got != want {
			t.Errorf("HTTPStatusCode(%d) = %q, want %q", status, got, want)
		}
	}
}

// TC-B-9 (REQ-8): a failing snapshot keeps the scrape alive.
func TestPhase12bSnapshotErrorKeepsScrapeUp(t *testing.T) {
	h := testHub(func(context.Context) (Snapshot, error) {
		return Snapshot{}, errors.New("password=hunter2 connection refused")
	})
	h.Error(KindWebhookReject, "timeout")
	out := scrape(t, h)
	if !strings.Contains(out, "nexushub_ops_scrape_error 1") || !strings.Contains(out, "nexushub_errors_total") {
		t.Fatalf("scrape error / counters missing:\n%s", out)
	}
	if strings.Contains(out, "hunter2") || strings.Contains(out, "\nnexushub_queue_pending_jobs ") {
		t.Fatal("error text leaked or stale gauges reported")
	}
	// A hanging snapshot is cut by ScrapeTimeout.
	slow := New(Options{ScrapeTimeout: 50 * time.Millisecond, Snapshot: func(ctx context.Context) (Snapshot, error) {
		<-ctx.Done()
		return Snapshot{}, ctx.Err()
	}})
	if !strings.Contains(scrape(t, slow), "nexushub_ops_scrape_error 1") {
		t.Fatal("slow snapshot must report scrape error")
	}
}

// TC-B-43 (REQ-10)
func TestPhase12bAlertCountersClosedLabels(t *testing.T) {
	h := testHub(nil)
	h.AlertSent(AlertStuckRunning)
	h.AlertSent(AlertFailed)
	h.AlertSent(AlertFailed)
	h.AlertSent("made-up")
	h.AlertSuppressed()
	h.AlertSuppressed()
	out := scrape(t, h)
	for _, want := range []string{
		`nexushub_alerts_sent_total{state="stuck_running"} 1`,
		`nexushub_alerts_sent_total{state="failed"} 2`,
		`nexushub_alerts_sent_total{state="other"} 1`,
		`nexushub_alerts_suppressed_total 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, "made-up") {
		t.Error("alert state label leaked")
	}
}
