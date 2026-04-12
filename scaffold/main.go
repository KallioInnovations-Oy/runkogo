// RunkoGO Scaffold — a self-documenting starter application.
//
// This binary serves an interactive web UI showing framework features,
// security conventions, and a live API tester — all embedded inside
// the binary with zero external files or dependencies.
//
// Run it:
//
//	go run .
//	open http://localhost:19100
//
// The data layer uses the UserStore interface. Swap implementations
// in main() — no handler changes needed:
//
//	store := NewMemoryStore(true)           // dev/test (default)
//	store := NewPostgresStore(ctx, dbURL)   // production (uncomment)
package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"time"

	runko "github.com/kallioinnovations/runkogo"
)

//go:embed static/index.html
var staticFS embed.FS

// ==========================================================================
// Template data
// ==========================================================================

type PageData struct {
	ServiceName string
	Port        string
	GoVersion   string
	BinarySize  string
	FileCount   string
}

// ==========================================================================
// Handlers — depend on UserStore interface, not concrete implementation
// ==========================================================================

type Handlers struct {
	store  UserStore
	logger *slog.Logger
	tmpl   *template.Template
	data   PageData
}

// Index serves the embedded HTML UI.
func (h *Handlers) Index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	h.tmpl.Execute(w, h.data)
}

// ListUsers returns all users with pagination.
// GET /api/v1/users
func (h *Handlers) ListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.store.List(r.Context())
	if err != nil {
		runko.Error(w, http.StatusInternalServerError, "store_error", "Failed to list users")
		return
	}
	runko.Paginated(w, users, 1, 20, len(users))
}

// GetUser returns a single user by ID.
// GET /api/v1/users/{id}
func (h *Handlers) GetUser(w http.ResponseWriter, r *http.Request) {
	id := runko.PathParam(r, "id")

	user, err := h.store.GetByID(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		runko.Error(w, http.StatusNotFound, "not_found", "User not found")
		return
	}
	if err != nil {
		runko.Error(w, http.StatusInternalServerError, "store_error", "Failed to get user")
		return
	}

	runko.JSON(w, http.StatusOK, user)
}

// CreateUser creates a new user.
// POST /api/v1/users
func (h *Handlers) CreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := runko.Decode(r, &req); err != nil {
		runko.Error(w, http.StatusBadRequest, "invalid_body", "Invalid JSON body")
		return
	}
	if req.Name == "" || req.Email == "" {
		runko.ErrorWithDetails(w, http.StatusUnprocessableEntity,
			"validation_error", "Missing required fields",
			runko.Map{"required": []string{"name", "email"}},
		)
		return
	}

	user, err := h.store.Create(r.Context(), req.Name, req.Email)
	if errors.Is(err, ErrConflict) {
		runko.Error(w, http.StatusConflict, "conflict", "A user with this email already exists")
		return
	}
	if err != nil {
		runko.Error(w, http.StatusInternalServerError, "store_error", "Failed to create user")
		return
	}

	log := runko.LogWithContext(h.logger, r.Context())
	log.Info("user created", "user_id", user.ID, "email", user.Email)

	runko.Created(w, "/api/v1/users/"+user.ID, user)
}

// DeleteUser removes a user by ID.
// DELETE /api/v1/users/{id}
func (h *Handlers) DeleteUser(w http.ResponseWriter, r *http.Request) {
	id := runko.PathParam(r, "id")

	err := h.store.Delete(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		runko.Error(w, http.StatusNotFound, "not_found", "User not found")
		return
	}
	if err != nil {
		runko.Error(w, http.StatusInternalServerError, "store_error", "Failed to delete user")
		return
	}

	log := runko.LogWithContext(h.logger, r.Context())
	log.Info("user deleted", "user_id", id)

	runko.NoContent(w)
}

// ==========================================================================
// Auth middleware
// ==========================================================================

func demoAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Authorization")
		if key != "Bearer demo-key" {
			runko.Error(w, http.StatusUnauthorized, "unauthorized", "Send header: Authorization: Bearer demo-key")
			return
		}
		ctx := runko.WithUserID(r.Context(), "demo-user")
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ==========================================================================
// Helpers
// ==========================================================================

func binarySize() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	info, err := os.Stat(exe)
	if err != nil {
		return "unknown"
	}
	mb := float64(info.Size()) / (1024 * 1024)
	return fmt.Sprintf("%.1fMB", mb)
}

// ==========================================================================
// Main
// ==========================================================================

func main() {
	app := runko.New(runko.Options{
		ServiceName: "runko-scaffold",
		LogLevel:    "debug",
	})

	// ---------------------------------------------------------------
	// Data layer — swap here for production database.
	// ---------------------------------------------------------------
	var store UserStore = NewMemoryStore(true)

	// To use PostgreSQL instead, uncomment store_postgres.go and:
	//
	//   dbURL := app.Config.MustGet("DATABASE_URL")
	//   pgStore, err := NewPostgresStore(context.Background(), dbURL)
	//   if err != nil {
	//       app.Logger.Error("database connection failed", "error", err)
	//       os.Exit(1)
	//   }
	//   store = pgStore

	// Parse embedded template.
	tmpl, err := template.ParseFS(staticFS, "static/index.html")
	if err != nil {
		panic("failed to parse embedded template: " + err.Error())
	}

	port := app.Config.GetDefault("PORT", "19100")

	h := &Handlers{
		store:  store,
		logger: app.Logger,
		tmpl:   tmpl,
		data: PageData{
			ServiceName: "runko-scaffold",
			Port:        port,
			GoVersion:   runtime.Version(),
			BinarySize:  binarySize(),
			FileCount:   "11",
		},
	}

	// ---------------------------------------------------------------
	// Lifecycle hooks
	// ---------------------------------------------------------------
	app.OnStartup(func(ctx context.Context) error {
		app.Logger.Info("scaffold ready — open your browser",
			"url", fmt.Sprintf("http://localhost:%s", port),
		)
		return nil
	})

	app.OnShutdown(func(ctx context.Context) error {
		app.Logger.Info("closing data store")
		return store.Close()
	})

	// ---------------------------------------------------------------
	// Health checks — uses the store's Ping method
	// ---------------------------------------------------------------
	app.AddHealthCheck("store", 5*time.Second, func(ctx context.Context) error {
		return store.Ping(ctx)
	})

	// ---------------------------------------------------------------
	// Middleware
	// ---------------------------------------------------------------
	app.Router.Use(
		runko.Recovery(app.Logger),
		runko.BodyLimit(1<<20),
		runko.DefaultSecurityHeaders(),
		runko.RequestIDMiddleware(),
		runko.ClientIPMiddleware(app.Proxy),
		runko.Logger(app.Logger),
		runko.CORS(runko.CORSConfig{
			AllowedOrigins: []string{"*"},
		}),
	)

	// ---------------------------------------------------------------
	// Routes
	// ---------------------------------------------------------------
	app.Router.HandleFunc("GET /", h.Index)

	api := app.Router.Group("/api/v1", demoAuth)
	api.HandleFunc("GET /users", h.ListUsers)
	api.HandleFunc("GET /users/{id}", h.GetUser)
	api.HandleFunc("POST /users", h.CreateUser)
	api.HandleFunc("DELETE /users/{id}", h.DeleteUser)

	if err := app.Run(); err != nil {
		app.Logger.Error("application error", "error", err)
	}
}
