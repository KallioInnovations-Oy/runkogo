// This example application demonstrates every feature of the RunkoGO framework.
// It implements a simple user management API that's ready for production
// deployment in a microservice cluster.
//
// Run it:
//
//	PORT=19100 LOG_LEVEL=debug go run .
//
// Test it:
//
//	curl http://localhost:19100/healthz
//	curl http://localhost:19100/readyz
//	curl http://localhost:19100/api/v1/users
//	curl http://localhost:19100/api/v1/users/42
//	curl -X POST http://localhost:19100/api/v1/users \
//	  -H "Content-Type: application/json" \
//	  -d '{"name":"Ville","email":"ville@example.com"}'
//
// Graceful shutdown:
//
//	# In another terminal, send SIGTERM:
//	kill -TERM $(pgrep -f "go run .")
//	# Watch the logs — the server drains connections and shuts down cleanly.
package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	runko "github.com/kallioinnovations/runkogo"
)

// ==========================================================================
// Domain types
// ==========================================================================

// User represents a user in our system.
type User struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateUserRequest is the payload for creating a new user.
type CreateUserRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// ==========================================================================
// In-memory store (replace with a real database in production)
// ==========================================================================

type UserStore struct {
	mu    sync.RWMutex
	users map[string]User
	seq   int
}

func NewUserStore() *UserStore {
	return &UserStore{
		users: map[string]User{
			"1": {ID: "1", Name: "Demo User", Email: "demo@example.com", CreatedAt: time.Now()},
		},
		seq: 1,
	}
}

func (s *UserStore) List() []User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	users := make([]User, 0, len(s.users))
	for _, u := range s.users {
		users = append(users, u)
	}
	return users
}

func (s *UserStore) Get(id string) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[id]
	return u, ok
}

func (s *UserStore) Create(name, email string) User {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	id := fmt.Sprintf("%d", s.seq)
	u := User{
		ID:        id,
		Name:      name,
		Email:     email,
		CreatedAt: time.Now(),
	}
	s.users[id] = u
	return u
}

func (s *UserStore) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[id]; !ok {
		return false
	}
	delete(s.users, id)
	return true
}

// ==========================================================================
// Handlers
// ==========================================================================

// UserHandler groups all user-related HTTP handlers.
// In a real app, this would hold a database connection instead of
// the in-memory store.
type UserHandler struct {
	store  *UserStore
	logger *slog.Logger
}

// List returns all users.
// GET /api/v1/users
func (h *UserHandler) List(w http.ResponseWriter, r *http.Request) {
	users := h.store.List()

	// Demonstrate using the logger with request context.
	// The request ID is automatically included.
	log := runko.LogWithContext(h.logger, r.Context())
	log.Debug("listing users", "count", len(users))

	runko.Paginated(w, users, 1, 20, len(users))
}

// Get returns a single user by ID.
// GET /api/v1/users/{id}
func (h *UserHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := runko.PathParam(r, "id")

	user, ok := h.store.Get(id)
	if !ok {
		runko.Error(w, http.StatusNotFound, "not_found", "User not found")
		return
	}

	runko.JSON(w, http.StatusOK, user)
}

// Create adds a new user.
// POST /api/v1/users
func (h *UserHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req CreateUserRequest
	if err := runko.Decode(r, &req); err != nil {
		runko.Error(w, http.StatusBadRequest, "invalid_body", "Invalid JSON body")
		return
	}

	// Validate.
	if req.Name == "" || req.Email == "" {
		runko.ErrorWithDetails(w, http.StatusUnprocessableEntity,
			"validation_error", "Missing required fields",
			runko.Map{"required": []string{"name", "email"}},
		)
		return
	}

	user := h.store.Create(req.Name, req.Email)

	log := runko.LogWithContext(h.logger, r.Context())
	log.Info("user created", "user_id", user.ID, "email", user.Email)

	runko.Created(w, "/api/v1/users/"+user.ID, user)
}

// Delete removes a user.
// DELETE /api/v1/users/{id}
func (h *UserHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := runko.PathParam(r, "id")

	if !h.store.Delete(id) {
		runko.Error(w, http.StatusNotFound, "not_found", "User not found")
		return
	}

	log := runko.LogWithContext(h.logger, r.Context())
	log.Info("user deleted", "user_id", id)

	runko.NoContent(w)
}

// ==========================================================================
// Custom middleware example
// ==========================================================================

