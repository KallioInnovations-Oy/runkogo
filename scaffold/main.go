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
//	store := NewMemoryStore(true)                              // dev/test (default)
//	store, err := NewPostgresStore(ctx, "pgx", databaseURL)    // production
//
// See store_postgres.go: it compiles as-is against database/sql and needs
// only a driver import to run.
package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"expvar"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strconv"
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

	// APIKey is injected into the page's live API tester. Hardcoding it in
	// the HTML would both publish the key in the shipped binary and break
	// every authenticated probe the moment an operator sets API_KEY.
	APIKey string
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
	if err := h.tmpl.Execute(w, h.data); err != nil {
		h.logger.Error("template execution failed", "error", err)
	}
}

// storeError translates domain sentinels into their HTTP representation.
//
// CONV-15: this mapping happens once, here, instead of being repeated as an
// errors.Is ladder in every handler. The store stays HTTP-agnostic — it
// returns domain errors and knows nothing about status codes — while each
// handler ends with a single unconditional RespondError call.
//
// Anything not matched falls through unchanged, and RespondError renders it
// as a generic 500 rather than leaking a driver message to the client.
func storeError(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return runko.NotFound("User not found").Wrap(err)
	case errors.Is(err, ErrConflict):
		return runko.Conflict("A user with this email already exists").Wrap(err)
	default:
		return err
	}
}

// maxPage bounds the page number so that (page-1)*perPage cannot overflow
// int. Without it, page=9223372036854775807 wraps to a negative offset —
// which the store would then either reject or, if it sliced naively, panic
// on. Unparseable or out-of-range values fall back to the default rather
// than erroring: pagination is a display concern, not a contract.
const (
	defaultPerPage = 20
	maxPerPage     = 100
	maxPage        = 1_000_000
)

func pageParams(r *http.Request) (page, perPage int) {
	page, perPage = 1, defaultPerPage

	if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && v > 0 {
		page = min(v, maxPage)
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("per_page")); err == nil && v > 0 {
		// Capped so a client cannot request the entire table in one call.
		perPage = min(v, maxPerPage)
	}
	return page, perPage
}

// ListUsers returns a page of users.
// GET /api/v1/users?page=1&per_page=20
func (h *Handlers) ListUsers(w http.ResponseWriter, r *http.Request) {
	page, perPage := pageParams(r)

	// The window is pushed down to the store rather than fetching
	// everything and slicing here, so swapping in a SQL-backed store turns
	// this into a LIMIT/OFFSET query instead of an unbounded table scan.
	users, total, err := h.store.ListPage(r.Context(), perPage, (page-1)*perPage)
	if err != nil {
		runko.RespondError(w, r, h.logger, storeError(err))
		return
	}

	runko.Paginated(w, users, page, perPage, total)
}

// GetUser returns a single user by ID.
// GET /api/v1/users/{id}
func (h *Handlers) GetUser(w http.ResponseWriter, r *http.Request) {
	user, err := h.store.GetByID(r.Context(), runko.PathParam(r, "id"))
	if err != nil {
		runko.RespondError(w, r, h.logger, storeError(err))
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
	if err := runko.Decode(w, r, &req); err != nil {
		runko.RespondError(w, r, h.logger,
			runko.BadRequest("Invalid JSON body").Wrap(err))
		return
	}

	var missing []string
	if req.Name == "" {
		missing = append(missing, "name")
	}
	if req.Email == "" {
		missing = append(missing, "email")
	}
	if len(missing) > 0 {
		runko.RespondError(w, r, h.logger,
			runko.Validation("Missing required fields", runko.Map{"required": missing}))
		return
	}

	user, err := h.store.Create(r.Context(), req.Name, req.Email)
	if err != nil {
		runko.RespondError(w, r, h.logger, storeError(err))
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

	if err := h.store.Delete(r.Context(), id); err != nil {
		runko.RespondError(w, r, h.logger, storeError(err))
		return
	}

	log := runko.LogWithContext(h.logger, r.Context())
	log.Info("user deleted", "user_id", id)

	runko.NoContent(w)
}

// Profile is the safe-method half of the CSRF demonstration. Visiting it
// issues the CSRF token cookie that the unsafe half requires.
// GET /app/profile
func (h *Handlers) Profile(w http.ResponseWriter, r *http.Request) {
	runko.JSON(w, http.StatusOK, runko.Map{
		"user": "demo-user",
		"hint": "POST here with the X-CSRF-Token header set to the csrf_token cookie value",
	})
}

// UpdateProfile is the unsafe half. The CSRF middleware rejects it unless
// the header echoes the cookie, so it never runs without a valid token.
// POST /app/profile
func (h *Handlers) UpdateProfile(w http.ResponseWriter, r *http.Request) {
	runko.JSON(w, http.StatusOK, runko.Map{
		"updated": true,
		"proof":   "the CSRF token matched, so this handler ran",
	})
}

// ==========================================================================
// Metrics — explicit, not everything expvar knows
// ==========================================================================

// Counters this application chose to publish. expvar.NewInt registers them
// globally, but only the ones named in metricsHandler are ever served.
var (
	requestsTotal  = expvar.NewInt("http_requests_total")
	responses2xx   = expvar.NewInt("http_responses_2xx")
	responses4xx   = expvar.NewInt("http_responses_4xx")
	responses5xx   = expvar.NewInt("http_responses_5xx")
	requestsInWork = expvar.NewInt("http_requests_in_flight")
)

// countMetrics records the RED-style counters the framework deliberately
// does not ship (CONV-12). Roughly twenty lines is what "bring your own
// metrics" actually costs.
func countMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestsTotal.Add(1)
		requestsInWork.Add(1)
		defer requestsInWork.Add(-1)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		switch {
		case rec.status >= 500:
			responses5xx.Add(1)
		case rec.status >= 400:
			responses4xx.Add(1)
		case rec.status >= 200 && rec.status < 300:
			responses2xx.Add(1)
		}
	})
}

