# RunkoGO

A zero-dependency Go framework for JSON APIs and microservice clusters. Built by [KallioInnovations Oy](https://kallioinnovations.fi).

**Runko** (Finnish) — *frame, skeleton, chassis*. The structural core that everything else builds on.

**Philosophy**: Every service built with RunkoGO is a single binary that already behaves correctly in a cluster. The developer focuses on business logic; the framework handles operational plumbing.

## Features

All features use only the Go standard library (Go 1.22+). Zero external dependencies.

- **App lifecycle** — Graceful startup, shutdown, and signal handling
- **Health endpoints** — `/healthz` (liveness) and `/readyz` (readiness) with custom checks; each check runs with its own deadline and panic recovery
- **Router** — Built on Go 1.22's enhanced `http.ServeMux` with route groups and middleware
- **Middleware** — Composable chain: recovery, request ID, logging, CORS, CSRF, rate limiting, body limits, allowed-hosts, security headers, client-IP resolution
- **Config** — Typed environment variable loading with validation
- **Structured logging** — JSON via `slog` with automatic request context and sensitive-parameter redaction
- **Context propagation** — Request ID, user ID, trace ID, and resolved client IP flow through every call
- **HTTP client** — Service-to-service calls with retries, circuit breaker, response-size limits, and header forwarding
- **Response helpers** — JSON, errors, pagination, and request body decoding with size limits
- **Security** — Hardened TLS, bounded headers, CSRF, trusted-proxy IP resolution, host validation, security headers, CR/LF injection guards

## Quick Start

```bash
cd example
PORT=19100 LOG_LEVEL=debug go run .
```

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
├── example/        # Complete example API
└── scaffold/       # Self-documenting starter application
```

## Architecture Guide

### App Lifecycle

```go
import runko "github.com/kallioinnovations/runkogo"

app := runko.New(runko.Options{
    ServiceName:     "my-service",
    ShutdownTimeout: 15 * time.Second,
    LogLevel:        "info",
    TrustedProxies:  []string{"10.0.0.0/8"}, // optional
    TLSMinVersion:   tls.VersionTLS13,       // optional; default TLS 1.2
    MaxHeaderBytes:  64 << 10,               // optional; default 64 KiB
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
6. Mark as not ready (load balancer stops routing)
7. Drain in-flight requests
8. Run shutdown hooks
9. Exit

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

Register middleware with `Use` BEFORE calling `Handle` — routes freeze their middleware chain at registration time.

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
- `X-Forwarded-For` is ignored unless `TrustedProxies` is configured.
- Query strings are not logged by default; when enabled, sensitive parameters (tokens, keys, session IDs) are redacted automatically.
- `runko.Error(w, status, code, publicMsg)` never surfaces internal detail. Use `runko.ErrorLog(w, r, logger, status, code, publicMsg, err)` to attach an internal error to the server log with request correlation.

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
// Non-idempotent methods (POST, PATCH) are NOT retried by default.
```

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

// Decode request body (with size limit and unknown field rejection)
var req CreateUserRequest
if err := runko.Decode(w, r, &req); err != nil {
    runko.Error(w, 400, "invalid_body", "Bad JSON")
    return
}
```

## Deployment

### Docker

```dockerfile
FROM golang:1.22-alpine AS build
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
