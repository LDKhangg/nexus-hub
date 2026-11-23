package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/metrics"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/labstack/echo/v4"
)

func opsTestHub() *metrics.Hub {
	return metrics.New(metrics.Options{States: workflowStates(), Codes: metricCodes(),
		Providers: []string{"anthropic"}, Models: []string{"claude-sonnet-5-5"}})
}

func hubText(t *testing.T, h *metrics.Hub) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

// freeLoopbackAddr returns a loopback host:port that was free a moment ago
// (port 0 is rejected by design, so a test needs a concrete port).
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func startMetrics(t *testing.T, addr string, hub *metrics.Hub) (*metricsServer, string, context.CancelFunc, chan error) {
	t.Helper()
	ms := listenMetrics(addr, hub)
	if ms == nil {
		t.Fatalf("listenMetrics(%q) = nil", addr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ms.Run(ctx) }()
	t.Cleanup(cancel)
	return ms, "http://" + ms.listener.Addr().String(), cancel, done
}

// TC-B-3 (REQ-8)
func TestPhase12bMetricsListenerServesOnlyGetMetrics(t *testing.T) {
	hub := opsTestHub()
	hub.Error(metrics.KindStepFailure, "timeout")
	_, base, cancel, done := startMetrics(t, freeLoopbackAddr(t), hub)
	get := func(method, path string) (int, string) {
		req, _ := http.NewRequest(method, base+path, nil)
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get(http.MethodGet, "/metrics"); code != 200 || !strings.Contains(body, "nexushub_errors_total") {
		t.Fatalf("GET /metrics = %d", code)
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		if code, _ := get(m, "/metrics"); code != http.StatusMethodNotAllowed {
			t.Errorf("%s /metrics = %d, want 405", m, code)
		}
	}
	for _, p := range []string{"/", "/metrics/x", "/debug/pprof/", "/healthz", "/webhook/telegram/x"} {
		if code, _ := get(http.MethodGet, p); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, code)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}

// TC-B-4 (REQ-8, abuse)
func TestPhase12bListenMetricsRefusesPublicOrBusyAddress(t *testing.T) {
	hub := opsTestHub()
	for _, addr := range []string{"", "0.0.0.0:9090", ":9090", "8.8.8.8:9090", "example.com:9090", "127.0.0.1:0x", "127.0.0.1:0"} {
		if ms := listenMetrics(addr, hub); ms != nil {
			_ = ms.listener.Close()
			t.Errorf("listenMetrics(%q) bound a listener", addr)
		}
	}
	ms := listenMetrics("127.0.0.1:1", hub) // port 1 is free or privileged; either way no panic
	if ms != nil {
		_ = ms.listener.Close()
	}
	first, _, _, _ := startMetrics(t, freeLoopbackAddr(t), hub)
	busy := first.listener.Addr().String()
	if again := listenMetrics(busy, hub); again != nil {
		_ = again.listener.Close()
		t.Fatal("a busy port must disable metrics, not rebind")
	}
}

// TC-B-5 (REQ-8): the public server registers no /metrics route.
func TestPhase12bPublicServerHasNoMetricsRoute(t *testing.T) {
	for _, f := range []string{"main.go", "wiring.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if regexp.MustCompile(`"/metrics`).Match(b) || regexp.MustCompile(`promhttp|hub\.Handler\(\)|\.Handler\(\)\)`).Match(b) {
			t.Errorf("%s mentions a metrics route or handler", f)
		}
	}
	ops, err := os.ReadFile("ops.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(ops), `"GET /metrics"`) != 1 {
		t.Error("ops.go must register /metrics exactly once, on the loopback mux")
	}
}

// TC-B-10 (REQ-8)
func TestPhase12bWebhookMiddlewareLatencyAndRejectCodes(t *testing.T) {
	hub := opsTestHub()
	e := echo.New()
	status := http.StatusOK
	var herr error
	h := func(c echo.Context) error {
		if herr != nil {
			return herr
		}
		return c.NoContent(status)
	}
	e.POST("/webhook/telegram/:id", h, webhookMetrics(hub, metrics.SourceTelegram))
	e.POST("/webhook/:source/:id", h, webhookMetrics(hub, ""))
	call := func(path string) {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
	}
	call("/webhook/telegram/a")
	call("/webhook/clickup/a")
	call("/webhook/Tin-nhan-bi-mat/a")
	status = 401
	call("/webhook/telegram/a")
	status = 413
	call("/webhook/clickup/a")
	herr = echo.NewHTTPError(http.StatusTooManyRequests)
	call("/webhook/clickup/a")
	herr = errors.New("boom with secret")
	call("/webhook/telegram/a")
	out := hubText(t, hub)
	for _, want := range []string{
		`nexushub_webhook_duration_seconds_count{source="telegram"} 3`,
		`nexushub_webhook_duration_seconds_count{source="clickup"} 3`,
		`nexushub_webhook_duration_seconds_count{source="other"} 1`,
		`nexushub_errors_total{code="http_401",kind="webhook_reject"} 1`,
		`nexushub_errors_total{code="http_413",kind="webhook_reject"} 1`,
		`nexushub_errors_total{code="http_429",kind="webhook_reject"} 1`,
		`nexushub_errors_total{code="http_5xx",kind="webhook_reject"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, "secret") || strings.Contains(out, "Tin-nhan") {
		t.Error("request text leaked into metrics")
	}
}

type fakeNoticeStore struct {
	relay.NoticeDeliveryStore
	retries, fails int
	err            error
}

func (f *fakeNoticeStore) MarkRetry(context.Context, relay.NoticeJob, time.Time, string) error {
	f.retries++
	return f.err
}
func (f *fakeNoticeStore) MarkFailed(context.Context, relay.NoticeJob, string) error {
	f.fails++
	return f.err
}

type fakeActionStore struct {
	relay.ActionStore
	retries, fails int
	err            error
}

func (f *fakeActionStore) MarkRetry(context.Context, relay.OutboundAction, time.Time, string) error {
	f.retries++
	return f.err
}
func (f *fakeActionStore) MarkFailed(context.Context, relay.OutboundAction, string) error {
	f.fails++
	return f.err
}

// TC-B-11 (REQ-8)
func TestPhase12bMeteredDeliveryStoresCountAndDelegate(t *testing.T) {
	hub := opsTestHub()
	boom := errors.New("inner")
	n := &fakeNoticeStore{err: boom}
	ns := meteredNoticeStore{NoticeDeliveryStore: n, hub: hub}
	if err := ns.MarkRetry(context.Background(), relay.NoticeJob{}, time.Now(), "x"); !errors.Is(err, boom) {
		t.Fatalf("notice retry err = %v", err)
	}
	if err := ns.MarkFailed(context.Background(), relay.NoticeJob{}, "x"); !errors.Is(err, boom) {
		t.Fatalf("notice failed err = %v", err)
	}
	a := &fakeActionStore{}
	as := meteredActionStore{ActionStore: a, hub: hub}
	if err := as.MarkRetry(context.Background(), relay.OutboundAction{}, time.Now(), "x"); err != nil {
		t.Fatal(err)
	}
	if err := as.MarkFailed(context.Background(), relay.OutboundAction{}, "x"); err != nil {
		t.Fatal(err)
	}
	if n.retries != 1 || n.fails != 1 || a.retries != 1 || a.fails != 1 {
		t.Fatalf("inner not called once each: %d %d %d %d", n.retries, n.fails, a.retries, a.fails)
	}
	out := hubText(t, hub)
	for _, want := range []string{
		`nexushub_errors_total{code="notice",kind="delivery_retry"} 1`,
		`nexushub_errors_total{code="notice",kind="delivery_failure"} 1`,
		`nexushub_errors_total{code="outbound_action",kind="delivery_retry"} 1`,
		`nexushub_errors_total{code="outbound_action",kind="delivery_failure"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// TC-B-12 (REQ-8): a final step failure is counted as step_failure, a
// retryable one as step_retry; an unknown reason becomes "other".
func TestPhase12bStepFailureObserverFinalFlag(t *testing.T) {
	hub := opsTestHub()
	o := opsServices{hub: hub}
	obs := o.stepFailureObserver()
	known := workflow.KnownReasons()
	if len(known) == 0 {
		t.Fatal("no known reasons")
	}
	code := string(known[0])
	obs(known[0], false)
	obs(known[0], true)
	obs(known[0], true)
	obs(workflow.ReasonCode("free text from a user"), true)
	out := hubText(t, hub)
	for _, want := range []string{
		`nexushub_errors_total{code="` + code + `",kind="step_retry"} 1`,
		`nexushub_errors_total{code="` + code + `",kind="step_failure"} 2`,
		`nexushub_errors_total{code="other",kind="step_failure"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "free text") {
		t.Error("reason text leaked")
	}
}
