# RunkoGO

A zero-dependency Go framework for scalable web applications and microservice clusters. Built by [KallioInnovations Oy](https://kallioinnovations.fi).

**Runko** (Finnish) — *frame, skeleton, chassis*. The structural core that everything else builds on.

**Philosophy**: Every application built with RunkoGO is a single binary that already behaves correctly in a cluster. The developer focuses on business logic; the framework handles operational plumbing.

## Features

All features use only the Go standard library (Go 1.22+). Zero external dependencies.

- **App lifecycle** — Graceful startup, shutdown, and signal handling
- **Health endpoints** — `/healthz` (liveness) and `/readyz` (readiness) with custom checks
- **Router** — Built on Go 1.22's enhanced `http.ServeMux` with route groups and middleware
- **Middleware** — Composable chain: recovery, request ID, logging, CORS, rate limiting
- **Config** — Typed environment variable loading with validation
- **Structured logging** — JSON via `slog` with automatic request context
- **Context propagation** — Request ID, user ID, and trace ID flow through every call
- **HTTP client** — Service-to-service calls with retries, circuit breaker, and header forwarding
- **Response helpers** — JSON, errors, pagination, and request body decoding

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
├── config.go       # Environment variable loading
├── context.go      # Request-scoped context values
├── logger.go       # Structured logging (slog wrapper)
├── router.go       # Router with groups and middleware support
├── middleware.go   # Standard middleware (recovery, logging, CORS, rate limit)
├── response.go     # JSON response helpers, error formatting, pagination
├── client.go       # HTTP client with retries and circuit breaker
├── go.mod
└── example/
    ├── main.go     # Complete example application
    └── go.mod
```

## Architecture Guide

### App Lifecycle

```go
import runko "github.com/kallioinnovations/runkogo"

app := runko.New(runko.Options{
    ServiceName:     "my-service",
    ShutdownTimeout: 15 * time.Second,
    LogLevel:        "info",
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

### Middleware

Middleware is a function `func(http.Handler) http.Handler`. They compose like nesting dolls:

```go
// Global middleware (runs on every request):
app.Router.Use(
    runko.Recovery(app.Logger),
    runko.RequestIDMiddleware(),
    runko.Logger(app.Logger),
)

// Custom middleware:
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
})

// In a handler:
var orders []Order
err := orderClient.GetJSON(r.Context(), "/api/v1/orders?user_id=42", &orders)

// Request ID and trace ID are automatically forwarded.
// Circuit breaker stops calls if the service is consistently failing.
```

### Health Checks

Built-in endpoints are registered automatically:

- `GET /healthz` — Liveness: "is the process alive?" Always 200 if the server is running.
- `GET /readyz` — Readiness: "can it handle traffic?" Runs registered health checks.

```go
app.AddHealthCheck("database", func() error {
    return db.Ping()
})

app.AddHealthCheck("redis", func() error {
    return redis.Ping()
})
```

A load balancer or orchestrator hits `/readyz` to decide whether to route traffic to this instance. During shutdown, readiness is set to false before draining connections, giving the load balancer time to stop sending new requests.

### Responses

Consistent JSON responses across all services:

```go
// Success
runko.JSON(w, 200, runko.Map{"user": user})

// Created with Location header
runko.Created(w, "/api/v1/users/42", user)

// No content (DELETE)
runko.NoContent(w)

// Error (consistent shape)
runko.Error(w, 404, "not_found", "User not found")
// {"error": {"code": "not_found", "message": "User not found"}}

// Validation error with details
runko.ErrorWithDetails(w, 422, "validation_error", "Invalid input",
    runko.Map{"fields": []string{"email"}},
)

// Paginated list
runko.Paginated(w, users, page, perPage, total)
// {"data": [...], "pagination": {"page": 1, "per_page": 20, ...}}

// Decode request body (with size limit and unknown field rejection)
var req CreateUserRequest
if err := runko.Decode(r, &req); err != nil {
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

| Variable | Description | Default |
|----------|-------------|---------|
| `PORT` | HTTP server port | `19100` |
| `LOG_LEVEL` | Log level: debug, info, warn, error | `info` |

Add your own via `app.Config.Get()` / `app.Config.MustGet()`.

## License

MIT — Built with zero dependencies, just like Go intended.
