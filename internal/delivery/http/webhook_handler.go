package http

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/labstack/echo/v4"
)

// maxBodyBytes là chốt chặn thứ hai, ngoài middleware BodyLimit ở main.
const maxBodyBytes = 1 << 20

// WebhookHandler là tầng controller: nhận request -> chọn parser theo source -> đẩy vào debouncer.
type WebhookHandler struct {
	parserFactory *WebhookFactory
	ingestor      *relay.Ingestor
}

func NewWebhookHandler(parserFactory *WebhookFactory, ingestor *relay.Ingestor) *WebhookHandler {
	return &WebhookHandler{parserFactory: parserFactory, ingestor: ingestor}
}

func (h *WebhookHandler) HandleWebhook(c echo.Context) error {
	// Event được flush sau khi request kết thúc, nên bỏ cancel của request.
	ctx := context.WithoutCancel(c.Request().Context())

	source := domain.ChannelSource(c.Param("source"))
	parser, err := h.parserFactory.GetParser(source)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "unsupported source"})
	}

	rawBody, err := io.ReadAll(io.LimitReader(c.Request().Body, maxBodyBytes+1))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	if len(rawBody) > maxBodyBytes {
		return c.JSON(http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
	}

	if v, ok := parser.(SignatureVerifier); ok {
		if err := v.Verify(c.Request().Header, rawBody); err != nil {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid signature"})
		}
	}

	event, err := parser.Parser(rawBody)
	if errors.Is(err, domain.ErrIgnoredEvent) {
		return c.JSON(http.StatusOK, map[string]string{"status": "ignored"})
	}
	if err != nil {
		c.Logger().Warnf("parse %s webhook: %v", source, err)
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "failed to parse event"})
	}

	h.ingestor.Push(ctx, event)
	return c.JSON(http.StatusOK, map[string]string{"status": "queued"})
}
