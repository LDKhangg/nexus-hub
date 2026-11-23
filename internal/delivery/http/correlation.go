package http

import (
	"github.com/LDKhangg/nexus-hub/internal/obs"
	"github.com/labstack/echo/v4"
)

// CorrelationMiddleware assigns a fresh ID to every request. Public routes
// never trust a caller-provided correlation header.
func CorrelationMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		request := c.Request()
		request = request.WithContext(obs.WithCorrelation(request.Context(), obs.NewCorrelationID()))
		c.SetRequest(request)
		return next(c)
	}
}
