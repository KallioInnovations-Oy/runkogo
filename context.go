package runko

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
)

// contextKey is an unexported type to prevent collisions with keys defined
// in other packages.
type contextKey int

const (
	ctxKeyRequestID contextKey = iota
	ctxKeyRequestStart
	ctxKeyUserID
	ctxKeyTraceID
	ctxKeyClientIP
	ctxKeyTraceparent
	ctxKeyTracestate
)

// RequestID extracts the request ID from the context, or "" if unset.
func RequestID(ctx context.Context) string {
	val, _ := ctx.Value(ctxKeyRequestID).(string)
	return val
}

// RequestStart extracts the request start time from the context.
func RequestStart(ctx context.Context) time.Time {
	val, _ := ctx.Value(ctxKeyRequestStart).(time.Time)
	return val
}

// UserID extracts the authenticated user ID from the context, or "" if
// unauthenticated.
func UserID(ctx context.Context) string {
	val, _ := ctx.Value(ctxKeyUserID).(string)
	return val
}

// TraceID extracts the distributed trace ID from the context.
func TraceID(ctx context.Context) string {
	val, _ := ctx.Value(ctxKeyTraceID).(string)
	return val
}

// ClientIP extracts the resolved client IP from the context (the real IP
// after trusted-proxy resolution). Returns "" if ClientIPMiddleware has
// not run.
func ClientIP(ctx context.Context) string {
	val, _ := ctx.Value(ctxKeyClientIP).(string)
	return val
}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

func WithRequestStart(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, ctxKeyRequestStart, t)
}

func WithUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyUserID, id)
}

func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyTraceID, id)
}

func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, ctxKeyClientIP, ip)
}

// Traceparent extracts the W3C Trace Context traceparent header value from
// the context, or "" if the incoming request carried none.
//
// RunkoGO does not create spans — it forwards this value unchanged so a
// service built on it does not sever a trace passing through. See CONV-12.
func Traceparent(ctx context.Context) string {
	val, _ := ctx.Value(ctxKeyTraceparent).(string)
	return val
}

func WithTraceparent(ctx context.Context, tp string) context.Context {
	return context.WithValue(ctx, ctxKeyTraceparent, tp)
}

// Tracestate extracts the W3C tracestate value from the context, or "" if
// the incoming request carried none.
func Tracestate(ctx context.Context) string {
	val, _ := ctx.Value(ctxKeyTracestate).(string)
	return val
}

func WithTracestate(ctx context.Context, ts string) context.Context {
	return context.WithValue(ctx, ctxKeyTracestate, ts)
}

// RequestIDFromHeader extracts a valid X-Request-ID or generates a fresh
// one. The header value is validated (alphanumeric, hyphens, underscores,
// max 64 chars); invalid values are silently replaced to prevent log
// injection.
func RequestIDFromHeader(r *http.Request) string {
	id := sanitizeID(r.Header.Get("X-Request-ID"))
	if id != "" {
		return id
	}
	return generateID()
}

// TraceIDFromHeader extracts a validated trace ID, or "" if absent/invalid.
func TraceIDFromHeader(r *http.Request) string {
	return sanitizeID(r.Header.Get("X-Trace-ID"))
}

// TraceparentFromHeader extracts a valid W3C traceparent, or "" if the
// header is absent, malformed, or ambiguous.
//
// A request carrying more than one traceparent has no single correct
// answer, and taking the first would let a client prepend its own ahead of
// the real one — the same spoofing shape that X-Forwarded-For is hardened
// against. The spec says to discard in that case, so it is discarded.
func TraceparentFromHeader(r *http.Request) string {
	values := r.Header.Values("traceparent")
	if len(values) != 1 {
		return ""
	}
	return sanitizeTraceparent(values[0])
}

// TracestateFromHeader extracts the companion tracestate value, or "" if
// absent or malformed. Multiple header lines are joined, which the spec
// permits for this header.
func TracestateFromHeader(r *http.Request) string {
	values := r.Header.Values("tracestate")
	if len(values) == 0 {
		return ""
	}
	return sanitizeTracestate(strings.Join(values, ","))
}

// generateID returns a random 32-character hex string (128 bits). Request
// IDs are used for tracing/correlation, not security, so math/rand/v2
// (goroutine-safe PCG) is sufficient and avoids panicking on crypto/rand
// exhaustion. 128 bits keeps the birthday-collision probability negligible
// at realistic request volumes.
func generateID() string {
	var b [16]byte
	binary.LittleEndian.PutUint64(b[:8], rand.Uint64())
	binary.LittleEndian.PutUint64(b[8:], rand.Uint64())
	return hex.EncodeToString(b[:])
}
