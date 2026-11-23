package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/llm"
	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/metrics"
	"github.com/LDKhangg/nexus-hub/internal/orchestrator"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/settings"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
)

// Phase 12 B1: observability wiring. The metrics hub, the LLM spend meter
// and the loopback /metrics listener. Metrics carry counts, states, ages
// and closed codes only.

const (
	metricsScrapeTimeout   = 5 * time.Second
	metricsShutdownTimeout = 2 * time.Second
	metricsHeaderTimeout   = 5 * time.Second
)

// Delivery codes of nexushub_errors_total (kind delivery_failure or
// delivery_retry): which queue failed, never the provider's detail.
const (
	codeNoticeDelivery = "notice"
	codeActionDelivery = "outbound_action"
)

type opsServices struct {
	store *storage.OpsStore
	hub   *metrics.Hub
	meter *llm.Meter
}

// workflowStates is the closed state label set.
func workflowStates() []string {
	states := []workflow.State{
		workflow.StateReceived, workflow.StateTriaging, workflow.StatePlanning, workflow.StateQueued, workflow.StateRunning,
		workflow.StateEvaluating, workflow.StateWaitingForUser, workflow.StateWaitingForApproval, workflow.StateWaitingForWorker,
		workflow.StateCompleted, workflow.StateFailed, workflow.StateCancelled,
	}
	out := make([]string, 0, len(states))
	for _, s := range states {
		out = append(out, string(s))
	}
	return out
}

// metricCodes is the closed code label set: every registered workflow
// reason (the same sources nh workflow validates against), the webhook
// status codes, the delivery queues and the budget code.
func metricCodes() []string {
	var out []string
	for _, source := range [][]workflow.ReasonCode{
		workflow.KnownReasons(),
		agent.KnownWorkflowReasons(),
		behavior.KnownWorkflowReasons(),
		behavior.KnownLocalTaskReasons(),
		orchestrator.KnownWorkflowReasons(),
		storage.KnownSignalReasons(),
	} {
		for _, r := range source {
			out = append(out, string(r))
		}
	}
	for _, st := range []int{400, 401, 403, 404, 405, 409, 413, 415, 429, 500} {
		out = append(out, metrics.HTTPStatusCode(st))
	}
	return append(out, "http_4xx", codeNoticeDelivery, codeActionDelivery, domain.LLMCodeBudgetExceeded)
}

func buildOps(pool *pgxpool.Pool, s settings.Settings) (opsServices, error) {
	store, err := storage.NewOpsStore(pool)
	if err != nil {
		return opsServices{}, errors.New("ops store startup failed")
	}
	hub := metrics.New(metrics.Options{
		States:    workflowStates(),
		Codes:     metricCodes(),
		Providers: []string{string(llm.ProviderAnthropic), string(llm.ProviderClaudeCLI), string(llm.ProviderOpencodeCLI)},
		Models:    []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5"},
		Snapshot: func(ctx context.Context) (metrics.Snapshot, error) {
			snap, err := store.Snapshot(ctx, "", time.Now())
			if err != nil {
				return metrics.Snapshot{}, err
			}
			out := metrics.Snapshot{
				ExecutionsByState: snap.ExecutionsByState, QueuePending: snap.QueuePending, OldestDueAge: snap.OldestDueAge,
				WorkerDevices: snap.WorkerDevices, WorkersOnline: snap.WorkersOnline, NewestHeartbeatAge: snap.NewestHeartbeatAge,
			}
			for _, u := range snap.LLMUsage {
				out.LLM = append(out.LLM, metrics.LLMDay{Provider: u.Provider, Model: u.Model, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CostUSD: u.CostUSD})
			}
			return out, nil
		},
		ScrapeTimeout: metricsScrapeTimeout,
	})
	meter := &llm.Meter{
		Ledger: store, Budget: s.LLMDailyBudgetUSD,
		OnRefused: func() { hub.Error(metrics.KindLLMBudget, domain.LLMCodeBudgetExceeded) },
	}
	if s.LLMDailyBudgetUSD > 0 {
		log.Printf("llm daily budget: enforced, usd=%.2f per UTC day per workspace", s.LLMDailyBudgetUSD)
	}
	return opsServices{store: store, hub: hub, meter: meter}, nil
}

