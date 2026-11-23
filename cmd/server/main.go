package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/control/telegram"
	"github.com/LDKhangg/nexus-hub/internal/adapters/netguard"
	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/agent"
	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/LDKhangg/nexus-hub/internal/config"
	httpdelivery "github.com/LDKhangg/nexus-hub/internal/delivery/http"
	"github.com/LDKhangg/nexus-hub/internal/metrics"
	"github.com/LDKhangg/nexus-hub/internal/obs"
	"github.com/LDKhangg/nexus-hub/internal/orchestrator"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/reporting"
	"github.com/LDKhangg/nexus-hub/internal/secrets"
	"github.com/LDKhangg/nexus-hub/internal/settings"
	"github.com/LDKhangg/nexus-hub/internal/workflow"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

func main() {
	logger := obs.Setup("info", os.Stderr)
	obs.BridgeStdLog(logger)
	if err := run(); err != nil {
		slog.Error("server stopped", "error_code", "startup_failed")
		os.Exit(1)
	}
}

const (
	bodyLimit      = "1M"
	secretCacheTTL = 5 * time.Minute
	// startupRecoveryTimeout bounds the recovery passes run before the hub
	// serves; a pass cut off here is repeated by its own sweeper.
	startupRecoveryTimeout = 30 * time.Second
	// stopGrace is how long shutdown waits, after shutdown_timeout ended
	// and in-flight work was cancelled, before it logs the workers that are
	// still stopping.
	stopGrace = 3 * time.Second
)

