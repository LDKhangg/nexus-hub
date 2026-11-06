package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/LDKhangg/nexus-hub/internal/adapters/sources/clickup"
	"github.com/LDKhangg/nexus-hub/internal/config"
	httpdelivery "github.com/LDKhangg/nexus-hub/internal/delivery/http"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

func main() {
	cfg := config.Load()

	e := echo.New()
	e.HideBanner = true
	e.Use(middleware.RequestLogger())
	e.Use(middleware.Recover())
	e.Use(middleware.BodyLimit(cfg.Server.BodyLimit))

	onFlush := func(ctx context.Context, event domain.NormalizedEvent) {
		log.Printf("[FLUSH] [Workspace: %s][Source: %s][Task/ID: %s] Nội dung gom nhóm:\n----------------------------------------\n%s",
			event.WorkspaceID, event.Source, event.ExternalID, event.Content)
	}
	debounceManage := relay.NewDebounceManage(cfg.Relay.DebounceWindow, cfg.Relay.MaxWait, onFlush)

	log.Println("[WARN] devmode not have public_base_url,clickup_api_key")
	factory := httpdelivery.NewWebhookFactory()
	// TODO(F): lấy wh secret và selfUserID từ clickup lúc khởi động
	factory.Register(domain.SourceClickup, clickup.NewClickUpParser("", ""))
	ingestor := relay.NewIngestor(debounceManage)
	wbHandler := httpdelivery.NewWebhookHandler(factory, ingestor)
	e.POST("/webhook/:source", wbHandler.HandleWebhook)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := e.Start(cfg.Server.Port); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Không thể khởi động server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("Đang shutdown server...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	if err := e.Shutdown(shutdownCtx); err != nil {
		log.Printf("Shutdown lỗi: %v", err)
	}
	debounceManage.FlushAll()
	log.Println("Đã shutdown xong")
}
