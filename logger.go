package runko

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

// newLogger creates a structured JSON logger writing to stdout.
// The logger automatically includes the service name in every entry.
func newLogger(serviceName, level string) *slog.Logger {
	var logLevel slog.Level
	switch strings.ToLower(level) {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn", "warning":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	})

	return slog.New(handler).With("service", serviceName)
}

// LogWithContext returns a logger enriched with request-scoped values
// from the context (request ID, user ID, trace ID). Use this inside
// handlers and business logic to get automatic correlation.
//
// Example:
//
//	log := runko.LogWithContext(app.Logger, r.Context())
//	log.Info("order created", "order_id", orderID)
//	// Output includes request_id, user_id, client_ip automatically.
func LogWithContext(logger *slog.Logger, ctx context.Context) *slog.Logger {
	attrs := make([]any, 0, 8)

	if rid := RequestID(ctx); rid != "" {
		attrs = append(attrs, "request_id", rid)
	}
	if uid := UserID(ctx); uid != "" {
		attrs = append(attrs, "user_id", uid)
	}
	if tid := TraceID(ctx); tid != "" {
		attrs = append(attrs, "trace_id", tid)
	}
	if cip := ClientIP(ctx); cip != "" {
		attrs = append(attrs, "client_ip", cip)
	}

	if len(attrs) == 0 {
		return logger
	}

	return logger.With(attrs...)
}
