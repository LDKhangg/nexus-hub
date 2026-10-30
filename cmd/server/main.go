package main

import (
	"context"
	"log"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/adapters/sources/clickup"
	"github.com/LDKhangg/nexus-hub/internal/delivery/http"
	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

func main() {
	// init new Echo framework
	e := echo.New()
	// 2 fundamental middleware
	e.Use(middleware.RequestLogger())
	e.Use(middleware.Recover())
	// create CallBack func
	onFlushCallback := func(ctx context.Context, event domain.NormalizedEvent) {
		log.Printf("[FLUSH] [Workspace: %s][Source: %s][Task/ID: %s] Nội dung gom nhóm:\n\n---------------------------------------- %s",
			event.WorkspaceID, event.Source, event.ExternalID, event.Content)
	}
	// init debounceManage
	debounceManage := relay.NewDebounceManage(8*time.Second, onFlushCallback)
	// init wh factory
	factory := http.NewWebhookFactory()
	clickupParser := clickup.NewClickUpParser()
	factory.Register(domain.SourceClickup, clickupParser)

	// init wh handler
	wbHandler := http.NewWebhookHandler(factory, debounceManage)
	// register route for echo
	e.POST("/webhook/:source", wbHandler.HandleWebhook)
	// start sever at port 8080
	port := ":8080"
	if err := e.Start(port); err != nil {
		log.Fatalf("Không thể khởi động server: %v", err)
	}
}