// stepFailureObserver counts workflow step retries and failures.
func (o opsServices) stepFailureObserver() func(workflow.ReasonCode, bool) {
	return func(reason workflow.ReasonCode, final bool) {
		kind := metrics.KindStepRetry
		if final {
			kind = metrics.KindStepFailure
		}
		o.hub.Error(kind, string(reason))
	}
}

// webhookMetrics records the handler latency of the inbound webhook routes
// and counts rejected requests (status >= 400) by status code. The source
// label is fixed per route (telegram) or the :source path value when it
// is clickup; anything else is "other".
func webhookMetrics(hub *metrics.Hub, fixedSource string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			started := time.Now()
			err := next(c)
			source := fixedSource
			if source == "" {
				source = metrics.SourceOther
				if c.Param("source") == metrics.SourceClickUp {
					source = metrics.SourceClickUp
				}
			}
			hub.WebhookLatency(source, time.Since(started))
			status := c.Response().Status
			var he *echo.HTTPError
			if errors.As(err, &he) {
				status = he.Code
			} else if err != nil && !c.Response().Committed {
				status = http.StatusInternalServerError
			}
			if status >= 400 {
				hub.Error(metrics.KindWebhookReject, metrics.HTTPStatusCode(status))
			}
			return err
		}
	}
}

// meteredNoticeStore counts notice delivery failures and retries.
type meteredNoticeStore struct {
	relay.NoticeDeliveryStore
	hub *metrics.Hub
}

func (m meteredNoticeStore) MarkRetry(ctx context.Context, j relay.NoticeJob, at time.Time, code string) error {
	m.hub.Error(metrics.KindDeliveryRetry, codeNoticeDelivery)
	return m.NoticeDeliveryStore.MarkRetry(ctx, j, at, code)
}

func (m meteredNoticeStore) MarkFailed(ctx context.Context, j relay.NoticeJob, code string) error {
	m.hub.Error(metrics.KindDeliveryFailure, codeNoticeDelivery)
	return m.NoticeDeliveryStore.MarkFailed(ctx, j, code)
}

// meteredActionStore counts outbound action delivery failures and retries.
type meteredActionStore struct {
	relay.ActionStore
	hub *metrics.Hub
}

func (m meteredActionStore) MarkRetry(ctx context.Context, a relay.OutboundAction, at time.Time, code string) error {
	m.hub.Error(metrics.KindDeliveryRetry, codeActionDelivery)
	return m.ActionStore.MarkRetry(ctx, a, at, code)
}

func (m meteredActionStore) MarkFailed(ctx context.Context, a relay.OutboundAction, code string) error {
	m.hub.Error(metrics.KindDeliveryFailure, codeActionDelivery)
	return m.ActionStore.MarkFailed(ctx, a, code)
}

// metricsServer serves /metrics and /readyz on the loopback listener. It never takes
// the hub down: a serve failure is logged and the service waits for
// shutdown. It stops with the background workers, after the public HTTP
// server stopped accepting (REQ-4 order).
type metricsServer struct {
	listener net.Listener
	handler  http.Handler
}

// listenMetrics opens the metrics listener on addr ("" = off). The address
// is validated as loopback here too, so a bad stored value never binds a
// public interface. A listen failure disables metrics and is logged; the
// hub still starts.
func listenMetrics(addr string, hub *metrics.Hub, readiness ...http.Handler) *metricsServer {
	if addr == "" {
		log.Print("metrics: off (metrics_addr is empty)")
		return nil
	}
	if !settings.ValidLoopbackAddr(addr) {
		log.Print("metrics: off (metrics_addr is not a loopback address)")
		return nil
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		log.Print("metrics: off (listener startup failed)")
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", hub.Handler())
	if len(readiness) > 0 && readiness[0] != nil {
		mux.Handle("GET /readyz", readiness[0])
	}
	log.Printf("metrics: serving ops endpoints on %s", l.Addr().String())
	return &metricsServer{listener: l, handler: mux}
}

func (m *metricsServer) Run(ctx context.Context) error {
	srv := &http.Server{Handler: m.handler, ReadHeaderTimeout: metricsHeaderTimeout}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(m.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Print("metrics: listener stopped; /metrics unavailable until restart")
		}
	}()
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), metricsShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
	}
	<-done
	return nil
}