// simpleAuth is a demo middleware that checks for an API key.
// In production, replace this with JWT validation, OAuth2, etc.
func simpleAuth(cfg *runko.ConfigLoader, logger *slog.Logger) runko.Middleware {
	// Read the API key from environment at startup.
	// Falls back to "demo-key" for development.
	apiKey := cfg.GetDefault("API_KEY", "demo-key")

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("Authorization")
			if key == "" {
				key = r.URL.Query().Get("api_key")
			}

			bearerMatch := subtle.ConstantTimeCompare([]byte(key), []byte("Bearer "+apiKey)) == 1
			rawMatch := subtle.ConstantTimeCompare([]byte(key), []byte(apiKey)) == 1
			if !bearerMatch && !rawMatch {
				logger.Warn("unauthorized request",
					"path", r.URL.Path,
					"request_id", runko.RequestID(r.Context()),
				)
				runko.Error(w, http.StatusUnauthorized, "unauthorized", "Invalid or missing API key")
				return
			}

			// Add user identity to context (in a real app, extract from JWT).
			ctx := runko.WithUserID(r.Context(), "api-user")
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ==========================================================================
// Main
// ==========================================================================

func main() {
	// ---------------------------------------------------------------
	// Step 1: Create the app
	// ---------------------------------------------------------------
	// This sets up config loading, structured logging, routing, and
	// health endpoints automatically.
	app := runko.New(runko.Options{
		ServiceName:     "user-service",
		ShutdownTimeout: 10 * time.Second,
		LogLevel:        "debug",
	})

	// ---------------------------------------------------------------
	// Step 2: Initialize dependencies
	// ---------------------------------------------------------------
	store := NewUserStore()
	handler := &UserHandler{
		store:  store,
		logger: app.Logger,
	}

	// ---------------------------------------------------------------
	// Step 3: Register startup hooks
	// ---------------------------------------------------------------
	// These run before the server accepts requests.
	app.OnStartup(func(ctx context.Context) error {
		app.Logger.Info("connecting to database...")
		// In a real app: db, err := sql.Open(...)
		// Simulate slow startup.
		time.Sleep(100 * time.Millisecond)
		app.Logger.Info("database connected")
		return nil
	})

	// ---------------------------------------------------------------
	// Step 4: Register shutdown hooks
	// ---------------------------------------------------------------
	// These run after the server stops accepting requests.
	app.OnShutdown(func(ctx context.Context) error {
		app.Logger.Info("closing database connection...")
		// In a real app: db.Close()
		return nil
	})

	// ---------------------------------------------------------------
	// Step 5: Register health checks
	// ---------------------------------------------------------------
	// The readiness endpoint (/readyz) runs these checks.
	app.AddHealthCheck("database", 5*time.Second, func(ctx context.Context) error {
		// In a real app: return db.PingContext(ctx)
		return nil
	})

	// ---------------------------------------------------------------
	// Step 6: Apply global middleware
	// ---------------------------------------------------------------
	// Order matters: first added runs first (outermost).
	app.Router.Use(
		// Catch panics so one bad request doesn't crash the server.
		runko.Recovery(app.Logger),

		// Limit all request bodies to 1MB. (CONV-06)
		runko.BodyLimit(1<<20),

		// Set security headers on every response. (CONV-04)
		runko.DefaultSecurityHeaders(),

		// Inject request ID (generate or forward from upstream).
		runko.RequestIDMiddleware(),

		// Resolve real client IP through trusted proxy chain. (CONV-01)
		runko.ClientIPMiddleware(app.Proxy),

		// Log every request with method, path, status, duration.
		runko.Logger(app.Logger),

		// Handle CORS for browser-based API consumers.
		runko.CORS(runko.CORSConfig{
			AllowedOrigins: []string{"*"}, // Lock this down in production.
		}),
	)

	// ---------------------------------------------------------------
	// Step 7: Register routes
	// ---------------------------------------------------------------
	// Public routes (no auth required).
	app.Router.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		runko.JSON(w, http.StatusOK, runko.Map{
			"service": "user-service",
			"version": "1.0.0",
		})
	})

	// API routes with auth and rate limiting.
	api := app.Router.Group("/api/v1",
		// Auth middleware — all routes in this group require a valid API key.
		simpleAuth(app.Config, app.Logger),

		// Rate limiting — 100 requests per minute per IP.
		runko.RateLimit(runko.RateLimitConfig{
			RequestsPerWindow: 100,
			Window:            time.Minute,
		}),
	)

	api.HandleFunc("GET /users", handler.List)
	api.HandleFunc("GET /users/{id}", handler.Get)
	api.HandleFunc("POST /users", handler.Create)
	api.HandleFunc("DELETE /users/{id}", handler.Delete)

	// ---------------------------------------------------------------
	// Step 8: Demonstrate service-to-service client (optional)
	// ---------------------------------------------------------------
	// If this service needs to call another service:
	_ = runko.NewServiceClient(runko.ServiceClientConfig{
		BaseURL:          app.Config.GetDefault("ORDER_SERVICE_URL", "http://order-service:8081"),
		Timeout:          5 * time.Second,
		MaxRetries:       2,
		CircuitThreshold: 5,
		CircuitCooldown:  30 * time.Second,
	})
	// Usage in a handler:
	//   var orders []Order
	//   err := orderClient.GetJSON(r.Context(), "/api/v1/orders?user_id="+id, &orders)

	// ---------------------------------------------------------------
	// Step 9: Run
	// ---------------------------------------------------------------
	// This blocks until SIGINT or SIGTERM. The full lifecycle is:
	// 1. Run startup hooks
	// 2. Start HTTP server
	// 3. Mark as ready
	// 4. Serve requests
	// 5. Receive shutdown signal
	// 6. Mark as not ready (load balancer stops sending traffic)
	// 7. Drain in-flight requests (up to ShutdownTimeout)
	// 8. Run shutdown hooks
	// 9. Exit
	if err := app.Run(); err != nil {
		app.Logger.Error("application error", "error", err)
	}
}
