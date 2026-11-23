// Package obs provides structured, privacy-safe application logging.
package obs

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"strings"

	"github.com/LDKhangg/nexus-hub/internal/redact"
	"github.com/google/uuid"
)

type contextKey uint8

const (
	correlationKey contextKey = iota
	executionKey
)

// Setup installs and returns the process JSON logger.
func Setup(level string, w io.Writer) *slog.Logger {
	options := &slog.HandlerOptions{Level: parseLevel(level)}
	handler := &redactingHandler{Handler: slog.NewJSONHandler(w, options), redactor: redact.New(nil), defaultComponent: "nexus-hub"}
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}

// BridgeStdLog routes package log output through the structured handler.
func BridgeStdLog(logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	legacy := slog.New(logger.Handler()).With("component", "legacy")
	log.SetFlags(0)
	log.SetOutput(slog.NewLogLogger(legacy.Handler(), slog.LevelInfo).Writer())
}

// WithCorrelation attaches a correlation ID to ctx.
func WithCorrelation(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey, id)
}

// Correlation returns the correlation ID attached to ctx.
func Correlation(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(correlationKey).(string)
	return id
}

// WithExecution attaches a workflow execution ID to ctx.
func WithExecution(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, executionKey, id)
}

// Logger returns the process logger enriched with identifiers from ctx.
func Logger(ctx context.Context) *slog.Logger {
	logger := slog.Default()
	if id := Correlation(ctx); id != "" {
		logger = logger.With("correlation_id", id)
	}
	if ctx != nil {
		if id, _ := ctx.Value(executionKey).(int64); id > 0 {
			logger = logger.With("execution_id", id)
		}
	}
	return logger
}

// NewCorrelationID returns a time-ordered UUIDv7 correlation ID.
func NewCorrelationID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

type redactingHandler struct {
	slog.Handler
	redactor         *redact.Redactor
	defaultComponent string
	hasComponent     bool
}

func (h *redactingHandler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, h.redactor.RedactSecrets(record.Message), record.PC)
	hasComponent := h.hasComponent
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "component" {
			hasComponent = true
		}
		clean.AddAttrs(h.redactAttr(attr))
		return true
	})
	if !hasComponent {
		clean.AddAttrs(slog.String("component", h.defaultComponent))
	}
	return h.Handler.Handle(ctx, clean)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	hasComponent := h.hasComponent
	for i, attr := range attrs {
		if attr.Key == "component" {
			hasComponent = true
		}
		clean[i] = h.redactAttr(attr)
	}
	return &redactingHandler{Handler: h.Handler.WithAttrs(clean), redactor: h.redactor, defaultComponent: h.defaultComponent, hasComponent: hasComponent}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{Handler: h.Handler.WithGroup(name), redactor: h.redactor, defaultComponent: h.defaultComponent, hasComponent: h.hasComponent}
}

func (h *redactingHandler) redactAttr(attr slog.Attr) slog.Attr {
	attr.Value = attr.Value.Resolve()
	switch attr.Value.Kind() {
	case slog.KindString:
		attr.Value = slog.StringValue(h.redactor.RedactSecrets(attr.Value.String()))
	case slog.KindGroup:
		group := attr.Value.Group()
		for i := range group {
			group[i] = h.redactAttr(group[i])
		}
		attr.Value = slog.GroupValue(group...)
	case slog.KindAny:
		switch value := attr.Value.Any().(type) {
		case error:
			attr.Value = slog.StringValue(h.redactor.RedactSecrets(value.Error()))
		case fmt.Stringer:
			attr.Value = slog.StringValue(h.redactor.RedactSecrets(value.String()))
		case []byte:
			attr.Value = slog.StringValue("[REDACTED]")
		}
	}
	return attr
}
