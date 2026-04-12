package runko

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Recovery middleware catches panics in downstream handlers and returns
// a 500 error instead of crashing the entire server. If the response
// has already been partially written when the panic occurs, Recovery
// logs the error but does not attempt to write — the connection is
// already corrupted and no clean error response is possible.
func Recovery(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rw := &recoveryWriter{ResponseWriter: w}
			defer func() {
				if err := recover(); err != nil {
					logger.Error("panic recovered",
						"error", fmt.Sprint(err),
						"method", r.Method,
						"path", r.URL.Path,
						"request_id", RequestID(r.Context()),
					)
					if !rw.started {
						Error(rw, http.StatusInternalServerError, "internal_error", "Internal server error")
					}
				}
			}()
			next.ServeHTTP(rw, r)
		})
	}
}

// recoveryWriter tracks whether the response has started (headers or
// body sent). Once started, Recovery cannot safely write an error.
type recoveryWriter struct {
	http.ResponseWriter
	started bool
}

func (rw *recoveryWriter) WriteHeader(code int) {
	rw.started = true
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *recoveryWriter) Write(b []byte) (int, error) {
	rw.started = true
	return rw.ResponseWriter.Write(b)
}

// BodyLimit middleware restricts the maximum request body size for all
// routes. This protects handlers that read r.Body directly without
// using Decode(). The limit applies to all HTTP methods. (CONV-06)
//
// Default recommendation: 1MB for API services, higher for file uploads.
// Handlers that need larger bodies can call DecodeWithLimit() which
// overrides the per-request limit.
func BodyLimit(maxBytes int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequestIDMiddleware injects a request ID and trace ID into every
// request's context and response headers. If the incoming request has
// valid X-Request-ID or X-Trace-ID headers (from an upstream service
// or load balancer), they are preserved. Otherwise a request ID is
// generated. This completes the trace propagation loop: incoming
// headers → context → ServiceClient outgoing headers → next service.
func RequestIDMiddleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := RequestIDFromHeader(r)
			ctx := WithRequestID(r.Context(), id)
			ctx = WithRequestStart(ctx, time.Now())

			// Propagate trace ID if present in incoming request.
			if tid := TraceIDFromHeader(r); tid != "" {
				ctx = WithTraceID(ctx, tid)
			}

			w.Header().Set("X-Request-ID", id)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Logger middleware logs every request with method, path, status code,
// and duration. Uses structured JSON logging.
//
// Query strings are NOT logged by default because they frequently
// contain tokens, API keys, and PII. Use LoggerWithConfig to enable
// query logging with automatic redaction of sensitive parameters. (PRIV-02)
func Logger(logger *slog.Logger) Middleware {
	return LoggerWithConfig(logger, LoggerConfig{})
}

// LoggerConfig configures the Logger middleware.
type LoggerConfig struct {
	// IncludeQuery enables logging of query strings. When true, query
	// parameters are logged with sensitive values redacted. (PRIV-02)
	IncludeQuery bool
}

// sensitiveParams are query parameter names whose values are always
// redacted in logs. Case-insensitive matching. (PRIV-02)
var sensitiveParams = map[string]bool{
	"token":         true,
	"key":           true,
	"password":      true,
	"secret":        true,
	"api_key":       true,
	"apikey":        true,
	"authorization": true,
	"access_token":  true,
	"refresh_token": true,
	"session":       true,
	"sessionid":     true,
	"csrf":          true,
	"pwd":           true,
	"pass":          true,
}

// LoggerWithConfig returns a Logger middleware with custom configuration.
func LoggerWithConfig(logger *slog.Logger, cfg LoggerConfig) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Wrap response writer to capture status code.
			sw := &statusWriter{ResponseWriter: w, statusCode: http.StatusOK}

			next.ServeHTTP(sw, r)

			duration := time.Since(start)

			level := slog.LevelInfo
			if sw.statusCode >= 500 {
				level = slog.LevelError
			} else if sw.statusCode >= 400 {
				level = slog.LevelWarn
			}

			attrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.statusCode,
				"duration_ms", duration.Milliseconds(),
				"request_id", RequestID(r.Context()),
				"client_ip", ClientIP(r.Context()),
			}

			if cfg.IncludeQuery && r.URL.RawQuery != "" {
				attrs = append(attrs, "query", redactQuery(r.URL.RawQuery))
			}

			logger.Log(r.Context(), level, "request", attrs...)
		})
	}
}