// statusRecorder captures the status code. Flush is forwarded so this
// middleware does not silently break streaming responses.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.written {
		s.status, s.written = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// metricsHandler serves only the counters above — no cmdline, no memstats.
func metricsHandler() http.Handler {
	published := []struct {
		name string
		v    expvar.Var
	}{
		{"http_requests_total", requestsTotal},
		{"http_requests_in_flight", requestsInWork},
		{"http_responses_2xx", responses2xx},
		{"http_responses_4xx", responses4xx},
		{"http_responses_5xx", responses5xx},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprintln(w, "{")
		for i, p := range published {
			if i > 0 {
				fmt.Fprintln(w, ",")
			}
			fmt.Fprintf(w, "  %q: %s", p.name, p.v.String())
		}
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "}")
	})
}

// ==========================================================================
// Auth middleware
// ==========================================================================

// apiKeyAuth compares the Authorization header against the configured key
// in constant time, so a wrong key cannot be discovered by timing.
//
// The key comes from the environment. Hardcoding it would be copied
// verbatim into real deployments — the scaffold is meant to be copied, so
// the pattern it shows has to be the safe one.
func apiKeyAuth(apiKey string, logger *slog.Logger) runko.Middleware {
	expected := []byte("Bearer " + apiKey)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("Authorization")
			if subtle.ConstantTimeCompare([]byte(key), expected) != 1 {
				runko.RespondError(w, r, logger,
					runko.Unauthorized("Send header: Authorization: Bearer <API_KEY>"))
				return
			}
			ctx := runko.WithUserID(r.Context(), "demo-user")
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
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
	// LogLevel is read from the environment rather than hardcoded — the
	// demo page documents LOG_LEVEL, and a documented knob that does
	// nothing is worse than an undocumented one.
	app := runko.New(runko.Options{
		ServiceName: "runko-scaffold",
		LogLevel:    os.Getenv("LOG_LEVEL"), // "" falls back to info
	})

	// ---------------------------------------------------------------
	// Data layer — swap here for production database.
	// ---------------------------------------------------------------
	var store UserStore = NewMemoryStore(true)

	// To use PostgreSQL instead: add a driver (go get github.com/jackc/pgx/v5),
	// import it for its side effects, run the migration in store_postgres.go,
	// then:
	//
	//   import _ "github.com/jackc/pgx/v5/stdlib"
	//
	//   pgStore, err := NewPostgresStore(context.Background(), "pgx",
	//       app.Config.MustGet("DATABASE_URL"))
	//   if err != nil {
	//       app.Logger.Error("database connection failed", "error", err)
	//       os.Exit(1)
	//   }
	//   store = pgStore
	//
	// store_postgres.go already compiles and is type-checked in CI, so the
	// only thing missing is the driver.

	// Parse embedded template.
	tmpl, err := template.ParseFS(staticFS, "static/index.html")
	if err != nil {
		panic("failed to parse embedded template: " + err.Error())
	}

	port := app.Config.GetDefault("PORT", "19100")

	// ---------------------------------------------------------------
	// Configuration — environment only (12-factor).
	// ---------------------------------------------------------------
	// The demo key exists so `go run .` works with no setup. It is logged
	// loudly as a warning precisely because a scaffold gets copied: a
	// silent default is one that reaches production unnoticed.
	apiKey := app.Config.GetDefault("API_KEY", "demo-key")
	if apiKey == "demo-key" {
		app.Logger.Warn("using the built-in demo API key — set API_KEY before deploying anywhere real")
	}

	// Explicit origins, never "*". Defaults to the local dev server so the
	// embedded UI works out of the box.
	allowedOrigins := app.Config.GetSlice("ALLOWED_ORIGINS")
	if len(allowedOrigins) == 0 {
		allowedOrigins = []string{"http://localhost:" + port}
	}

	h := &Handlers{
		store:  store,
		logger: app.Logger,
		tmpl:   tmpl,
		data: PageData{
			ServiceName: "runko-scaffold",
			Port:        port,
			GoVersion:   runtime.Version(),
			BinarySize:  binarySize(),
			APIKey:      apiKey,
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
	//
	// Order is load-bearing (CONV-04, CONV-08):
	//   - Logger sits AFTER RequestID and ClientIP so their context reaches
	//     the log line, and so the matched route pattern survives — those
	//     two clone the request, and the mux stamps the clone.
	//   - CORS sits AFTER Logger because it answers preflights itself. Put
	//     it first and every preflight goes unlogged.
	// ---------------------------------------------------------------
	app.Router.Use(
		runko.Recovery(app.Logger),
		runko.BodyLimit(1<<20),
		runko.DefaultSecurityHeaders(),
		runko.RequestIDMiddleware(),
		runko.ClientIPMiddleware(app.Proxy),
		runko.Logger(app.Logger),
		countMetrics, // CONV-12: metrics are the application's job
		// CONV-06 / CONV-11: an in-process backstop against one abusive
		// client, not a cluster-wide limit. Generous here so the demo
		// page's own probes do not trip it.
		runko.RateLimit(runko.RateLimitConfig{
			RequestsPerWindow: 300,
			Window:            time.Minute,
			Logger:            app.Logger,
		}),
		runko.CORS(runko.CORSConfig{
			// Listed explicitly, never "*". A wildcard here is copied into
			// production more often than it is reconsidered.
			AllowedOrigins: allowedOrigins,
		}),
	)

	// ---------------------------------------------------------------
	// Routes
	// ---------------------------------------------------------------
	// "GET /{$}" matches ONLY the root path. Plain "GET /" is a catch-all
	// that swallows every unmatched GET, so a typo returns this page with
	// a 200 instead of the framework's JSON 404 — and it collides with any
	// Mount, because a method-less subtree pattern and a catch-all are
	// mutually ambiguous to ServeMux.
	app.Router.HandleFunc("GET /{$}", h.Index)

	// Bearer-token API. Browsers do not attach Authorization headers
	// automatically, so these endpoints are not CSRF-reachable.
	api := app.Router.Group("/api/v1", apiKeyAuth(apiKey, app.Logger))
	api.HandleFunc("GET /users", h.ListUsers)
	api.HandleFunc("GET /users/{id}", h.GetUser)
	api.HandleFunc("POST /users", h.CreateUser)
	api.HandleFunc("DELETE /users/{id}", h.DeleteUser)

	// CONV-14: cookie-authenticated routes DO need CSRF, because a browser
	// sends cookies on cross-origin requests automatically. A GET here
	// issues the token cookie; the POST requires it echoed in the
	// X-CSRF-Token header.
	//
	// The token cookie is Secure by default, which is correct — but it means
	// a browser will discard it if you reach this demo over plain HTTP on a
	// LAN address rather than localhost, and every POST will then fail with
	// csrf_missing. Browsers treat localhost as a trustworthy origin, so the
	// default works for `go run .`; set Secure: false only if you genuinely
	// need plain HTTP over a network.
	webapp := app.Router.Group("/app", runko.CSRF(runko.CSRFConfig{}))
	webapp.HandleFunc("GET /profile", h.Profile)
	webapp.HandleFunc("POST /profile", h.UpdateProfile)

	// CONV-12: the framework ships no metrics. Mount your own collector —
	// expvar is stdlib, so the scaffold stays dependency-free.
	//
	// Note what is NOT used here: expvar.Handler(). That handler serves
	// every published var, which includes os.Args ("cmdline") and full
	// memstats, neither of which was chosen by this application. Command
	// lines routinely carry DSNs and credentials. Publishing what you
	// picked, rather than everything the runtime happens to expose, is the
	// pattern worth copying.
	debug := app.Router.Group("/debug", apiKeyAuth(apiKey, app.Logger))
	debug.Mount("/vars", metricsHandler())

	if err := app.Run(); err != nil {
		app.Logger.Error("application error", "error", err)
	}
}
