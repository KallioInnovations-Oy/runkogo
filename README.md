# RunkoGO

A zero-dependency Go framework for JSON APIs and microservice clusters. Built by [KallioInnovations Oy](https://kallioinnovations.fi).

**Runko** (Finnish) — *frame, skeleton, chassis*. The structural core that everything else builds on.

**Philosophy**: Every service built with RunkoGO is a single binary that already behaves correctly in a cluster. The developer focuses on business logic; the framework handles operational plumbing.

RunkoGO is a **starting point**, not a batteries-included framework. It does
not ship everything a production service needs — but every promise it makes
resolves to one of three states: *Enforced* (guaranteed, with a test that
fails if it breaks), *Convention* (you must do something, and it says what),
or *Deferred* (out of scope, with the alternative named).
**[CONVENTIONS.md](CONVENTIONS.md) is the canonical list**, and a test fails
the build if the code or docs drift from it. See
[CHANGELOG.md](CHANGELOG.md) for behavior changes between releases.

## Features

All features use only the Go standard library (Go 1.23+). Zero external dependencies.

- **App lifecycle** — Graceful startup, shutdown, and signal handling
- **Health endpoints** — `/healthz` (liveness) and `/readyz` (readiness) with custom checks; each check runs with its own deadline and panic recovery
- **Router** — Built on Go 1.22's enhanced `http.ServeMux` with route groups and middleware; JSON 404/405 with overridable handlers, and full middleware coverage on every response
- **Middleware** — Composable chain: recovery, request ID, logging, CORS, CSRF, rate limiting, body limits, allowed-hosts, security headers, client-IP resolution
- **Config** — Typed environment variable loading with validation
- **Structured logging** — JSON via `slog` with automatic request context and sensitive-parameter redaction
- **Context propagation** — Request ID, user ID, trace ID, and resolved client IP flow through every call
- **HTTP client** — Service-to-service calls with retries, circuit breaker, response-size limits, and header forwarding
- **Response helpers** — JSON, errors, pagination, and request body decoding with size limits
- **Security** — Hardened TLS, bounded headers, CSRF, trusted-proxy IP resolution, host validation, security headers, CR/LF injection guards

## Quick Start

The scaffold is the starting point — a complete single service with an
interactive page that lets you probe every convention:

```bash
cd scaffold
go run .          # then open http://localhost:19100
```

The example covers the other half of the pitch — calling another service:

```bash
cd example
go run .          # then open http://localhost:19100/demo
```

It starts a RunkoGO service plus a deliberately unreliable peer, so retries,
circuit breaking and trace propagation are observable without any external
dependency.

```bash
# Health checks (no auth required)
curl http://localhost:19100/healthz
curl http://localhost:19100/readyz

# API endpoints (require API key)
curl -H "Authorization: Bearer demo-key" http://localhost:19100/api/v1/users
curl -H "Authorization: Bearer demo-key" http://localhost:19100/api/v1/users/1

# Create a user
curl -X POST http://localhost:19100/api/v1/users \
  -H "Authorization: Bearer demo-key" \
  -H "Content-Type: application/json" \
  -d '{"name":"Ville","email":"ville@example.com"}'

# Graceful shutdown
kill -TERM $(pgrep -f "go run .")
```

## Project Structure

```
runkogo/
├── app.go          # App lifecycle, startup/shutdown, health checks
├── client.go       # HTTP client with retries and circuit breaker
├── config.go       # Environment variable loading
├── context.go      # Request-scoped context values, request ID generation
├── csrf.go         # Double-submit-cookie CSRF middleware
├── hosts.go        # Allowed host validation middleware
├── logger.go       # Structured logging (slog wrapper)
├── middleware.go   # Standard middleware (recovery, logging, CORS, rate limit, body limit)
├── proxy.go        # Trusted proxy IP resolution
├── response.go     # JSON response helpers, error formatting, pagination
├── router.go       # Router with groups and middleware support
├── sanitize.go     # Request ID validation and sanitization
├── security.go     # Security headers middleware
├── go.mod
├── example/        # Service-to-service: retries, circuit breaking, tracing
└── scaffold/       # The starting point: one complete service + demo UI
```

## Architecture Guide

### App Lifecycle

```go
import runko "github.com/kallioinnovations/runkogo"

app := runko.New(runko.Options{
    ServiceName:     "my-service",
    ShutdownTimeout: 15 * time.Second,
    LogLevel:        "info",
    TrustedProxies:  []string{"10.0.0.0/8"}, // optional; X-Forwarded-For only
    TLSMinVersion:   tls.VersionTLS13,       // optional; default TLS 1.2
    MaxHeaderBytes:  64 << 10,               // optional; default 64 KiB
    PreStopDelay:    5 * time.Second,        // optional; -1 disables

    // Server timeouts: 0 selects the default, negative disables.
    ReadTimeout:  30 * time.Second,
    WriteTimeout: -1,                        // required for SSE/streaming
})

app.OnStartup(func(ctx context.Context) error {
    // Open DB connections, warm caches
    return nil
})

app.OnShutdown(func(ctx context.Context) error {
    // Close DB, flush buffers
    return nil
})

app.Run() // Blocks until SIGINT/SIGTERM
```

The lifecycle:
1. Run startup hooks (fail fast if any error)
2. Start HTTP server
3. Mark as ready
4. Serve requests
5. Receive shutdown signal
6. Mark as not ready — `/readyz` starts failing immediately
7. Keep serving for `PreStopDelay` (default 5s) **with the listener still open**, giving the load balancer time to stop routing here
8. Drain in-flight requests (`ShutdownTimeout`)
9. Run shutdown hooks (fresh `ShutdownTimeout`)
10. Exit

Step 7 is not padding. Orchestrators remove a pod from the load balancer
asynchronously, so closing the listener the instant readiness flips leaves
the balancer routing to a socket that now refuses connections — 502s on
every rolling deploy. Set `PreStopDelay: -1` to disable it (correct for
tests and for deployments with no load balancer in front).

**Shutdown hooks also run on early returns** — a failed listen, an
unexpected server error, or a startup hook that fails after an earlier one
succeeded — so resources acquired during startup are always released. They
run exactly once, and are skipped only when the *first* startup hook fails,
since nothing was acquired at that point.

> **Shutdown budget.** Steps 7–9 are sequential, so the worst case is their
> sum: `5s + 15s + 15s = 35s` with the defaults. Kubernetes defaults
> `terminationGracePeriodSeconds` to 30, which would SIGKILL the process
> partway through its shutdown hooks. Raise the grace period above the sum,
> or lower the sum beneath it. See [CONV-13](CONVENTIONS.md#conv-13--lifecycle-is-ordered-and-shutdown-has-a-budget).

### Routing

Go 1.22 added method matching and path parameters to `http.ServeMux`:

```go
app.Router.Handle("GET /users/{id}", getUser)
app.Router.Handle("POST /users", createUser)

// Access path parameters:
id := runko.PathParam(r, "id")
```

Route groups add a prefix and middleware:

```go
api := app.Router.Group("/api/v1", authMiddleware, rateLimitMiddleware)
api.Handle("GET /users", listUsers)       // matches GET /api/v1/users
api.Handle("GET /users/{id}", getUser)    // matches GET /api/v1/users/{id}
```

Root middleware wraps the router itself, so ordering relative to `Handle`
does not matter and **every** request is covered — matched routes,
redirects, 404s and 405s alike. Group middleware is different: it is frozen
onto each route as it is registered, so call `Use` on a group *before*
`Handle`.

Unmatched requests return the same JSON error shape as everything else,
rather than the standard library's `text/plain`:

```jsonc
// 404
{"error": {"code": "not_found", "message": "Resource not found"}}
// 405, with an Allow header listing the methods that are permitted
{"error": {"code": "method_not_allowed", "message": "Method not allowed for this resource"}}
```

Override either one:

```go
app.Router.NotFound(http.HandlerFunc(myNotFound))
app.Router.MethodNotAllowed(http.HandlerFunc(myMethodNotAllowed))
```

Because 404s and 405s run the root chain, they carry security headers, a
request ID, and an access-log line. Note that group middleware does *not*
run for them — a request under `/api/v1` that matches no route is a 404
handled at the root, so group auth middleware cannot conceal which paths
exist.

### Middleware

Middleware is a function `func(http.Handler) http.Handler`. They compose like nesting dolls:

```go
app.Router.Use(
    runko.Recovery(app.Logger),
    runko.BodyLimit(1 << 20),              // 1 MB cap on request bodies
    runko.DefaultSecurityHeaders(),         // CSP-ready headers
    runko.RequestIDMiddleware(),
    runko.ClientIPMiddleware(app.Proxy),
    runko.Logger(app.Logger),
    runko.RateLimit(runko.RateLimitConfig{
        RequestsPerWindow: 100,
        Window:            1 * time.Minute,
    }),
)
```

**Order matters for coverage.** Middleware that answers a request itself —
`CORS` preflight, `AllowedHosts` rejection, `RateLimit` 429, `CSRF`
rejection — returns without calling the rest of the chain. Register those
*after* `Logger` and `DefaultSecurityHeaders`, or the responses they
generate skip both, and a rate-limited flood leaves no log line. The chain
is guaranteed to run for every request; the order within it is yours.

Custom middleware:

```go
func requireAdmin(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if runko.UserID(r.Context()) != "admin" {
            runko.Error(w, 403, "forbidden", "Admin required")
            return
        }
        next.ServeHTTP(w, r)
    })
}
```

### Security

Every response inherits the framework's security conventions. See [SECURITY.md](SECURITY.md) for the full policy.

```go
// Security headers (nosniff, frame-deny, referrer, permissions, etc.)
app.Router.Use(runko.DefaultSecurityHeaders())

// CSRF for cookie-authenticated webapps (double-submit cookie)
app.Router.Use(runko.CSRF(runko.CSRFConfig{
    SameSite: http.SameSiteStrictMode, // default is Lax
}))

// Reject requests whose Host header is not one of ours
app.Router.Use(runko.AllowedHosts(runko.AllowedHostsConfig{
    Hosts: []string{"api.example.com", "api.example.fi"},
}))

// Resolve the real client IP behind a load balancer
app := runko.New(runko.Options{
    TrustedProxies: []string{"10.0.0.0/8"},
})
app.Router.Use(runko.ClientIPMiddleware(app.Proxy))
// ... later:
ip := runko.ClientIP(r.Context())
```

Secure defaults worth knowing:
- TLS 1.2 minimum (set `TLSMinVersion: tls.VersionTLS13` for new deployments).
- `MaxHeaderBytes` defaults to 64 KiB — tight enough to shrink DoS surface, loose enough for typical browser traffic.
- `X-Forwarded-For` is ignored unless `TrustedProxies` is configured. All header lines are joined, and an entry that does not parse as an IP terminates the walk — everything to its left is attacker-reachable. **`X-Real-IP` is not read**; configure your proxy to set `X-Forwarded-For`.
- `RateLimit` buckets IPv6 clients by /64, not by full address, so a client cannot rotate through its delegated range to bypass the limit. A shared /64 is therefore limited collectively. At capacity the limiter evicts the least recently seen client rather than refusing new ones.
- Query strings are not logged by default; when enabled, sensitive parameters (tokens, keys, session IDs) are redacted automatically.
- `runko.Error(w, status, code, publicMsg)` never surfaces internal detail. Use `runko.ErrorLog(w, r, logger, status, code, publicMsg, err)` to attach an internal error to the server log with request correlation.

### Bringing your own router

RunkoGO's lifecycle, health checks, security middleware and service client
do not depend on its router. Pass any `http.Handler` and the framework
serves that instead:

```go
r := chi.NewRouter()
app := runko.New(runko.Options{Handler: r})

// Health endpoints are yours to mount when you supply the handler.
r.Method("GET", "/healthz", app.LivenessHandler())
r.Method("GET", "/readyz", app.ReadinessHandler())

app.Run()
```

Handlers are plain `http.HandlerFunc` throughout, so `runko.Recovery`,
`runko.Logger`, `runko.CORS` and the rest compose onto a chi or stdlib
chain without an adapter. `App.Router` is not served in this mode, and
`Run` logs an error if it finds routes or middleware registered on it —
silent dead configuration is worse than a loud one.

### Observability

RunkoGO ships structured logs, not metrics — see
[CONV-12](CONVENTIONS.md#conv-12--observability-is-structured-logs-not-metrics).
It does provide the two hooks that make bringing your own practical:

```go
// Mount any http.Handler at a prefix, for every method and everything
// beneath it. The path is NOT rewritten, which is what pprof requires.
app.Router.Mount("/metrics", promhttp.Handler())

// Mount on a Group to put it behind auth — worth doing for pprof, which
// exposes heap and goroutine state to anyone who can reach it.
admin := app.Router.Group("/debug", requireAdmin)
admin.Mount("/pprof", http.DefaultServeMux)

// For a sub-app that expects paths relative to its mount point:
app.Router.Mount("/admin", http.StripPrefix("/admin", adminApp))
```

W3C `traceparent` and `tracestate` are validated on the way in and
forwarded byte-for-byte by `ServiceClient`, so a RunkoGO service between two
instrumented services does not sever the trace. It creates no spans of its
own — it is a conduit, not a participant:

```go
tp := runko.Traceparent(r.Context()) // "" if the caller sent none
ts := runko.Tracestate(r.Context())
```

Higher `traceparent` versions are forwarded rather than dropped (the format
is additive per W3C §3.2.4), and a request carrying two `traceparent`
headers is discarded as ambiguous rather than resolved by taking the first.

### Configuration

Environment variables only. No files, no YAML:

```go
// Required (panics at startup if missing):
dbHost := app.Config.MustGet("DB_HOST")

// With defaults:
port := app.Config.GetDefault("PORT", "19100")

// Typed:
workers := app.Config.GetIntDefault("WORKERS", 4)
timeout := app.Config.GetDurationDefault("TIMEOUT", 30*time.Second)
debug := app.Config.GetBool("DEBUG")

// Lists:
origins := app.Config.GetSlice("ALLOWED_ORIGINS")
// ALLOWED_ORIGINS=http://localhost,https://app.example.com
```

### Service-to-Service Communication

```go
orderClient := runko.NewServiceClient(runko.ServiceClientConfig{
    BaseURL:          "http://order-service:8081",
    Timeout:          5 * time.Second,
    MaxRetries:       2,
    CircuitThreshold: 5,
    CircuitCooldown:  30 * time.Second,
    MaxResponseSize:  10 << 20, // 10 MB cap to prevent OOM
})

// In a handler:
var orders []Order
err := orderClient.GetJSON(r.Context(), "/api/v1/orders?user_id=42", &orders)

// Request ID and trace ID are automatically forwarded.
// Circuit breaker stops calls if the service is consistently failing.
// Non-idempotent methods (POST) are NOT retried by default.
```

Retry semantics worth knowing ([CONV-10](CONVENTIONS.md#conv-10--retries-are-safe-by-default)):

- `POST` is not retried on 5xx **or on transport errors**. A connection reset while awaiting a response is not proof the server did not process the request — retrying can double-charge a payment. Set `RetryNonIdempotent: true` only if your endpoints use idempotency keys.
- `MaxRetries` is a ceiling, not a guarantee: the circuit breaker is re-checked before each retry, so a call stops early once the breaker opens.
- The breaker is per-client-instance and per-process. It is not shared across replicas.

### Health Checks

Built-in endpoints are registered automatically:

- `GET /healthz` — Liveness: "is the process alive?" Always 200 if the server is running.
- `GET /readyz` — Readiness: "can it handle traffic?" Runs registered health checks.

```go
app.AddHealthCheck("database", 5*time.Second, func(ctx context.Context) error {
    return db.PingContext(ctx)
})

app.AddHealthCheck("redis", 2*time.Second, func(ctx context.Context) error {
    return redis.Ping(ctx).Err()
})
```

Each check runs with its own deadline and panic recovery so one bad check can't abort the readiness report. Failure responses list only the names of failing checks; set `HEALTH_DETAIL=true` to include error messages for debugging.

A load balancer or orchestrator hits `/readyz` to decide whether to route traffic to this instance. During shutdown, readiness is set to false before draining connections, giving the load balancer time to stop sending new requests.

### Responses

Consistent JSON responses across all services:

```go
// Success
runko.JSON(w, 200, runko.Map{"user": user})

// Created with Location header (panics on CR/LF — use server-minted IDs)
runko.Created(w, "/api/v1/users/42", user)

// No content (DELETE)
runko.NoContent(w)

// Public error (consistent shape, never echoes internal detail)
runko.Error(w, 404, "not_found", "User not found")
// {"error": {"code": "not_found", "message": "User not found"}}

// Public error + internal logging (recommended when you have an error)
runko.ErrorLog(w, r, app.Logger, 500, "store_error", "Failed to create user", err)

// Validation error with details
runko.ErrorWithDetails(w, 422, "validation_error", "Invalid input",
    runko.Map{"fields": []string{"email"}},
)

// Paginated list
runko.Paginated(w, users, page, perPage, total)
// {"data": [...], "pagination": {"page": 1, "per_page": 20, ...}}

// Errors that know how to render themselves (CONV-15)
runko.NotFound("User not found")
runko.Conflict("Email already registered")
runko.Validation("Invalid input", runko.Map{"fields": []string{"email"}})
runko.Internal(err)  // generic 500 to the client, cause to the log

// Decode request body (with size limit and unknown field rejection)
var req CreateUserRequest
if err := runko.Decode(w, r, &req); err != nil {
    runko.Error(w, 400, "invalid_body", "Bad JSON")
    return
}
```

### Error Handling

Deciding what a client may see belongs where the error is created, not in
every handler that passes it along. `AppError` carries the status, the
public code, the public message and the internal cause together:

```go
// In the store, where the failure is understood:
func (s *Store) Get(id string) (*User, error) {
    row, err := s.db.Query(...)
    if errors.Is(err, sql.ErrNoRows) {
        return nil, runko.NotFound("User not found").Wrap(err)
    }
    ...
}

// In every handler, unconditionally:
user, err := store.Get(id)
if err != nil {
    runko.RespondError(w, r, app.Logger, err)
    return
}
```

That replaces the ladder each handler would otherwise repeat:

```go
// Before:
if errors.Is(err, ErrNotFound) { runko.Error(w, 404, "not_found", "..."); return }
if errors.Is(err, ErrConflict) { runko.Error(w, 409, "conflict", "..."); return }
if err != nil                  { runko.Error(w, 500, "store_error", "..."); return }
```

`RespondError` finds the `AppError` anywhere in the wrap chain, so a
sentinel created three layers down still renders correctly at the boundary.

**An error that is not an `AppError` becomes a generic 500.** An error that
never passed through this vocabulary has not been vetted for disclosure, so
a raw driver message naming a host and a username cannot reach a client by
default. The cause is always logged with request correlation — 5xx at error
level, 4xx at warn, because a client mistake should not page anyone.

## Deployment

### Docker

```dockerfile
FROM golang:1.23-alpine AS build
WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 go build -o /service ./example/

FROM scratch
COPY --from=build /service /service
EXPOSE 19100
ENTRYPOINT ["/service"]
```

Binary is ~7.5MB. Container starts in milliseconds.

### Docker Compose (microservice cluster)

```yaml
services:
  user-service:
    build: ./user-service
    environment:
      PORT: "8080"
      DB_HOST: "postgres"
      ORDER_SERVICE_URL: "http://order-service:8081"

  order-service:
    build: ./order-service
    environment:
      PORT: "8081"
      DB_HOST: "postgres"
      USER_SERVICE_URL: "http://user-service:8080"

  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_DB: app
```

Each service discovers others via Docker Compose's built-in DNS. No service registry needed.

### Environment Variables

| Variable        | Description                                      | Default |
|-----------------|--------------------------------------------------|---------|
| `PORT`          | HTTP server port                                 | `19100` |
| `HOST`          | HTTP bind address                                | `0.0.0.0` |
| `LOG_LEVEL`     | Log level: `debug`, `info`, `warn`, `error`      | `info`  |
| `HEALTH_DETAIL` | Include failing-check error messages in `/readyz` | `false` |

Add your own via `app.Config.Get()` / `app.Config.MustGet()`.

## License

MIT — Built with zero dependencies, just like Go intended.
