package runko

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"
)

// contextKey is an unexported type for context keys to prevent collisions.
type contextKey int

const (
	ctxKeyRequestID contextKey = iota
	ctxKeyRequestStart
	ctxKeyUserID
	ctxKeyTraceID
	ctxKeyClientIP
)

// RequestID extracts the request ID from the context.
// Returns empty string if not set.
func RequestID(ctx context.Context) string {
	val, _ := ctx.Value(ctxKeyRequestID).(string)
	return val
}

// RequestStart extracts the request start time from the context.
func RequestStart(ctx context.Context) time.Time {
	val, _ := ctx.Value(ctxKeyRequestStart).(time.Time)
	return val
}

// UserID extracts the authenticated user ID from the context.
// Returns empty string if not authenticated.
func UserID(ctx context.Context) string {
	val, _ := ctx.Value(ctxKeyUserID).(string)
	return val
}

// TraceID extracts the distributed trace ID from the context.
func TraceID(ctx context.Context) string {
	val, _ := ctx.Value(ctxKeyTraceID).(string)
	return val
}

// ClientIP extracts the resolved client IP from the context.
// This is the real client IP after trusted proxy resolution. (CONV-01)
// Returns empty string if the ClientIP middleware has not run.
func ClientIP(ctx context.Context) string {
	val, _ := ctx.Value(ctxKeyClientIP).(string)
	return val
}

// WithRequestID returns a new context with the given request ID.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

// WithRequestStart returns a new context with the request start time.
func WithRequestStart(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, ctxKeyRequestStart, t)
}

// WithUserID returns a new context with the authenticated user ID.
func WithUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyUserID, id)
}

// WithTraceID returns a new context with a distributed trace ID.
func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyTraceID, id)
}

// WithClientIP returns a new context with the resolved client IP.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, ctxKeyClientIP, ip)
}

// RequestIDFromHeader extracts or generates a request ID.
// If the incoming request has a valid X-Request-ID header, it's reused
// (preserving trace continuity across services). Otherwise a new one
// is generated.
//
// The header value is validated: only alphanumeric characters, hyphens,
// and underscores are accepted, with a maximum length of 64 characters.
// Invalid values are silently replaced with a generated ID to prevent
// log injection attacks. (CONV-02)
func RequestIDFromHeader(r *http.Request) string {
	id := sanitizeID(r.Header.Get("X-Request-ID"))
	if id != "" {
		return id
	}
	return generateID()
}

// TraceIDFromHeader extracts a validated trace ID from headers.
// Returns empty string if not present or invalid. (CONV-02)
func TraceIDFromHeader(r *http.Request) string {
	return sanitizeID(r.Header.Get("X-Trace-ID"))
}

// generateID creates a random 16-character hex string.
// Uses crypto/rand for uniqueness without external dependencies.
func generateID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