func run() error {
	cfg := config.Load()
	startupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := storage.NewPostgresPool(startupCtx, cfg.DatabaseURL)
	// Driver connection errors can include credentials; expose only a safe category.
	if err != nil {
		slog.Error("startup dependency unavailable", "component", "startup", "check", "db", "error_code", "database_unavailable")
		return errors.New("database startup failed")
	}
	defer pool.Close()
	settingsStore, err := storage.NewSettingsStore(pool)
	if err != nil {
		return errors.New("settings store startup failed")
	}
	s, err := settings.Load(startupCtx, settingsStore)
	if err != nil {
		return err
	}
	logger := obs.Setup(s.LogLevel, os.Stderr)
	obs.BridgeStdLog(logger)
	if s.AllowDangerous {
		log.Print("setting allow_dangerous is deprecated and has no effect; the policy rule set decides dangerous tools (nh policy)")
	}
	// Observability (Phase 12 B1): metrics, the LLM spend ledger and the
	// llm_daily_budget_usd guard.
	ops, err := buildOps(pool, s)
	if err != nil {
		return err
	}
	credentials, err := storage.NewCredentialStore(pool)
	if err != nil {
		return errors.New("credential store startup failed")
	}
	key, err := secrets.LoadMasterKey(cfg.MasterKeyFile)
	if err != nil {
		slog.Warn("startup dependency unavailable", "component", "startup", "check", "master_key", "error_code", "master_key_unavailable")
		key = nil
	}
	redactor := agent.NewRedactor(nil)
	resolver := secrets.NewResolver(credentials, key, redactor, os.LookupEnv, secretCacheTTL, time.Now)
	integrations, err := storage.NewIntegrationRepository(pool)
	if err != nil {
		return errors.New("integration repository startup failed")
	}
	notices, err := storage.NewNoticeRepository(pool, func() (string, error) { id, err := uuid.NewV7(); return id.String(), err }, storage.DebounceSettings{Window: s.DebounceWindow, MaxWait: s.DebounceMaxWait})
	if err != nil {
		return errors.New("notice repository startup failed")
	}
	if err := ensurePrivateDirs(s.ReposDir, s.TmpDir); err != nil {
		return err
	}
	workerSettings := relay.WorkerSettings{PollInterval: s.NoticePollInterval, RequestTimeout: s.NoticeRequestTimeout}
	sender := telegram.NewClient(netguard.Client(s.NoticeRequestTimeout, netguard.DefaultHosts), resolver)
	worker := relay.NewNoticeWorker(meteredNoticeStore{NoticeDeliveryStore: notices, hub: ops.hub}, sender, workerSettings)
	behaviors, err := storage.NewBehaviorStore(pool)
	if err != nil {
		return errors.New("behavior store startup failed")
	}
	// memory_search reads the workspace's decision memory; it is a read
	// tool of the shared registry like any other.
	ag, err := buildAgent(pool, s, resolver, redactor, sender, ops, behavior.NewMemorySearchTool(behaviors))
	if err != nil {
		return err
	}
	defer ag.gitRunner.Close()
	notices.SetKnownTools(ag.known)
	// Telegram inbound: the handler only verifies, gates and stores; the
	// inbox worker does the work. Behavior commands create pending runs;
	// buttons, replies and /undo go to the action engine; /trace answers
	// "not available" until it is wired.
	reportSettings := reporting.NewPostgresStore(pool)
	telegramHandler := httpdelivery.NewTelegramHandler(integrations, resolver, behaviors, behaviors, s.OwnerTelegramChatID, reportSettings).WithOwnerUser(s.OwnerTelegramUserID)
	commands, err := behavior.NewCommandRunner(behaviors, behaviors)
	if err != nil {
		return errors.New("behavior command runner startup failed")
	}
	messenger := telegram.NewMessenger(sender, integrations)
	runs, engine, err := buildBehaviorEngine(pool, s, behaviors, integrations, ag, redactor, resolver, messenger)
	if err != nil {
		return err
	}
	// Topics: execution messages go to per-task forum topics; any topic
	// failure sends to General instead.
	topics, err := buildTopicRouter(pool, s, integrations, ag, resolver, messenger)
	if err != nil {
		return err
	}
	engine.SetTopics(topics)
	// ClickUp drafts: every hub-written ClickUp comment asks the owner first.
	clickUpDrafts, err := buildClickUpDrafts(pool, s, behaviors, integrations, ag, redactor, resolver, messenger)
	if err != nil {
		return err
	}
	// ClickUp notices of a task with a topic go to that topic (Phase 10).
	worker.SetNoticeRouter(topics)
	commands.SetTaskResolver(topics)
	ag.actions.SetThreadRouter(topics)
	workspaces, err := storage.NewWorkspaceRepository(pool)
	if err != nil {
		return errors.New("workspace repository startup failed")
	}
	chatPoller := buildChatPoller(s, behaviors, workspaces, integrations, ag.known, resolver, messenger)
	// The durable workflow runtime schedules cut-over workspaces; the legacy
	// behavior run and agent job claimers keep running for workspaces still
	// in legacy runtime mode (the runtime-mode fence keeps them apart). The
	// outbound, digest, chat-poll and Telegram assistant workers are
	// unchanged.
	// Local worker bridge: jobs, thresholds, and the local-task step that
	// hands one job to a worker and waits for it.
	workerStore, err := storage.NewWorkerStore(pool)
	if err != nil {
		return errors.New("worker store startup failed")
	}
	thresholds := behavior.WorkerThresholds{
		MinClaudeLeftPct: s.WorkerMinClaudeLeft, MinDiskFreeBytes: s.WorkerMinDiskFree, MaxRAMUsedPct: s.WorkerMaxRAMUsed,
	}
	workerStore.SetAssignmentThresholds(thresholds)
	// Policy gates (Phase 9): the owner's "Duyệt" / "Từ chối" of an action
	// the policy engine holds for approval. Its sweep closes the gates of
	// ended executions (review B R-B-1).
	policyGates := &behavior.PolicyGates{
		Store: ag.policyStore, Messenger: messenger,
		OwnerTelegramUserID: s.OwnerTelegramUserID, RequestTimeout: s.NoticeRequestTimeout,
	}
	var (
		taskStore      *storage.WorkerTaskStore
		workerNotifier *behavior.WorkerNotifier
		orchPorts      orchestratorPorts
		// The report button of a final report (Phase 10 REQ-9) reads the
		// stored result of an ended /task or /plan execution.
		reports = &behavior.ExecutionReports{}
	)
	// Phase 12 REQ-10: stuck and failed workflow alerts to the General
	// topic. A step failed for good asks for an early scan; the database
	// scan stays the source of truth.
	alerts := &behavior.WorkflowAlerts{
		Store: ops.store, Chats: ownerChats{integrations: integrations, ownerChatID: s.OwnerTelegramChatID},
		Messenger: messenger, Metrics: ops.hub, Enabled: s.AlertsEnabled,
		StuckRunning: time.Duration(s.AlertStuckRunningMinutes) * time.Minute,
		StuckQueued:  time.Duration(s.AlertStuckQueuedMinutes) * time.Minute,
		SendTimeout:  s.NoticeRequestTimeout,
	}
	retentionStore, err := storage.NewRetentionStore(pool)
	if err != nil {
		return errors.New("retention store startup failed")
	}
	retention := &behavior.Retention{
		Store: retentionStore, Metrics: ops.hub, Enabled: s.RetentionEnabled,
		Rules: behavior.RetentionRules{
			MessageLogsDays: s.RetentionMessageLogsDays, ToolRunsDays: s.RetentionToolRunsDays,
			PolicyDecisionsDays: s.RetentionPolicyDecisionsDays, WorkflowDays: s.RetentionWorkflowDays,
			OutboundDays: s.RetentionOutboundDays,
		},
	}
	stepObserver := ops.stepFailureObserver()
	onStepFailure := func(reason workflow.ReasonCode, final bool) {
		stepObserver(reason, final)
		if final {
			alerts.Kick()
		}
	}
	workflows, err := buildWorkflowRuntime(pool, s, ag, behaviors, runs, engine, onStepFailure, func(reg *workflow.Registry, store *storage.WorkflowStore) error {
		var err error
		if taskStore, err = storage.NewWorkerTaskStore(pool, workerStore, store); err != nil {
			return err
		}
		reports.Executions, reports.Routes, reports.Jobs = store, taskStore, workerStore
		policyGates.Waker = taskStore
		workerNotifier = behavior.NewWorkerNotifier(behavior.WorkerNotifierDeps{
			Routes: taskStore, Messenger: messenger, Topics: topics, Approvals: behaviors,
			Devices: workerStore, Telemetry: workerStore, Thresholds: thresholds, Waker: taskStore,
			ReportButtons: reports,
		})
		step := &behavior.WorkerRunStep{Jobs: workerStore, Routes: taskStore, Instructions: ag.conversations, Queued: workerNotifier}
		if err := step.Register(reg); err != nil {
			return err
		}
		return registerOrchestratorSteps(&orchPorts, reg, pool, store, s, ag, behaviors, messenger, topics, workerStore, taskStore, integrations, redactor, clickUpDrafts, workerNotifier, policyGates)
	})
	if err != nil {
		return err
	}
	reports.PlanRoutes, reports.Plans = orchPorts.Routes, orchPorts.Plans
	workerStore.SetObserver(workerNotifier)
	// The Telegram assistant keeps the owner's conversation in memory and
	// uses the configured provider's tool-calling path. Workflow reads and
	// commands share the durable runtime store; writes still require the
	// existing Làm/Huỷ approval callback.
	var assistant *behavior.Assistant
	if s.TelegramAssistant {
		assistant = &behavior.Assistant{
			Models: ag.provider, Model: s.AssistantModel, FastModel: s.AssistantFastModel, DeepModel: s.AssistantDeepModel,
			HistoryTurns: int(s.AssistantHistoryTurns), Runs: behaviors, Inbox: behavior.NewAssistantInbox(),
			Workflows: workflows.store, WorkflowCommands: workflows.commands, Redact: redactor.RedactSecrets,
			Conversations: ag.conversations, SummaryModel: s.TriageModel,
			SummarySanitize: func(text string) string {
				return redactor.RedactSecrets(agent.CleanInput(text))
			},
			Timeout: s.AssistantTimeout, RatePerMinute: int(s.AssistantRatePerMinute),
		}
		telegramHandler.WithAssistant(assistant.Inbox)
		telegramHandler.WithReplyTasks(behavior.ReplyTasks{Conversations: ag.conversations, Links: topics})
	}
	inbox := behavior.NewInboxWorker(behavior.InboxDeps{
		Workers: workerStore, WorkerTelemetry: workerStore, LocalTasks: taskStore, WorkerJobs: workerStore, WorkerNotifier: workerNotifier,
		Inbox: behaviors, Approvals: behaviors, Defs: behaviors, Messenger: messenger,
		Commands: commands, Undoer: engine, Callbacks: engine,
		Tracer:            &behavior.TraceService{Store: behaviors, Profiles: behaviors, People: ag.people},
		People:            ag.people,
		Reports:           reportSettings,
		Assistant:         assistant,
		Conversations:     ag.conversations,
		Workflows:         workflows.store,
		WorkflowCommands:  workflows.commands,
		Topics:            topics,
		Links:             topics,
		InstructionWaker:  &orchestrator.InstructionWaker{Steps: workflows.store, Waker: taskStore},
		OrchestratedTasks: orchPorts.Tasks, OrchestratorPlans: orchPorts.Plans,
		ClickUpDrafts: clickUpDrafts, ReportDrafts: clickUpDrafts, CommentLinks: topics,
		ExecutionReports: reports, PolicyGates: policyGates,
	}, behavior.InboxSettings{PollInterval: s.NoticePollInterval, RequestTimeout: s.NoticeRequestTimeout, CommandTopic: s.TelegramCommandTopic})
	// A done/closed ClickUp task closes its topic once no execution of it is
	// active (status events and a periodic sweep).
	lifecycle, err := buildTopicLifecycle(pool, s, integrations, ag, resolver, messenger, workflows.store, workspaces)
	if err != nil {
		return err
	}
	notices.SetStatusObserver(lifecycle.StatusChanged)
	topics.OnTaskTopicOpen(lifecycle.TopicReopened)
	services := []serviceWorker{worker, ag.orchestrator, ag.actions, inbox, runs, engine, chatPoller, workflows, lifecycle, &behavior.WorkerSweeper{Store: workerStore}, workerNotifier, &behavior.WorkerTaskSweeper{Store: taskStore}, clickUpDrafts, policyGates, alerts, retention}
	// Telegram updates arrive by webhook, or by long polling when the hub
	// has no public URL (telegram_mode, see settings.TelegramPolling). Both
	// paths end in the same handler.
	if s.TelegramPolling() {
		log.Print("telegram: receiving updates by long polling")
		services = append(services, buildTelegramPoller(workspaces, integrations, resolver, telegramHandler))
	} else {
		log.Print("telegram: receiving updates by webhook")
	}
	handler := httpdelivery.NewWebhookHandler(integrations, httpdelivery.NewVerifiedParserFactory(resolver), relay.NewNoticeService(notices))
	e := echo.New()
	e.HideBanner = true
	e.Use(httpdelivery.CorrelationMiddleware)
	e.Use(structuredRequestLogger)
	e.Use(middleware.Recover())
	e.Use(middleware.BodyLimit(bodyLimit))
	e.Server.ReadHeaderTimeout = 5 * time.Second
	e.Match([]string{http.MethodGet, http.MethodHead}, "/healthz", func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})
	httpdelivery.NewWorkerHandler(workerStore, workerStore).WithLeases(workerStore, &behavior.LocalTaskInstructions{Routes: taskStore, Reader: ag.conversations}).Register(e)
	e.POST("/webhook/telegram/:integrationID", telegramHandler.Handle, webhookMetrics(ops.hub, metrics.SourceTelegram))
	e.POST("/webhook/:source/:integrationID", handler.HandleWebhook, webhookMetrics(ops.hub, ""))
	// /metrics is never on the public server: its own loopback listener
	// (metrics_addr) stops with the background workers.
	readiness := obs.NewReadiness(obs.ReadinessDeps{
		PingDB: pool.Ping,
		MigrationVersion: func(ctx context.Context) (int64, bool, error) {
			var version int64
			var applied bool
			err := pool.QueryRow(ctx, "SELECT version_id, is_applied FROM goose_db_version ORDER BY id DESC LIMIT 1").Scan(&version, &applied)
			return version, applied, err
		},
		MasterKeyLoaded:  key != nil,
		WorkflowLastPoll: workflows.worker.LastPoll,
		WorkflowInterval: workflows.worker.PollInterval(),
		GitSandboxReady:  ag.gitRunner.Available(),
	})
	startupReadyCtx, cancelStartupReady := context.WithTimeout(context.Background(), 2*time.Second)
	startupReadiness := readiness.Check(startupReadyCtx)
	cancelStartupReady()
	slog.Info("readiness summary", "component", "startup", "status", startupReadiness.Status,
		"db", startupReadiness.Checks.DB, "migrations", startupReadiness.Checks.Migrations,
		"master_key", startupReadiness.Checks.MasterKey, "workflow_worker", startupReadiness.Checks.WorkflowWorker,
		"git_sandbox", startupReadiness.Checks.GitSandbox)
	if m := listenMetrics(s.MetricsAddr, ops.hub, readiness.Handler()); m != nil {
		services = append(services, m)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Crash resume (REQ-3): every lease, wait and post a crash left behind
	// is repaired once before the hub serves. Each service repeats its pass
	// when it starts; all passes are idempotent.
	recoverAtStartup(ctx, []recoveryPass{
		{"workflow_reconcile", workflows.Recover},
		{"worker_leases", func(ctx context.Context) error { _, err := workerStore.ExpireLeases(ctx); return err }},
		{"worker_tasks", taskStore.SyncWorkerTasks},
		{"behavior_approvals", func(ctx context.Context) error { _, err := engine.Sweep(ctx); return err }},
		{"policy_gates", func(ctx context.Context) error { _, err := policyGates.Sweep(ctx); return err }},
		{"clickup_drafts", clickUpDrafts.Sweep},
	}, startupRecoveryTimeout)
	return runServices(ctx, e, services, s.ListenAddr, s.ShutdownTimeout)
}

