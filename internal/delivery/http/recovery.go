package http

import (
	"net/http"

	"github.com/LDKhangg/nexus-hub/internal/obs"
	"github.com/labstack/echo/v4"
)

// Recover handles panics without exposing the panic value or stack.
func Recover(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) (err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				request := c.Request()
				obs.Logger(request.Context()).Error("http handler panic",
					"component", "http",
					"error_code", "panic",
					"method", request.Method,
					"route", c.Path(),
					"status", http.StatusInternalServerError,
				)
				if !c.Response().Committed {
					err = c.NoContent(http.StatusInternalServerError)
				}
			}
		}()
		return next(c)
	}
}
