// Package runko provides a zero-dependency Go framework for scalable web
// applications and microservice clusters. Every application built with
// RunkoGO is a single binary that handles graceful shutdown, health checks,
// structured logging, and configuration out of the box.
package runko

import (
	"context"
	"crypto/tls"
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

// Default header budget: enough for browser traffic with cookies, JWTs, and
// tracing headers; 16× tighter than Go's 1 MB default to shrink DoS surface.
const defaultMaxHeaderBytes = 64 << 10

// App is the central application container. It manages the HTTP server
// lifecycle, middleware chains, configuration, and graceful shutdown.
type App struct {
	Config *ConfigLoader
	Logger *slog.Logger
	Router *Router

	// Proxy resolves client IPs through trusted proxy chains.
	Proxy *proxyResolver

	server *http.Server

	serviceName     string
	shutdownTimeout time.Duration

	onStartup  []func(ctx context.Context) error
	onShutdown []func(ctx context.Context) error

	health *healthState

	tlsCert       string
	tlsKey        string
	tlsMinVersion uint16

	maxHeaderBytes int

	// stop cancels the Run() context, triggering graceful shutdown.
	// Set by Run; used by tests via triggerShutdown.
	stop context.CancelFunc
}

type healthState struct {
	mu     sync.RWMutex
	ready  bool
	checks []healthCheck
}

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

	// TrustedProxies is a list of IP addresses or CIDR ranges allowed to
	// set forwarding headers (X-Forwarded-For, X-Real-IP). If empty (the
	// default), forwarding headers are ignored and RemoteAddr is always
	// used — secure by default.
	//
	// Examples: "127.0.0.1", "10.0.0.0/8", "172.17.0.0/16".
	TrustedProxies []string

	// TLSCert and TLSKey are paths to a PEM-encoded cert and key. When both
	// are set the server uses HTTPS; when neither is set the server uses
	// HTTP (assumes TLS termination at a reverse proxy). Panics if only
	// one is set.
	TLSCert string
	TLSKey  string

	// TLSMinVersion is the minimum TLS version accepted when serving
	// HTTPS. Defaults to tls.VersionTLS12. Set to tls.VersionTLS13 to
	// require TLS 1.3 (recommended for new deployments).
	TLSMinVersion uint16

	// MaxHeaderBytes caps the size of request line + headers. Defaults to
	// 64 KiB, which fits typical browser traffic (cookies + JWT + tracing)
	// while rejecting abusive payloads. Raise for SSO-heavy deployments
	// with large SAML cookies.
	MaxHeaderBytes int
}

// New creates a new App with the given options. This is the single entry
// point for any RunkoGO application. It sets up config loading, structured
// logging, a router with standard middleware, and health endpoints.
// Panics if TrustedProxies contains invalid entries or TLS is partially
// configured.
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
	if opts.TLSMinVersion == 0 {
		opts.TLSMinVersion = tls.VersionTLS12
	}
	if opts.MaxHeaderBytes == 0 {
		opts.MaxHeaderBytes = defaultMaxHeaderBytes
	}

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
		tlsMinVersion:   opts.TLSMinVersion,
		maxHeaderBytes:  opts.MaxHeaderBytes,
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
// database connections, cache warming, migrations. If any startup hook
// returns an error, the application exits.
func (a *App) OnStartup(fn func(ctx context.Context) error) {
	a.onStartup = append(a.onStartup, fn)
}

// OnShutdown registers a function to run during graceful shutdown,
// after the HTTP server has stopped accepting new requests.
// Use this for closing database pools, flushing buffers.
func (a *App) OnShutdown(fn func(ctx context.Context) error) {
	a.onShutdown = append(a.onShutdown, fn)
}

// AddHealthCheck registers a named readiness check with a timeout.
// The app reports as not ready if any registered check returns an error.
// Each check runs with its own context deadline so a slow database ping
// can't block all other checks.
//
// The check slice is append-only: readinessHandler snapshots the slice
// header under RLock and iterates it lock-free. If a RemoveHealthCheck
// were added, readinessHandler would need to copy the entries under the
// lock or take the lock for the duration of the scan.
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