// redactQuery replaces sensitive query parameter values with "[REDACTED]".
func redactQuery(rawQuery string) string {
	// Parse manually to preserve order and handle malformed queries.
	parts := strings.Split(rawQuery, "&")
	for i, part := range parts {
		eqIdx := strings.Index(part, "=")
		if eqIdx == -1 {
			continue
		}
		key := strings.ToLower(part[:eqIdx])
		if sensitiveParams[key] {
			parts[i] = part[:eqIdx+1] + "[REDACTED]"
		}
	}
	return strings.Join(parts, "&")
}

// statusWriter wraps http.ResponseWriter to capture the status code
// while preserving Flusher and Hijacker interfaces for SSE and
// WebSocket support.
type statusWriter struct {
	http.ResponseWriter
	statusCode int
	written    bool
}

func (sw *statusWriter) WriteHeader(code int) {
	if sw.written {
		return
	}
	sw.statusCode = code
	sw.written = true
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if !sw.written {
		sw.written = true
	}
	return sw.ResponseWriter.Write(b)
}

// Flush passes through to the underlying writer for SSE support.
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack passes through to the underlying writer for WebSocket support.
func (sw *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := sw.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
}

// CORS middleware handles Cross-Origin Resource Sharing headers.
// Configure allowed origins, methods, and headers for browser-based
// API consumption.
type CORSConfig struct {
	// AllowedOrigins is a list of origins that are allowed.
	// Use "*" to allow all origins (not recommended for production).
	AllowedOrigins []string

	// AllowedMethods is a list of HTTP methods allowed.
	// Defaults to GET, POST, PUT, DELETE, PATCH, OPTIONS.
	AllowedMethods []string

	// AllowedHeaders is a list of headers the client may send.
	AllowedHeaders []string

	// AllowCredentials indicates whether cookies/auth headers are allowed.
	AllowCredentials bool

	// MaxAge is how long the browser should cache preflight results (seconds).
	MaxAge int
}

