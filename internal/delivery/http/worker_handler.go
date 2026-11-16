package http

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/LDKhangg/nexus-hub/internal/behavior"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

// workerBodyLimit caps every worker request body.
const workerBodyLimit = "64K"

const workerDeviceKey = "worker_device"

// WorkerHandler serves the local worker API under /worker/v1. Responses never
// echo tokens, codes or request bodies; errors are fixed strings.
type WorkerHandler struct {
	enroller      behavior.WorkerEnroller
	auth          behavior.WorkerAuthenticator
	deviceLimiter *tokenBuckets
	enrollLimiter *tokenBuckets
}

func NewWorkerHandler(enroller behavior.WorkerEnroller, auth behavior.WorkerAuthenticator) *WorkerHandler {
	return &WorkerHandler{
		enroller: enroller, auth: auth,
		deviceLimiter: newTokenBuckets(20, 5, time.Now),
		enrollLimiter: newTokenBuckets(10, 10.0/60, time.Now),
	}
}

// Register mounts the worker routes. Every route except enroll, including
// unknown paths below /worker/v1, requires a valid device token.
func (h *WorkerHandler) Register(e *echo.Echo) {
	e.POST("/worker/v1/enroll", h.Enroll, middleware.BodyLimit(workerBodyLimit))
	g := e.Group("/worker/v1", middleware.BodyLimit(workerBodyLimit), h.authenticate)
	g.POST("/poll", h.Poll)
}

func workerError(c echo.Context, status int, msg string) error {
	return c.JSON(status, map[string]string{"error": msg})
}

type enrollRequest struct {
	Code         string   `json:"code"`
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
}

type enrollResponse struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	Token    string `json:"token"`
}

func (h *WorkerHandler) Enroll(c echo.Context) error {
	if !h.enrollLimiter.allow("enroll") {
		return workerError(c, http.StatusTooManyRequests, "rate limited")
	}
	var req enrollRequest
	if err := decodeWorkerBody(c, &req); err != nil {
		return workerBodyError(c, err)
	}
	if req.Code == "" || req.Name == "" {
		return workerError(c, http.StatusBadRequest, "invalid request")
	}
	device, token, err := h.enroller.Enroll(c.Request().Context(), req.Code, req.Name, req.Capabilities)
	switch {
	case err == nil:
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.JSON(http.StatusOK, enrollResponse{DeviceID: device.ID, Name: device.Name, Token: token})
	case errors.Is(err, behavior.ErrNotFound):
		return workerError(c, http.StatusForbidden, "enrollment code is not valid")
	case errors.Is(err, behavior.ErrConflict):
		return workerError(c, http.StatusConflict, "device name is already in use")
	case errors.Is(err, behavior.ErrInvalid):
		return workerError(c, http.StatusBadRequest, "invalid request")
	}
	return workerError(c, http.StatusInternalServerError, "enrollment failed")
}

// authenticate requires "Authorization: Bearer <token>" for a live device and
// applies the per-device rate limit.
func (h *WorkerHandler) authenticate(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		header := c.Request().Header.Get("Authorization")
		scheme, token, ok := strings.Cut(header, " ")
		token = strings.TrimSpace(token)
		if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
			return workerError(c, http.StatusUnauthorized, "unauthorized")
		}
		device, err := h.auth.Authenticate(c.Request().Context(), token)
		if err != nil {
			if errors.Is(err, behavior.ErrNotFound) {
				return workerError(c, http.StatusUnauthorized, "unauthorized")
			}
			return workerError(c, http.StatusInternalServerError, "internal error")
		}
		if !h.deviceLimiter.allow(device.ID) {
			c.Response().Header().Set("Retry-After", "1")
			return workerError(c, http.StatusTooManyRequests, "rate limited")
		}
		c.Set(workerDeviceKey, device)
		return next(c)
	}
}

// WorkerDeviceFrom returns the authenticated device set by the middleware.
func WorkerDeviceFrom(c echo.Context) (behavior.WorkerDevice, bool) {
	d, ok := c.Get(workerDeviceKey).(behavior.WorkerDevice)
	return d, ok
}

// Poll is a stub until the lease protocol lands: no job is available.
func (h *WorkerHandler) Poll(c echo.Context) error {
	return c.NoContent(http.StatusNoContent)
}

func decodeWorkerBody(c echo.Context, v any) error {
	body := http.MaxBytesReader(c.Response(), c.Request().Body, 64<<10)
	dec := json.NewDecoder(body)
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}

func workerBodyError(c echo.Context, err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return workerError(c, http.StatusRequestEntityTooLarge, "request too large")
	}
	return workerError(c, http.StatusBadRequest, "invalid request")
}

// tokenBuckets is a small in-memory token bucket per key.
type tokenBuckets struct {
	mu      sync.Mutex
	burst   float64
	rate    float64 // tokens per second
	now     func() time.Time
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newTokenBuckets(burst, ratePerSec float64, now func() time.Time) *tokenBuckets {
	return &tokenBuckets{burst: burst, rate: ratePerSec, now: now, buckets: map[string]*bucket{}}
}

func (t *tokenBuckets) allow(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	b, ok := t.buckets[key]
	if !ok {
		if len(t.buckets) > 4096 {
			t.buckets = map[string]*bucket{} // bounded memory; resets are harmless
		}
		b = &bucket{tokens: t.burst, last: now}
		t.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * t.rate
	if b.tokens > t.burst {
		b.tokens = t.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