// Run starts the HTTP server and blocks until SIGINT or SIGTERM, then
// gracefully drains in-flight requests and runs shutdown hooks.
func (a *App) Run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	a.stop = stop

	for _, fn := range a.onStartup {
		if err := fn(ctx); err != nil {
			a.Logger.Error("startup hook failed", "error", err)
			return fmt.Errorf("startup failed: %w", err)
		}
	}

	host := a.Config.GetDefault("HOST", "0.0.0.0")
	port := a.Config.GetDefault("PORT", "19100")

	portNum, err := strconv.Atoi(port)
	if err != nil || portNum < 1 || portNum > 65535 {
		return fmt.Errorf("invalid PORT %q: must be a number between 1 and 65535", port)
	}

	addr := host + ":" + port

	// Register health endpoints now, after all Use() calls have been made,
	// so they inherit global middleware (logging, request ID, client IP)
	// while staying outside any auth-protected groups.
	a.Router.Handle("GET /healthz", a.livenessHandler())
	a.Router.Handle("GET /readyz", a.readinessHandler())

	// Bind before starting the server goroutine so we can return the
	// bind error directly and avoid racing the readiness flag.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}

	useTLS := a.tlsCert != "" && a.tlsKey != ""

	a.server = &http.Server{
		Handler:           a.Router,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    a.maxHeaderBytes,
	}
	if useTLS {
		a.server.TLSConfig = &tls.Config{MinVersion: a.tlsMinVersion}
	}

	serverErr := make(chan error, 1)
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

	// Wait briefly for immediate startup errors (e.g., TLS misconfiguration)
	// before marking as ready.
	select {
	case err := <-serverErr:
		return fmt.Errorf("server error: %w", err)
	case <-time.After(50 * time.Millisecond):
	}

	a.health.mu.Lock()
	a.health.ready = true
	a.health.mu.Unlock()

	a.Logger.Info("service ready", "addr", ln.Addr().String())

	select {
	case err := <-serverErr:
		return fmt.Errorf("server error: %w", err)
	case <-ctx.Done():
		a.Logger.Info("shutdown signal received")
	}

	a.health.mu.Lock()
	a.health.ready = false
	a.health.mu.Unlock()

	drainCtx, drainCancel := context.WithTimeout(context.Background(), a.shutdownTimeout)
	defer drainCancel()

	if err := a.server.Shutdown(drainCtx); err != nil {
		a.Logger.Error("server shutdown error", "error", err)
	}

	// Shutdown hooks get a fresh full timeout so they aren't starved when
	// HTTP drain consumes most of phase 1.
	hookCtx, hookCancel := context.WithTimeout(context.Background(), a.shutdownTimeout)
	defer hookCancel()

	for _, fn := range a.onShutdown {
		if err := fn(hookCtx); err != nil {
			a.Logger.Error("shutdown hook error", "error", err)
		}
	}

	a.Logger.Info("service stopped")
	return nil
}

// runHealthCheck executes a single check with its own deadline and recovers
// from panics so one bad check does not abort the readiness report.
func (a *App) runHealthCheck(parent context.Context, hc healthCheck) (err error) {
	ctx, cancel := context.WithTimeout(parent, hc.timeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return hc.check(ctx)
}

// livenessHandler returns 200 if the process is alive. Kubernetes uses this
// to decide whether to restart the container.
func (a *App) livenessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		JSON(w, http.StatusOK, Map{"status": "alive"})
	})
}

// readinessHandler returns 200 if the service is ready to handle traffic,
// 503 otherwise. Failure responses list only the names of failing checks;
// set HEALTH_DETAIL=true to include error messages for debugging.
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

		failedNames := make([]string, 0)
		failedDetails := make(map[string]string)
		for _, hc := range checks {
			err := a.runHealthCheck(r.Context(), hc)
			if err != nil {
				failedNames = append(failedNames, hc.name)
				failedDetails[hc.name] = err.Error()
				a.Logger.Error("health check failed",
					"check", hc.name,
					"error", err.Error(),
				)
			}
		}

		if len(failedNames) > 0 {
			response := Map{"status": "degraded"}
			if showDetail {
				response["failures"] = failedDetails
			} else {
				response["failures"] = failedNames
			}
			JSON(w, http.StatusServiceUnavailable, response)
			return
		}

		JSON(w, http.StatusOK, Map{"status": "ready"})
	})
}
