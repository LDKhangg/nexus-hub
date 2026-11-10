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
	"github.com/LDKhangg/nexus-hub/internal/adapters/storage"
	"github.com/LDKhangg/nexus-hub/internal/config"
	httpdelivery "github.com/LDKhangg/nexus-hub/internal/delivery/http"
	"github.com/LDKhangg/nexus-hub/internal/relay"
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

func run() error {
	cfg := config.Load()
	settings, err := config.LoadNoticeSettings()
	if err != nil {
		return err
	}
	startupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	pool, err := storage.NewPostgresPool(startupCtx, cfg.DB.DatabaseURL)
	cancel()
	// Driver connection errors can include credentials; expose only a safe category.
	if err != nil {
		return errors.New("database startup failed")
	}
	defer pool.Close()
	integrations, err := storage.NewIntegrationRepository(pool)
	if err != nil {
		return errors.New("integration repository startup failed")
	}
	notices, err := storage.NewNoticeRepository(pool, func() (string, error) { id, err := uuid.NewV7(); return id.String(), err })
	if err != nil {
		return errors.New("notice repository startup failed")
	}
	sender := telegram.NewClient(&http.Client{Timeout: settings.RequestTimeout}, os.LookupEnv)
	worker := relay.NewNoticeWorker(notices, sender, settings)
	handler := httpdelivery.NewWebhookHandler(integrations, httpdelivery.NewVerifiedParserFactory(os.LookupEnv), relay.NewNoticeService(notices))
	e := echo.New()
	e.HideBanner = true
	e.Use(middleware.RequestLogger())
	e.Use(middleware.Recover())
	e.Use(middleware.BodyLimit(cfg.Server.BodyLimit))
	e.Server.ReadHeaderTimeout = 5 * time.Second
	e.POST("/webhook/:source/:integrationID", handler.HandleWebhook)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runServices(ctx, e, worker, cfg.Server.Port, cfg.Server.ShutdownTimeout)
}

type serviceWorker interface{ Run(context.Context) error }

// runServices owns cancellation and waits for all DB users before returning.
func runServices(ctx context.Context, e *echo.Echo, worker serviceWorker, address string, shutdownTimeout time.Duration) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return errors.New("HTTP listener startup failed")
	}
	e.Listener = listener
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	workerDone := make(chan error, 1)
	serverDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	go func() { serverDone <- e.Start(address) }()
	var result error
	workerStopped := false
	select {
	case <-ctx.Done():
	case err := <-serverDone:
		if !errors.Is(err, http.ErrServerClosed) {
			result = errors.New("HTTP server stopped unexpectedly")
		}
	case <-workerDone:
		workerStopped = true
		result = errors.New("notice worker stopped unexpectedly")
	}
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()
	if err := e.Shutdown(shutdownCtx); err != nil {
		_ = e.Close()
		result = errors.New("HTTP shutdown timed out")
	}
	if !workerStopped {
		<-workerDone
	}
	return result
}