// CORS returns a middleware that handles CORS headers.
//
// Panics at startup if AllowedOrigins contains "*" and AllowCredentials
// is true — the CORS spec forbids this combination. (CONV-05)
func CORS(cfg CORSConfig) Middleware {
	// Validate dangerous configurations at startup. (CONV-05)
	hasWildcard := false
	for _, o := range cfg.AllowedOrigins {
		if o == "*" {
			hasWildcard = true
			break
		}
	}
	if hasWildcard && cfg.AllowCredentials {
		panic("runko: CORS misconfiguration — AllowedOrigins contains \"*\" " +
			"with AllowCredentials enabled. This combination is forbidden by " +
			"the CORS specification and could leak credentials to arbitrary " +
			"origins. Either remove \"*\" and list specific origins, or " +
			"disable AllowCredentials.")
	}

	if len(cfg.AllowedMethods) == 0 {
		cfg.AllowedMethods = []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"}
	}
	if len(cfg.AllowedHeaders) == 0 {
		cfg.AllowedHeaders = []string{"Content-Type", "Authorization", "X-Request-ID"}
	}
	if cfg.MaxAge == 0 {
		cfg.MaxAge = 86400
	}

	methods := strings.Join(cfg.AllowedMethods, ", ")
	headers := strings.Join(cfg.AllowedHeaders, ", ")
	maxAge := fmt.Sprintf("%d", cfg.MaxAge)

	// Build a set for fast preflight method validation.
	allowedMethodSet := make(map[string]bool, len(cfg.AllowedMethods))
	for _, m := range cfg.AllowedMethods {
		allowedMethodSet[m] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			// Always set Vary: Origin to prevent cache poisoning.
			// Without this, a CDN could serve a CORS-allowed response
			// to a disallowed origin.
			w.Header().Add("Vary", "Origin")

			// Check if origin is allowed.
			matched := ""
			for _, o := range cfg.AllowedOrigins {
				if o == "*" {
					// Wildcard: set literal "*", never reflect input.
					matched = "*"
					break
				}
				if o == origin {
					// Specific match: reflect the allowed origin.
					matched = origin
					break
				}
			}

			if matched != "" {
				w.Header().Set("Access-Control-Allow-Origin", matched)
				w.Header().Set("Access-Control-Allow-Methods", methods)
				w.Header().Set("Access-Control-Allow-Headers", headers)
				w.Header().Set("Access-Control-Max-Age", maxAge)
				if cfg.AllowCredentials {
					w.Header().Set("Access-Control-Allow-Credentials", "true")
				}
			}

			// Handle preflight.
			if r.Method == http.MethodOptions {
				// Validate the requested method against allowed methods.
				// If the preflight asks for a method we don't allow,
				// skip CORS headers so the browser blocks the real request.
				if reqMethod := r.Header.Get("Access-Control-Request-Method"); reqMethod != "" {
					if !allowedMethodSet[reqMethod] {
						w.Header().Del("Access-Control-Allow-Origin")
						w.Header().Del("Access-Control-Allow-Methods")
						w.Header().Del("Access-Control-Allow-Headers")
						w.Header().Del("Access-Control-Max-Age")
						w.Header().Del("Access-Control-Allow-Credentials")
					}
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// ClientIPMiddleware resolves the real client IP using the trusted proxy
// chain and stores it in the request context. Downstream middleware and
// handlers access it via runko.ClientIP(r.Context()). (CONV-01)
//
// Security: When no trusted proxies are configured (the default),
// X-Forwarded-For is completely ignored and RemoteAddr is used directly.
// This prevents IP spoofing attacks against rate limiting and audit logs.
func ClientIPMiddleware(proxy *proxyResolver) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := proxy.resolveClientIP(
				r.RemoteAddr,
				r.Header.Get("X-Forwarded-For"),
			)
			ctx := WithClientIP(r.Context(), ip)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RateLimit middleware limits requests per client IP using a simple
// fixed window counter. No external state needed — runs in-process.
// For a multi-instance deployment, use an external rate limiter instead.
//
// Requires ClientIPMiddleware to run first (reads IP from context).
// Falls back to RemoteAddr if ClientIP is not in context.
type RateLimitConfig struct {
	// RequestsPerWindow is the max requests allowed per time window.
	RequestsPerWindow int

	// Window is the time window for rate limiting.
	Window time.Duration

	// MaxClients is the maximum number of distinct IPs tracked
	// simultaneously. When full, new unknown IPs receive 429
	// immediately. Prevents memory exhaustion under DDoS. (CONV-06)
	// Default: 10000.
	MaxClients int
}

// RateLimit returns a middleware that limits requests per client IP.
func RateLimit(cfg RateLimitConfig) Middleware {
	if cfg.MaxClients == 0 {
		cfg.MaxClients = 10000
	}

	type client struct {
		count       int
		windowStart time.Time
	}

	var mu sync.Mutex
	clients := make(map[string]*client)
	lastCleanup := time.Now()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Read resolved client IP from context (set by ClientIPMiddleware).
			ip := ClientIP(r.Context())
			if ip == "" {
				// Fallback if ClientIPMiddleware hasn't run.
				ip = stripPort(r.RemoteAddr)
			}

			mu.Lock()
			now := time.Now()

			// Inline cleanup: purge expired entries periodically.
			// Replaces a background goroutine to avoid goroutine leaks.
			if now.Sub(lastCleanup) > cfg.Window {
				for clientIP, entry := range clients {
					if now.Sub(entry.windowStart) > cfg.Window {
						delete(clients, clientIP)
					}
				}
				lastCleanup = now
			}

			c, exists := clients[ip]
			if !exists || now.Sub(c.windowStart) > cfg.Window {
				// Check map capacity before adding new entry. (CONV-06)
				if !exists && len(clients) >= cfg.MaxClients {
					mu.Unlock()
					w.Header().Set("Retry-After", fmt.Sprintf("%d", int(cfg.Window.Seconds())))
					Error(w, http.StatusTooManyRequests, "rate_limited", "Too many requests")
					return
				}
				clients[ip] = &client{count: 1, windowStart: now}
				mu.Unlock()
				next.ServeHTTP(w, r)
				return
			}

			c.count++
			if c.count > cfg.RequestsPerWindow {
				mu.Unlock()
				w.Header().Set("Retry-After", fmt.Sprintf("%d", int(cfg.Window.Seconds())))
				Error(w, http.StatusTooManyRequests, "rate_limited", "Too many requests")
				return
			}
			mu.Unlock()

			next.ServeHTTP(w, r)
		})
	}
}
