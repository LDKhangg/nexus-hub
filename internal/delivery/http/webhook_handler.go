package http

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/LDKhangg/nexus-hub/internal/domain"
	"github.com/LDKhangg/nexus-hub/internal/relay"
	"github.com/LDKhangg/nexus-hub/internal/tenant"
	"github.com/labstack/echo/v4"
)

const maxBodyBytes = 1 << 20

type WebhookIntegrationLookup interface {
	LookupWebhookIntegration(context.Context, string) (tenant.WebhookIntegration, error)
}

type WebhookHandler struct {
	integrations WebhookIntegrationLookup
	parsers      VerifiedParserFactory
	acceptor     relay.NoticeAcceptor
}

func NewWebhookHandler(integrations WebhookIntegrationLookup, parsers VerifiedParserFactory, acceptor relay.NoticeAcceptor) *WebhookHandler {
	return &WebhookHandler{integrations: integrations, parsers: parsers, acceptor: acceptor}
}

func (h *WebhookHandler) HandleWebhook(c echo.Context) error {
	respond := func(code int, message string) error { return c.JSON(code, map[string]string{"error": message}) }
	id := c.Param("integrationID")
	if !tenant.ValidUUID(id) {
		return respond(http.StatusBadRequest, "invalid integration")
	}
	if c.Param("source") != "clickup" {
		return respond(http.StatusNotFound, "unknown integration")
	}
	ctx := c.Request().Context()
	integration, err := h.integrations.LookupWebhookIntegration(ctx, id)
	if errors.Is(err, tenant.ErrIntegrationNotFound) {
		return respond(http.StatusNotFound, "unknown integration")
	}
	if err != nil {
		return respond(http.StatusServiceUnavailable, "service unavailable")
	}
	if tenant.ValidateClickUpSource(integration) != nil {
		return respond(http.StatusNotFound, "unknown integration")
	}
	cfg, err := tenant.ParseClickUpNoticeConfig(integration.Config)
	if err != nil {
		return respond(http.StatusServiceUnavailable, "service unavailable")
	}
	parser, err := h.parsers.ForIntegration(integration)
	if err != nil {
		return respond(http.StatusServiceUnavailable, "service unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request().Body, maxBodyBytes+1))
	if err != nil {
		if errors.Is(err, echo.ErrStatusRequestEntityTooLarge) {
			return respond(http.StatusRequestEntityTooLarge, "body too large")
		}
		return respond(http.StatusBadRequest, "invalid body")
	}
	if len(raw) > maxBodyBytes {
		return respond(http.StatusRequestEntityTooLarge, "body too large")
	}
	if parser.Verify(c.Request().Header, raw) != nil {
		return respond(http.StatusUnauthorized, "invalid signature")
	}
	events, err := parser.Parser(raw)
	if errors.Is(err, domain.ErrIgnoredEvent) {
		return c.JSON(http.StatusOK, map[string]string{"status": "ignored"})
	}
	if err != nil {
		return respond(http.StatusBadRequest, "invalid event")
	}
	for i := range events {
		if events[i].Metadata["webhook_id"] != cfg.WebhookID {
			return respond(http.StatusUnauthorized, "invalid signature")
		}
		events[i].WorkspaceID = integration.WorkspaceID
		events[i].IntegrationID = integration.ID
	}
	result, err := h.acceptor.Accept(ctx, events)
	if err != nil {
		return respond(http.StatusServiceUnavailable, "service unavailable")
	}
	status := "queued"
	if result.Accepted == 0 {
		status = "ignored"
		if result.Duplicates > 0 {
			status = "duplicate"
		}
	}
	return c.JSON(http.StatusOK, map[string]any{"status": status, "accepted": result.Accepted, "duplicates": result.Duplicates, "ignored": result.Ignored})
}
