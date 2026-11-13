package main

import (
	"context"
	"errors"
	"log"
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
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/reporting"
	"github.com/LDKhangg/nexus-hub/internal/secrets"
	"github.com/LDKhangg/nexus-hub/internal/settings"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

const (
	bodyLimit       = "1M"
	shutdownTimeout = 10 * time.Second
	secretCacheTTL  = 5 * time.Minute
)

func run() error {
	cfg := config.Load()
	startupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := storage.NewPostgresPool(startupCtx, cfg.DatabaseURL)
	// Driver connection errors can include credentials; expose only a safe category.
	if err != nil {
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
	credentials, err := storage.NewCredentialStore(pool)
	if err != nil {
		return errors.New("credential store startup failed")
	}
	key, err := secrets.LoadMasterKey(cfg.MasterKeyFile)
	if err != nil {
		// Legacy env references keep working; DB credentials fail visibly.
		log.Printf("master key unavailable (%v); only legacy env credential references will resolve", err)
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
	worker := relay.NewNoticeWorker(notices, sender, workerSettings)
	behaviors, err := storage.NewBehaviorStore(pool)
	if err != nil {
		return errors.New("behavior store startup failed")
	}
	// memory_search reads the workspace's decision memory; it is a read
	// tool of the shared registry like any other.
	ag, err := buildAgent(pool, s, resolver, redactor, sender, behavior.NewMemorySearchTool(behaviors))
	if err != nil {
		return err
	}
	notices.SetKnownTools(ag.known)
	// Telegram inbound: the handler only verifies, gates and stores; the
	// inbox worker does the work. Behavior commands create pending runs;
	// buttons, replies and /undo go to the action engine; /trace answers
	// "not available" until it is wired.
	reportSettings := reporting.NewPostgresStore(pool)
	telegramHandler := httpdelivery.NewTelegramHandler(integrations, resolver, behaviors, behaviors, s.OwnerTelegramChatID, reportSettings).WithOwnerUser(s.OwnerTelegramUserID)
	// The Telegram assistant (telegram_assistant): the owner's plain free
	// text reaches the worker in memory only, then the triage model.
	var assistant *behavior.Assistant
	if s.TelegramAssistant {
		assistant = &behavior.Assistant{Models: ag.provider, Model: s.TriageModel, Runs: behaviors, Inbox: behavior.NewAssistantInbox()}
		telegramHandler.WithAssistant(assistant.Inbox)
	}
	commands, err := behavior.NewCommandRunner(behaviors, behaviors)
	if err != nil {
		return errors.New("behavior command runner startup failed")
	}
	messenger := telegram.NewMessenger(sender, integrations)
	runs, engine, err := buildBehaviorEngine(pool, s, behaviors, integrations, ag, redactor, resolver, messenger)
	if err != nil {
		return err
	}
	inbox := behavior.NewInboxWorker(behavior.InboxDeps{
		Inbox: behaviors, Approvals: behaviors, Defs: behaviors, Messenger: messenger,
		Commands: commands, Undoer: engine, Callbacks: engine,
		Tracer:    &behavior.TraceService{Store: behaviors, Profiles: behaviors, People: ag.people},
		People:    ag.people,
		Reports:   reportSettings,
		Assistant: assistant,
	}, behavior.InboxSettings{PollInterval: s.NoticePollInterval, RequestTimeout: s.NoticeRequestTimeout})
	workspaces, err := storage.NewWorkspaceRepository(pool)
	if err != nil {
		return errors.New("workspace repository startup failed")
	}
	chatPoller := buildChatPoller(s, behaviors, workspaces, integrations, ag.known, resolver, messenger)
	services := []serviceWorker{worker, ag.orchestrator, ag.actions, inbox, runs, engine, chatPoller}
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
	e.Use(middleware.RequestLogger())
	e.Use(middleware.Recover())
	e.Use(middleware.BodyLimit(bodyLimit))
	e.Server.ReadHeaderTimeout = 5 * time.Second
	e.POST("/webhook/telegram/:integrationID", telegramHandler.Handle)
	e.POST("/webhook/:source/:integrationID", handler.HandleWebhook)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runServices(ctx, e, services, s.ListenAddr, shutdownTimeout)
}

type serviceWorker interface{ Run(context.Context) error }

// runServices owns cancellation: any worker or the server stopping shuts
// everything down, and it waits for every DB user before returning.
func runServices(ctx context.Context, e *echo.Echo, workers []serviceWorker, address string, shutdownTimeout time.Duration) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return errors.New("HTTP listener startup failed")
	}
	e.Listener = listener
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	workerDone := make(chan error, len(workers))
	serverDone := make(chan error, 1)
	for _, w := range workers {
		go func() { workerDone <- w.Run(workerCtx) }()
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
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()
	if err := e.Shutdown(shutdownCtx); err != nil {
		_ = e.Close()
		result = errors.New("HTTP shutdown timed out")
	}
	for ; stopped < len(workers); stopped++ {
		<-workerDone
	}
	return result
}