// recoveryPass is one named startup repair.
type recoveryPass struct {
	name string
	run  func(context.Context) error
}

// recoverAtStartup runs each pass in order within timeout. A failed pass is
// logged by name only (never record contents) and does not stop startup:
// its sweeper retries it on its own period.
func recoverAtStartup(ctx context.Context, passes []recoveryPass, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	failed := 0
	for _, p := range passes {
		if ctx.Err() != nil {
			failed++
			continue
		}
		if err := p.run(ctx); err != nil {
			failed++
			slog.Warn("startup recovery not completed", "pass", p.name, "retry", true)
		}
	}
	slog.Info("startup recovery completed", "passes", len(passes), "failed", failed, "duration_ms", time.Since(started).Milliseconds())
}

type serviceWorker interface{ Run(context.Context) error }

// drainingService is a worker that can finish in-flight work on shutdown:
// it stops claiming when stop ends and runs claimed work with hard, which
// ends only when shutdown_timeout runs out.
type drainingService interface {
	RunDrain(stop, hard context.Context) error
}

// runServices owns cancellation: any worker or the server stopping shuts
// everything down, and it waits for every DB user before returning.
//
// Shutdown order (REQ-4), all within shutdownTimeout: the HTTP server stops
// accepting and lets in-flight requests finish (webhooks are stored before
// their 2xx, so nothing acknowledged is lost); then every background loop
// stops claiming new work; then in-flight job steps get the rest of the
// timeout to finish. When it runs out, in-flight work is cancelled, and a
// cut-off workflow step releases its own lease so the next start resumes
// it. A shutdown by signal returns nil (exit 0).
func runServices(ctx context.Context, e *echo.Echo, workers []serviceWorker, address string, shutdownTimeout time.Duration) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return errors.New("HTTP listener startup failed")
	}
	e.Listener = listener
	stopCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()
	hardCtx, cancelInFlight := context.WithCancel(context.Background())
	defer cancelInFlight()
	workerDone := make(chan error, len(workers))
	serverDone := make(chan error, 1)
	for _, w := range workers {
		if d, ok := w.(drainingService); ok {
			go func() { workerDone <- d.RunDrain(stopCtx, hardCtx) }()
			continue
		}
		go func() { workerDone <- w.Run(stopCtx) }()
	}
	go func() { serverDone <- e.Start(address) }()
	var result error
	stopped := 0
	select {
	case <-ctx.Done():
	case err := <-serverDone:
		if !errors.Is(err, http.ErrServerClosed) {
			result = errors.New("HTTP server stopped unexpectedly")
		}
	case <-workerDone:
		stopped = 1
		result = errors.New("worker stopped unexpectedly")
	}
	started := time.Now()
	deadline := started.Add(shutdownTimeout)
	shutdownCtx, shutdownCancel := context.WithDeadline(context.Background(), deadline)
	defer shutdownCancel()
	if err := e.Shutdown(shutdownCtx); err != nil {
		_ = e.Close()
		result = errors.New("HTTP shutdown timed out")
	}
	stopWorkers()
	timedOut := false
	drainTimer := time.NewTimer(time.Until(deadline))
	defer drainTimer.Stop()
	for stopped < len(workers) {
		select {
		case <-workerDone:
			stopped++
		case <-drainTimer.C:
			timedOut = true
			cancelInFlight()
			slog.Warn("shutdown timeout reached", "in_flight_cancelled", true, "workers_running", len(workers)-stopped)
			stopped += waitWorkers(workerDone, len(workers)-stopped, stopGrace)
		}
	}
	cancelInFlight()
	slog.Info("shutdown completed", "duration_ms", time.Since(started).Milliseconds(), "timed_out", timedOut)
	return result
}

// waitWorkers waits for n workers to stop, logging once (count only) when
// some still run after grace; it returns n, because the caller must not
// close the database while any worker uses it.
func waitWorkers(done <-chan error, n int, grace time.Duration) int {
	t := time.NewTimer(grace)
	defer t.Stop()
	for left := n; left > 0; {
		select {
		case <-done:
			left--
		case <-t.C:
			slog.Warn("shutdown workers still stopping after grace", "workers_running", left)
		}
	}
	return n
}

func structuredRequestLogger(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		started := time.Now()
		err := next(c)
		if err != nil {
			c.Error(err)
		}
		path := c.Path()
		if path == "" {
			path = "unmatched"
		}
		obs.Logger(c.Request().Context()).Info("http request",
			"method", c.Request().Method,
			"path", path,
			"status", c.Response().Status,
			"duration_ms", time.Since(started).Milliseconds(),
		)
		return nil
	}
}
