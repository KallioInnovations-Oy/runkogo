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

// Recovery catches panics in downstream handlers and returns a 500.
// If the response has already been partially written, Recovery logs the
// panic but does not attempt to write — the connection is already
// corrupted and no clean error response is possible.
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

func (rw *recoveryWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rw *recoveryWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := rw.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
}

// BodyLimit restricts the maximum request body size. Applies to all methods.
// Handlers that need larger bodies can call DecodeWithLimit to override.
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

// RequestIDMiddleware injects request and trace IDs into every request's
// context and response headers. Preserves valid incoming X-Request-ID and
// X-Trace-ID from upstream services; otherwise generates a request ID.
func RequestIDMiddleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := RequestIDFromHeader(r)
			ctx := WithRequestID(r.Context(), id)
			ctx = WithRequestStart(ctx, time.Now())

			if tid := TraceIDFromHeader(r); tid != "" {
				ctx = WithTraceID(ctx, tid)
			}

			w.Header().Set("X-Request-ID", id)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Logger logs every request with method, path, status, and duration.
//
// Query strings are NOT logged by default because they frequently contain
// tokens, API keys, and PII. Use LoggerWithConfig to enable query logging
// with automatic redaction of sensitive parameters.
func Logger(logger *slog.Logger) Middleware {
	return LoggerWithConfig(logger, LoggerConfig{})
}

// LoggerConfig configures the Logger middleware.
type LoggerConfig struct {
	// IncludeQuery enables logging of query strings with sensitive values
	// redacted.
	IncludeQuery bool
}

// sensitiveParams are query parameter names whose values are redacted in
// logs. Matching is case-insensitive.
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

func LoggerWithConfig(logger *slog.Logger, cfg LoggerConfig) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

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

func redactQuery(rawQuery string) string {
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

// statusWriter wraps http.ResponseWriter to capture the status code while
// preserving Flusher and Hijacker for SSE and WebSocket support.
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
		sw.statusCode = http.StatusOK
		sw.written = true
	}
	return sw.ResponseWriter.Write(b)
}

func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (sw *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := sw.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
}

// CORSConfig configures the CORS middleware.
type CORSConfig struct {
	// AllowedOrigins is a list of origins that are allowed. Use "*" to
	// allow all origins (not recommended for production).
	AllowedOrigins []string

	// AllowedMethods defaults to GET, POST, PUT, DELETE, PATCH, OPTIONS.
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
// Panics at startup if AllowedOrigins contains "*" and AllowCredentials is
// true — the CORS spec forbids this combination.
func CORS(cfg CORSConfig) Middleware {
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
		cfg.AllowedHeaders = []string{"Content-Type", "Authorization", "X-Request-ID", "X-CSRF-Token"}
	}
	if cfg.MaxAge == 0 {
		cfg.MaxAge = 86400
	}

	methods := strings.Join(cfg.AllowedMethods, ", ")
	headers := strings.Join(cfg.AllowedHeaders, ", ")
	maxAge := fmt.Sprintf("%d", cfg.MaxAge)

	allowedMethodSet := make(map[string]bool, len(cfg.AllowedMethods))
	for _, m := range cfg.AllowedMethods {
		allowedMethodSet[m] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			// Vary: Origin on every response prevents CDN cache poisoning.
			w.Header().Add("Vary", "Origin")

			matched := ""
			for _, o := range cfg.AllowedOrigins {
				if o == "*" {
					matched = "*"
					break
				}
				if o == origin {
					matched = origin
					break
				}
			}

			if matched != "" {
				w.Header().Set("Access-Control-Allow-Origin", matched)
				if cfg.AllowCredentials {
					w.Header().Set("Access-Control-Allow-Credentials", "true")
				}
			}

			if r.Method == http.MethodOptions {
				w.Header().Add("Vary", "Access-Control-Request-Method")
				w.Header().Add("Vary", "Access-Control-Request-Headers")

				if matched != "" {
					w.Header().Set("Access-Control-Allow-Methods", methods)
					w.Header().Set("Access-Control-Allow-Headers", headers)
					w.Header().Set("Access-Control-Max-Age", maxAge)
				}

				// If preflight requests a method we don't allow, strip CORS
				// headers so the browser blocks the real request.
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
// chain and stores it in the request context. Handlers access it via
// runko.ClientIP(r.Context()).
//
// When no trusted proxies are configured (the default), X-Forwarded-For is
// ignored and RemoteAddr is used directly. This prevents IP spoofing against
// rate limiting and audit logs.
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

// RateLimitConfig configures the RateLimit middleware.
//
// Requires ClientIPMiddleware to run first; falls back to RemoteAddr when
// ClientIP is not in context. For multi-instance deployments, use an
// external rate limiter instead.
type RateLimitConfig struct {
	// RequestsPerWindow is the max requests allowed per time window.
	RequestsPerWindow int

	// Window is the time window for rate limiting.
	Window time.Duration

	// MaxClients caps the number of distinct IPs tracked simultaneously.
	// When full, new IPs receive 429 immediately. Default: 10000.
	MaxClients int

	// Logger receives capacity warnings. Defaults to slog.Default().
	Logger *slog.Logger

	// Clock returns the current time. Defaults to time.Now. Tests inject
	// a fake clock to exercise window expiry deterministically.
	Clock func() time.Time
}

// RateLimit returns a middleware that limits requests per client IP using
// a fixed window counter. Runs in-process with no external state.
func RateLimit(cfg RateLimitConfig) Middleware {
	if cfg.MaxClients == 0 {
		cfg.MaxClients = 10000
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}

	type client struct {
		count       int
		windowStart time.Time
	}

	var mu sync.Mutex
	clients := make(map[string]*client)
	lastCleanup := cfg.Clock()
	capacityWarned := false

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := ClientIP(r.Context())
			if ip == "" {
				ip = stripPort(r.RemoteAddr)
			}

			mu.Lock()
			now := cfg.Clock()

			// Inline cleanup: sweep the whole map once per Window. The map
			// is bounded by MaxClients (10k default), so a full sweep is
			// cheap — microseconds under the lock. Bounding MaxClients is
			// the operator's knob for lock duration.
			if now.Sub(lastCleanup) > cfg.Window {
				for clientIP, entry := range clients {
					if now.Sub(entry.windowStart) > cfg.Window {
						delete(clients, clientIP)
					}
				}
				lastCleanup = now
				if len(clients) < cfg.MaxClients {
					capacityWarned = false
				}
			}

			c, exists := clients[ip]
			if !exists || now.Sub(c.windowStart) > cfg.Window {
				if !exists && len(clients) >= cfg.MaxClients {
					if !capacityWarned {
						capacityWarned = true
						cfg.Logger.Warn("rate limiter at capacity, rejecting new clients",
							"max_clients", cfg.MaxClients,
							"tracked_clients", len(clients),
						)
					}
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
