package http

import (
	"io"
	"net/http"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/labstack/echo/v4"
)

// là tầng controller, nơi nhận request->phân loại platform để chọn parser

// constructor chứa factory và debounce cần để xử lý
type WebhookHandler struct {
	parserFactory *WebhookFactory
	debouncerMgr  *relay.DebounceManage
}

func NewWebhookHandler(parserFactory *WebhookFactory, debouncerMgr *relay.DebounceManage) *WebhookHandler {
	return &WebhookHandler{
		parserFactory: parserFactory,
		debouncerMgr:  debouncerMgr,
	}
}

func (h *WebhookHandler) HandleWebhook(c echo.Context) error {
	ctx := Context.WithoutCancel(c.Request().Context())

	// lay source tu request

	sourceParam := c.Param("source")
	source := domain.ChannelSource(sourceParam)

	rawBody, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
	}
	parser, err := h.parserFactory.GetParser(source)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	event, err := parser.Parser(rawBody)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to parse event"})
	}

	debouncer := h.debouncerMgr.GetOrCreate(event.Source)
	debouncer.Push(ctx, event)

	return c.JSON(http.StatusOK, map[string]string{"status": "queued"})
}
