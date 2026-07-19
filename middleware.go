package runko

import (
	"bufio"
	"container/list"
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

			// Carry an incoming W3C traceparent through untouched. Without
			// this a RunkoGO service sitting between two instrumented
			// services breaks the trace at exactly the hop operators most
			// need to see. See CONV-12.
			if tp := TraceparentFromHeader(r); tp != "" {
				ctx = WithTraceparent(ctx, tp)

				// tracestate is only meaningful alongside a traceparent;
				// on its own it is orphaned vendor data, so it is captured
				// only when the pair is intact.
				if ts := TracestateFromHeader(r); ts != "" {
					ctx = WithTracestate(ctx, ts)
				}
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
//
// CONV-09: sensitive data stays out of logs and URLs.
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

			// The matched route template, e.g. "GET /users/{id}". Logging
			// only the path makes /users/1 and /users/2 distinct keys, so
			// latency cannot be aggregated by endpoint without cardinality
			// exploding.
			//
			// The mux stamps this onto the request during routing, which
			// happens beneath this middleware, so it is read after next
			// returns. Two cases leave it empty: an unmatched request, and
			// a middleware registered AFTER Logger that clones the request
			// — RequestIDMiddleware and ClientIPMiddleware both do, via
			// r.WithContext. The mux then stamps the clone, which Logger
			// does not hold. Register Logger after them (as the example
			// and scaffold do) to keep the pattern.
			if r.Pattern != "" {
				attrs = append(attrs, "pattern", r.Pattern)
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
			// Join every X-Forwarded-For line, not just the first. Go
			// stores repeated headers as separate values, and some proxies
			// append a new line rather than extending the existing one.
			// Reading only the first would let a client whose forged line
			// arrives ahead of the proxy's choose its own client IP.
			ip := proxy.resolveClientIP(
				r.RemoteAddr,
				strings.Join(r.Header.Values("X-Forwarded-For"), ", "),
			)
			ctx := WithClientIP(r.Context(), ip)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RateLimitConfig configures the RateLimit middleware.
//
// Requires ClientIPMiddleware to run first; falls back to RemoteAddr when
// ClientIP is not in context.
//
// CONV-11: this limiter is per-instance. Across N replicas the effective
// limit is N times what you configure here, and every counter resets on
// deploy. Treat it as a backstop against one abusive client exhausting one
// instance — cluster-wide limiting belongs at the ingress.
//
// CONV-06: the tracking table is bounded by MaxClients.
type RateLimitConfig struct {
	// RequestsPerWindow is the max requests allowed per time window.
	RequestsPerWindow int

	// Window is the time window for rate limiting.
	Window time.Duration

	// MaxClients caps the number of distinct client buckets tracked
	// simultaneously. When full, the least recently seen bucket is evicted
	// to make room. Default: 10000.
	//
	// Eviction rather than rejection is deliberate: rejecting unseen
	// clients at capacity would let an attacker rotating through cheap
	// addresses fill the table and deny service to every new legitimate
	// client — a more effective attack than the flooding this middleware
	// exists to stop.
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
	return newRateLimiter(cfg).middleware()
}

type rateLimitClient struct {
	count       int
	windowStart time.Time
	elem        *list.Element // position in the LRU list
}

// rateLimiter holds the state behind RateLimit.
//
// It is a named type rather than a closure so that tests can assert the
// size of the tracking table directly. "Bounded resource consumption" is a
// stated convention, and the bound is only meaningfully enforced by the
// eviction and sweep paths — neither of which is observable from response
// codes alone.
type rateLimiter struct {
	cfg RateLimitConfig

	mu             sync.Mutex
	clients        map[string]*rateLimitClient
	lru            *list.List // most recently seen at the front; values are map keys
	lastCleanup    time.Time
	capacityWarned bool
}

func newRateLimiter(cfg RateLimitConfig) *rateLimiter {
	if cfg.MaxClients == 0 {
		cfg.MaxClients = 10000
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	return &rateLimiter{
		cfg:         cfg,
		clients:     make(map[string]*rateLimitClient),
		lru:         list.New(),
		lastCleanup: cfg.Clock(),
	}
}

// tracked reports how many client buckets are currently held.
func (rl *rateLimiter) tracked() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return len(rl.clients)
}

// allow reports whether a request from the given client key may proceed.
func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.cfg.Clock()

	// Inline cleanup: sweep the whole map once per Window. The map is
	// bounded by MaxClients (10k default), so a full sweep is cheap —
	// microseconds under the lock. Bounding MaxClients is the operator's
	// knob for lock duration.
	if now.Sub(rl.lastCleanup) > rl.cfg.Window {
		for clientKey, entry := range rl.clients {
			if now.Sub(entry.windowStart) > rl.cfg.Window {
				rl.lru.Remove(entry.elem)
				delete(rl.clients, clientKey)
			}
		}
		rl.lastCleanup = now
		if len(rl.clients) < rl.cfg.MaxClients {
			rl.capacityWarned = false
		}
	}

	c, exists := rl.clients[key]
	if exists {
		rl.lru.MoveToFront(c.elem)

		if now.Sub(c.windowStart) > rl.cfg.Window {
			c.count = 1
			c.windowStart = now
			return true
		}
		c.count++
		return c.count <= rl.cfg.RequestsPerWindow
	}

	// Make room by evicting the least recently seen buckets.
	for len(rl.clients) >= rl.cfg.MaxClients {
		oldest := rl.lru.Back()
		if oldest == nil {
			break
		}
		if !rl.capacityWarned {
			rl.capacityWarned = true
			rl.cfg.Logger.Warn("rate limiter at capacity, evicting least recently seen clients",
				"max_clients", rl.cfg.MaxClients,
				"tracked_clients", len(rl.clients),
			)
		}
		rl.lru.Remove(oldest)
		delete(rl.clients, oldest.Value.(string))
	}

	rl.clients[key] = &rateLimitClient{
		count:       1,
		windowStart: now,
		elem:        rl.lru.PushFront(key),
	}
	return true
}

func (rl *rateLimiter) middleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := ClientIP(r.Context())
			if ip == "" {
				ip = stripPort(r.RemoteAddr)
			}

			if !rl.allow(rateLimitKey(ip)) {
				w.Header().Set("Retry-After", fmt.Sprintf("%d", int(rl.cfg.Window.Seconds())))
				Error(w, http.StatusTooManyRequests, "rate_limited", "Too many requests")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// rateLimitKey buckets a client IP for rate limiting.
//
// IPv6 clients are routinely delegated a /64, so keying on the full
// address would let one client rotate through 18 quintillion keys and
// bypass the limit entirely — while also filling the tracking table.
// IPv6 is therefore bucketed by /64 and IPv4 by full address.
// Unparseable values are used verbatim so they still bucket consistently.
func rateLimitKey(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ip
	}
	if v4 := parsed.To4(); v4 != nil {
		return v4.String()
	}
	masked := parsed.Mask(net.CIDRMask(64, 128))
	if masked == nil {
		return ip
	}
	return masked.String() + "/64"
}
