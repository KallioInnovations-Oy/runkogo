// Package runko provides a zero-dependency Go framework for scalable web
// applications and microservice clusters. Every application built with
// RunkoGO is a single binary that handles graceful shutdown, health checks,
// structured logging, and configuration out of the box.
package runko

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// App is the central application container. It manages the HTTP server
// lifecycle, middleware chains, configuration, and graceful shutdown.
type App struct {
	// Config holds the application configuration loaded from environment.
	Config *ConfigLoader

	// Logger is the structured logger for this application.
	Logger *slog.Logger

	// Router is the HTTP router with middleware support.
	Router *Router

	// Proxy resolves client IPs through trusted proxy chains. (CONV-01)
	Proxy *proxyResolver

	// server is the underlying HTTP server.
	server *http.Server

	// serviceName identifies this service in logs and health checks.
	serviceName string

	// shutdownTimeout is how long to wait for in-flight requests on shutdown.
	shutdownTimeout time.Duration

	// onStartup hooks run after the server is configured but before listening.
	onStartup []func(ctx context.Context) error

	// onShutdown hooks run during graceful shutdown (close DB, flush buffers).
	onShutdown []func(ctx context.Context) error

	// health tracks readiness state.
	health *healthState

	// tlsCert and tlsKey paths for HTTPS. Both empty = HTTP only.
	tlsCert string
	tlsKey  string
}

// healthState tracks liveness and readiness independently.
type healthState struct {
	mu     sync.RWMutex
	ready  bool
	checks []healthCheck
}

// healthCheck is a named readiness check with a timeout.
type healthCheck struct {
	name    string
	timeout time.Duration
	check   func(ctx context.Context) error
}

// Options configures a new App instance.
type Options struct {
	// ServiceName identifies this service (used in logs, health endpoint).
	// Defaults to "runko-app".
	ServiceName string

	// ShutdownTimeout is the maximum time to wait for in-flight requests
	// during graceful shutdown. Defaults to 15 seconds.
	ShutdownTimeout time.Duration

	// LogLevel sets the minimum log level. Defaults to INFO.
	// Accepts: "debug", "info", "warn", "error".
	LogLevel string

	// TrustedProxies is a list of IP addresses or CIDR ranges that are
	// allowed to set forwarding headers (X-Forwarded-For, X-Real-IP).
	// If empty (the default), forwarding headers are IGNORED and
	// RemoteAddr is always used. This is secure by default. (CONV-01)
	//
	// Examples:
	//   "127.0.0.1"       — local reverse proxy
	//   "10.0.0.0/8"      — private network
	//   "172.17.0.0/16"   — Docker default bridge
	TrustedProxies []string

	// TLSCert is the path to a PEM-encoded TLS certificate file.
	// When both TLSCert and TLSKey are set, the server uses HTTPS.
	// When neither is set, the server uses HTTP (assumes TLS
	// termination at a reverse proxy). Panics if only one is set.
	TLSCert string

	// TLSKey is the path to a PEM-encoded TLS private key file.
	TLSKey string
}

// New creates a new App with the given options. This is the single entry
// point for any RunkoGO application. It sets up config loading, structured
// logging, a router with standard middleware, and health endpoints.
// Panics if TrustedProxies contains invalid entries or TLS is
// partially configured (CONV-05).
func New(opts Options) *App {
	if opts.ServiceName == "" {
		opts.ServiceName = "runko-app"
	}
	if opts.ShutdownTimeout == 0 {
		opts.ShutdownTimeout = 15 * time.Second
	}
	if opts.LogLevel == "" {
		opts.LogLevel = "info"
	}

	// Validate TLS config: both or neither must be set. (CONV-05)
	if (opts.TLSCert == "") != (opts.TLSKey == "") {
		certStatus, keyStatus := "<set>", "<set>"
		if opts.TLSCert == "" {
			certStatus = "<empty>"
		}
		if opts.TLSKey == "" {
			keyStatus = "<empty>"
		}
		panic("runko: TLS misconfiguration — both TLSCert and TLSKey must " +
			"be set, or both must be empty. Got TLSCert=" + certStatus +
			" TLSKey=" + keyStatus)
	}

	logger := newLogger(opts.ServiceName, opts.LogLevel)

	app := &App{
		Config:          newConfigLoader(),
		Logger:          logger,
		Proxy:           newProxyResolver(opts.TrustedProxies),
		serviceName:     opts.ServiceName,
		shutdownTimeout: opts.ShutdownTimeout,
		tlsCert:         opts.TLSCert,
		tlsKey:          opts.TLSKey,
		health: &healthState{
			ready:  false,
			checks: make([]healthCheck, 0),
		},
	}

	app.Router = newRouter(logger)

	return app
}

// OnStartup registers a function to run during application startup,
// before the HTTP server begins accepting requests. Use this for
// database connections, cache warming, migrations, etc.
// If any startup hook returns an error, the application exits.
func (a *App) OnStartup(fn func(ctx context.Context) error) {
	a.onStartup = append(a.onStartup, fn)
}

// OnShutdown registers a function to run during graceful shutdown,
// after the HTTP server has stopped accepting new requests.
// Use this for closing database pools, flushing buffers, etc.
func (a *App) OnShutdown(fn func(ctx context.Context) error) {
	a.onShutdown = append(a.onShutdown, fn)
}

