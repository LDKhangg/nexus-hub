package http

import (
	"crypto/sha256"
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

// Device tokens are "nhw_" plus 32 random bytes in unpadded base64url
// (storage.randomWorkerToken). Anything else is refused before the DB.
const (
	workerTokenPrefix = "nhw_"
	workerTokenLen    = len(workerTokenPrefix) + 43
	authFailureKey    = "auth_failure"
)

// WorkerHandler serves the local worker API under /worker/v1. Responses never
// echo tokens, codes or request bodies; errors are fixed strings.
type WorkerHandler struct {
	enroller      behavior.WorkerEnroller
	auth          behavior.WorkerAuthenticator
	deviceLimiter *tokenBuckets
	enrollLimiter *tokenBuckets
	authFailures  *tokenBuckets                    // global budget for failed authentications (R-3)
	knownTokens   *knownTokenSet                   // hashes of tokens that authenticated; skip authFailures
	leases        behavior.WorkerLeases            // nil: poll always answers 204
	instructions  behavior.WorkerInstructionSource // nil: no instructions
	pollWait      time.Duration                    // long-poll cap (W6)
	pollRecheck   time.Duration                    // re-check interval while waiting
	stopping      chan struct{}                    // closed on server shutdown: polls end with 204
	stopOnce      sync.Once
}

func NewWorkerHandler(enroller behavior.WorkerEnroller, auth behavior.WorkerAuthenticator) *WorkerHandler {
	return &WorkerHandler{
		enroller: enroller, auth: auth,
		deviceLimiter: newTokenBuckets(20, 5, time.Now),
		enrollLimiter: newTokenBuckets(10, 10.0/60, time.Now),
		authFailures:  newTokenBuckets(30, 1, time.Now),
		knownTokens:   newKnownTokenSet(256, 24*time.Hour, time.Now),
		pollWait:      behavior.WorkerPollMaxWait,
		pollRecheck:   3 * time.Second,
		stopping:      make(chan struct{}),
	}
}

// WithLeases enables the lease protocol. instructions may be nil.
func (h *WorkerHandler) WithLeases(leases behavior.WorkerLeases, instructions behavior.WorkerInstructionSource) *WorkerHandler {
	h.leases = leases
	h.instructions = instructions
	return h
}

// Shutdown ends every waiting poll with 204 so the worker reconnects and
// the HTTP server can drain (R-2). It is safe to call more than once.
func (h *WorkerHandler) Shutdown() {
	h.stopOnce.Do(func() { close(h.stopping) })
}

// Register mounts the worker routes. Every route except enroll, including
// unknown paths below /worker/v1, requires a valid device token. Waiting
// polls end when e's HTTP server shuts down.
func (h *WorkerHandler) Register(e *echo.Echo) {
	if e.Server != nil {
		e.Server.RegisterOnShutdown(h.Shutdown)
	}
	e.POST("/worker/v1/enroll", h.Enroll, middleware.BodyLimit(workerBodyLimit))
	g := e.Group("/worker/v1", middleware.BodyLimit(workerBodyLimit), h.authenticate)
	g.POST("/poll", h.Poll)
	g.POST("/jobs/:id/heartbeat", h.Heartbeat)
	g.POST("/jobs/:id/progress", h.Progress)
	g.POST("/jobs/:id/result", h.Result)
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
// applies the per-device rate limit. Malformed tokens are refused without a
// DB call; while the global budget for failed authentications is empty every
// lookup of an unknown token is refused with 429. A token that authenticated
// recently skips that budget but is still checked against the DB.
func (h *WorkerHandler) authenticate(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		header := c.Request().Header.Get("Authorization")
		scheme, token, ok := strings.Cut(header, " ")
		token = strings.TrimSpace(token)
		if !ok || !strings.EqualFold(scheme, "Bearer") || !wellFormedWorkerToken(token) {
			return workerError(c, http.StatusUnauthorized, "unauthorized")
		}
		hash := sha256.Sum256([]byte(token))
		known := h.knownTokens.has(hash)
		if !known && h.authFailures.empty(authFailureKey) {
			c.Response().Header().Set("Retry-After", "1")
			return workerError(c, http.StatusTooManyRequests, "rate limited")
		}
		device, err := h.auth.Authenticate(c.Request().Context(), token)
		if err != nil {
			if errors.Is(err, behavior.ErrNotFound) {
				if known {
					h.knownTokens.remove(hash) // revoked or rotated: back to the bucket
				}
				h.authFailures.allow(authFailureKey)
				return workerError(c, http.StatusUnauthorized, "unauthorized")
			}
			return workerError(c, http.StatusInternalServerError, "internal error")
		}
		h.knownTokens.add(hash)
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

func decodeWorkerBody(c echo.Context, v any) error {
	return decodeWorkerJSON(c, v, false)
}

// decodeWorkerJSON decodes one JSON value; an empty body is allowed only
// when optional is set.
func decodeWorkerJSON(c echo.Context, v any, optional bool) error {
	body := http.MaxBytesReader(c.Response(), c.Request().Body, 64<<10)
	dec := json.NewDecoder(body)
	if err := dec.Decode(v); err != nil {
		if optional && errors.Is(err, io.EOF) {
			return nil
		}
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

// wellFormedWorkerToken checks the device token shape without a DB call.
func wellFormedWorkerToken(token string) bool {
	if len(token) != workerTokenLen || !strings.HasPrefix(token, workerTokenPrefix) {
		return false
	}
	for _, r := range token[len(workerTokenPrefix):] {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
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

// empty reports whether key has no whole token left, without taking one.
func (t *tokenBuckets) empty(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	b, ok := t.buckets[key]
	if !ok {
		return false
	}
	now := t.now()
	tokens := b.tokens + now.Sub(b.last).Seconds()*t.rate
	return tokens < 1
}

// knownTokenSet holds SHA-256 hashes of device tokens that authenticated
// recently, so a flood of bad tokens cannot lock out a known device. A known
// token still goes through Authenticate on every request, so a revoke takes
// effect at once. Bounded in size and age; hashes are never logged.
type knownTokenSet struct {
	mu      sync.Mutex
	max     int
	ttl     time.Duration
	now     func() time.Time
	entries map[[sha256.Size]byte]time.Time // hash -> added at
}

func newKnownTokenSet(max int, ttl time.Duration, now func() time.Time) *knownTokenSet {
	return &knownTokenSet{max: max, ttl: ttl, now: now, entries: map[[sha256.Size]byte]time.Time{}}
}

func (k *knownTokenSet) has(hash [sha256.Size]byte) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	at, ok := k.entries[hash]
	if !ok {
		return false
	}
	if k.now().Sub(at) > k.ttl {
		delete(k.entries, hash)
		return false
	}
	return true
}

// add records hash, refreshing its age. When full, expired entries go first,
// then the oldest one.
func (k *knownTokenSet) add(hash [sha256.Size]byte) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	if _, ok := k.entries[hash]; !ok && len(k.entries) >= k.max {
		var oldest [sha256.Size]byte
		var oldestAt time.Time
		first := true
		for h, at := range k.entries {
			if now.Sub(at) > k.ttl {
				delete(k.entries, h)
				continue
			}
			if first || at.Before(oldestAt) {
				oldest, oldestAt, first = h, at, false
			}
		}
		if len(k.entries) >= k.max {
			delete(k.entries, oldest)
		}
	}
	k.entries[hash] = now
}

func (k *knownTokenSet) remove(hash [sha256.Size]byte) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.entries, hash)
}