// AddHealthCheck registers a named readiness check with a timeout.
// The app reports as not ready if any registered check returns an error.
// Each check runs with its own context deadline so a slow database
// ping can't block all other checks. (CONV-07)
//
// Example:
//
//	app.AddHealthCheck("database", 5*time.Second, func(ctx context.Context) error {
//	    return db.PingContext(ctx)
//	})
func (a *App) AddHealthCheck(name string, timeout time.Duration, check func(ctx context.Context) error) {
	a.health.mu.Lock()
	defer a.health.mu.Unlock()
	a.health.checks = append(a.health.checks, healthCheck{
		name:    name,
		timeout: timeout,
		check:   check,
	})
}

// Run starts the HTTP server and blocks until a shutdown signal is
// received (SIGINT or SIGTERM). It handles the full lifecycle:
//
//  1. Run startup hooks
//  2. Start HTTP server
//  3. Mark as ready
//  4. Block until signal
//  5. Mark as not ready
//  6. Gracefully drain connections
//  7. Run shutdown hooks
//  8. Exit
func (a *App) Run() error {
	// Create root context that cancels on OS signal.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Run startup hooks.
	for _, fn := range a.onStartup {
		if err := fn(ctx); err != nil {
			a.Logger.Error("startup hook failed", "error", err)
			return fmt.Errorf("startup failed: %w", err)
		}
	}

	// Determine bind address.
	host := a.Config.GetDefault("HOST", "0.0.0.0")
	port := a.Config.GetDefault("PORT", "19100")

	// Validate port is a valid number.
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum < 1 || portNum > 65535 {
		return fmt.Errorf("invalid PORT %q: must be a number between 1 and 65535", port)
	}

	addr := host + ":" + port

	// Register health endpoints now, after all Use() calls have been made.
	// This ensures they inherit global middleware (logging, request ID,
	// client IP) while remaining outside any auth-protected groups.
	a.Router.Handle("GET /healthz", a.livenessHandler())
	a.Router.Handle("GET /readyz", a.readinessHandler())

	// Bind the port first, before starting the server goroutine.
	// This ensures the port is actually available before marking ready.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}

	// Configure HTTP server.
	a.server = &http.Server{
		Handler:           a.Router,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Start server in background.
	serverErr := make(chan error, 1)
	useTLS := a.tlsCert != "" && a.tlsKey != ""
	go func() {
		proto := "http"
		if useTLS {
			proto = "https"
		}
		a.Logger.Info("server starting", "addr", ln.Addr().String(), "proto", proto)

		var err error
		if useTLS {
			err = a.server.ServeTLS(ln, a.tlsCert, a.tlsKey)
		} else {
			err = a.server.Serve(ln)
		}
		if err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
		close(serverErr)
	}()

	// Mark as ready — the port is bound, the server is accepting.
	a.health.mu.Lock()
	a.health.ready = true
	a.health.mu.Unlock()

	a.Logger.Info("service ready", "addr", ln.Addr().String())

	// Block until signal or server error.
	select {
	case err := <-serverErr:
		return fmt.Errorf("server error: %w", err)
	case <-ctx.Done():
		a.Logger.Info("shutdown signal received")
	}

	// Mark as not ready (load balancer stops sending traffic).
	a.health.mu.Lock()
	a.health.ready = false
	a.health.mu.Unlock()

	// Gracefully shut down HTTP server.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.shutdownTimeout)
	defer cancel()

	if err := a.server.Shutdown(shutdownCtx); err != nil {
		a.Logger.Error("server shutdown error", "error", err)
	}

	// Run shutdown hooks.
	for _, fn := range a.onShutdown {
		if err := fn(shutdownCtx); err != nil {
			a.Logger.Error("shutdown hook error", "error", err)
		}
	}

	a.Logger.Info("service stopped")
	return nil
}

// livenessHandler returns 200 if the process is alive.
// Kubernetes uses this to know whether to restart the container.
func (a *App) livenessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		JSON(w, http.StatusOK, Map{"status": "alive"})
	})
}

// readinessHandler returns 200 if the service is ready to handle traffic.
// Returns 503 if not ready or if any health check fails.
//
// By default, failure responses list only the NAMES of failing checks,
// never the error details (which may contain hostnames, ports, or
// connection strings). Set HEALTH_DETAIL=true to include error messages
// for debugging. (CONV-03)
func (a *App) readinessHandler() http.Handler {
	showDetail := a.Config.GetBool("HEALTH_DETAIL")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.health.mu.RLock()
		ready := a.health.ready
		checks := a.health.checks
		a.health.mu.RUnlock()

		if !ready {
			JSON(w, http.StatusServiceUnavailable, Map{
				"status": "not_ready",
			})
			return
		}

		// Run all registered health checks with per-check timeouts. (CONV-07)
		failedNames := make([]string, 0)
		failedDetails := make(map[string]string)
		for _, hc := range checks {
			checkCtx, checkCancel := context.WithTimeout(r.Context(), hc.timeout)
			if err := hc.check(checkCtx); err != nil {
				failedNames = append(failedNames, hc.name)
				failedDetails[hc.name] = err.Error()
				// Always log the full error internally.
				a.Logger.Error("health check failed",
					"check", hc.name,
					"error", err.Error(),
				)
			}
			checkCancel()
		}

		if len(failedNames) > 0 {
			response := Map{"status": "degraded"}
			if showDetail {
				// Detailed mode: include error messages (dev/debug only).
				response["failures"] = failedDetails
			} else {
				// Default mode: list only check names, no error details.
				response["failures"] = failedNames
			}
			JSON(w, http.StatusServiceUnavailable, response)
			return
		}

		JSON(w, http.StatusOK, Map{"status": "ready"})
	})
}
