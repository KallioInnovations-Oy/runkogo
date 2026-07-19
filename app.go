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

// Default grace period between readiness flipping to false and the
// listener closing. Sized for Kubernetes endpoint propagation.
const defaultPreStopDelay = 5 * time.Second

// Default server timeouts, sized for JSON APIs.
const (
	defaultReadTimeout       = 30 * time.Second
	defaultReadHeaderTimeout = 10 * time.Second
	defaultWriteTimeout      = 30 * time.Second
	defaultIdleTimeout       = 120 * time.Second
)

// resolveTimeout maps the Options convention (0 = default, negative =
// disabled) onto http.Server's convention (0 = no timeout).
func resolveTimeout(configured, fallback time.Duration) time.Duration {
	switch {
	case configured == 0:
		return fallback
	case configured < 0:
		return 0
	default:
		return configured
	}
}

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
	preStopDelay    time.Duration

	readTimeout       time.Duration
	readHeaderTimeout time.Duration
	writeTimeout      time.Duration
	idleTimeout       time.Duration

	// handler, when set, replaces Router as the served handler.
	handler http.Handler

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
	// set X-Forwarded-For. If empty (the default), the header is ignored
	// and RemoteAddr is always used — secure by default.
	//
	// Examples: "127.0.0.1", "10.0.0.0/8", "172.17.0.0/16".
	//
	// Only X-Forwarded-For is read. X-Real-IP is NOT consulted: configure
	// your proxy to set X-Forwarded-For, or every request will resolve to
	// the proxy's own address — collapsing all traffic into one rate-limit
	// bucket and recording the balancer's IP in every audit log line.
	//
	// CONV-01: secure by default, explicit opt-in for trust.
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

	// PreStopDelay is how long the server keeps accepting requests after
	// readiness flips to false, before the listener closes.
	//
	// Orchestrators remove a pod from the load balancer asynchronously:
	// /readyz starts failing immediately, but endpoint propagation takes
	// seconds. Closing the listener the moment readiness flips means the
	// balancer is still routing to a socket that now refuses connections,
	// which surfaces as 502s on every rolling deploy. This delay covers
	// that window.
	//
	// Defaults to 5 seconds. Set to a negative value to disable — correct
	// for tests and for single-instance deployments with no load balancer
	// in front.
	//
	// SHUTDOWN BUDGET — read this before tuning either timeout.
	//
	// Shutdown runs three phases in sequence, and the worst case is their
	// sum, not the larger of them:
	//
	//	PreStopDelay + ShutdownTimeout (drain) + ShutdownTimeout (hooks)
	//
	// With the defaults that is 5s + 15s + 15s = 35s. Kubernetes defaults
	// terminationGracePeriodSeconds to 30, so a worst-case shutdown is
	// SIGKILLed partway through the shutdown hooks — losing exactly the
	// cleanup those hooks exist to perform.
	//
	// The budget is the operator's to reconcile. Either raise the grace
	// period above the sum, or lower the sum beneath it. For the defaults
	// on Kubernetes, one of:
	//
	//	terminationGracePeriodSeconds: 45   // in the pod spec, or
	//	ShutdownTimeout: 10 * time.Second   // 5 + 10 + 10 = 25s
	//
	// The framework does not clamp these values: it cannot see the grace
	// period, and silently shortening a drain the operator asked for would
	// cut off in-flight requests.
	PreStopDelay time.Duration

	// Server timeouts. Each defaults to a value sized for JSON APIs; set a
	// negative value to disable one entirely (http.Server treats zero as
	// "no timeout", which is why zero here means "use the default" rather
	// than "off").
	//
	// Defaults: ReadTimeout 30s, ReadHeaderTimeout 10s, WriteTimeout 30s,
	// IdleTimeout 120s.
	//
	// WriteTimeout is the one to reach for. It bounds the whole response,
	// so the 30s default rules out server-sent events, long-polling,
	// streaming responses and slow downloads — the middleware already
	// preserves http.Flusher and http.Hijacker for exactly those, so this
	// timeout is the only thing in the way:
	//
	//	runko.New(runko.Options{WriteTimeout: -1}) // SSE, streaming
	//
	// Disabling it means a stalled client can hold a connection until
	// IdleTimeout or the OS gives up, so prefer a generous value over -1
	// unless the response really is unbounded.
	//
	// ReadTimeout bounds the whole request including the body, so raise it
	// for large uploads rather than relying on the body limit alone.
	ReadTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration

	// Handler replaces the built-in Router as the server's handler.
	//
	// Set this to use RunkoGO's lifecycle, health checks, security
	// middleware and service client with a different router — chi, or a
	// plain http.ServeMux — instead of adopting the whole framework:
	//
	//	r := chi.NewRouter()
	//	app := runko.New(runko.Options{Handler: r})
	//	r.Method("GET", "/healthz", app.LivenessHandler())
	//	r.Method("GET", "/readyz", app.ReadinessHandler())
	//
	// When set, App.Router is not served and the health endpoints are NOT
	// registered automatically — mount them yourself via LivenessHandler
	// and ReadinessHandler, wherever your router wants them. Run logs an
	// error if it detects routes or middleware registered on the unused
	// built-in Router, since that is silent dead configuration.
	Handler http.Handler
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
	if opts.PreStopDelay == 0 {
		opts.PreStopDelay = defaultPreStopDelay
	}
	if opts.PreStopDelay < 0 {
		opts.PreStopDelay = 0
	}

	// Zero means "use the default"; negative means "no timeout", which is
	// what http.Server spells as zero.
	opts.ReadTimeout = resolveTimeout(opts.ReadTimeout, defaultReadTimeout)
	opts.ReadHeaderTimeout = resolveTimeout(opts.ReadHeaderTimeout, defaultReadHeaderTimeout)
	opts.WriteTimeout = resolveTimeout(opts.WriteTimeout, defaultWriteTimeout)
	opts.IdleTimeout = resolveTimeout(opts.IdleTimeout, defaultIdleTimeout)

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
		Config:            newConfigLoader(),
		Logger:            logger,
		Proxy:             newProxyResolver(opts.TrustedProxies),
		serviceName:       opts.ServiceName,
		shutdownTimeout:   opts.ShutdownTimeout,
		preStopDelay:      opts.PreStopDelay,
		readTimeout:       opts.ReadTimeout,
		readHeaderTimeout: opts.ReadHeaderTimeout,
		writeTimeout:      opts.WriteTimeout,
		idleTimeout:       opts.IdleTimeout,
		handler:           opts.Handler,
		tlsCert:           opts.TLSCert,
		tlsKey:            opts.TLSKey,
		tlsMinVersion:     opts.TLSMinVersion,
		maxHeaderBytes:    opts.MaxHeaderBytes,
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

// Run starts the HTTP server and blocks until SIGINT or SIGTERM.
//
// Shutdown proceeds in a fixed order: readiness flips to false, the server
// keeps serving for PreStopDelay with the listener still open, in-flight
// requests drain, then shutdown hooks run with a fresh timeout.
//
// Shutdown hooks also run when Run returns early — a failed listen, an
// unexpected server error, or a startup hook that fails after an earlier
// one succeeded. They run exactly once. They are skipped only when the
// first startup hook fails, since nothing was acquired at that point.
//
// CONV-13: the three shutdown phases are sequential, so the worst-case
// duration is their sum. See Options.PreStopDelay for the budget and how
// it interacts with terminationGracePeriodSeconds.
func (a *App) Run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	a.stop = stop

	// Shutdown hooks release what startup hooks acquired, so they must run
	// on every exit past that point — including a failed listen or an
	// unexpected server error — or those resources leak. Guarded by Once
	// because the normal shutdown path and the defer both reach it.
	var shutdownOnce sync.Once
	runShutdownHooks := func() {
		shutdownOnce.Do(func() {
			// Hooks get a fresh full timeout so they aren't starved when
			// HTTP drain consumes most of the shutdown budget.
			hookCtx, cancel := context.WithTimeout(context.Background(), a.shutdownTimeout)
			defer cancel()
			for _, fn := range a.onShutdown {
				if err := fn(hookCtx); err != nil {
					a.Logger.Error("shutdown hook error", "error", err)
				}
			}
		})
	}

	completedStartupHooks := 0
	for _, fn := range a.onStartup {
		if err := fn(ctx); err != nil {
			a.Logger.Error("startup hook failed", "error", err)
			// Roll back the hooks that already succeeded. When none did,
			// there is nothing to release and shutdown hooks may legitimately
			// assume startup ran, so they are skipped.
			if completedStartupHooks > 0 {
				runShutdownHooks()
			}
			return fmt.Errorf("startup failed: %w", err)
		}
		completedStartupHooks++
	}

	// All startup hooks succeeded; every exit from here runs shutdown hooks.
	defer runShutdownHooks()

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
	// With a custom Handler the framework does not know where the health
	// endpoints belong, so the caller mounts them via LivenessHandler and
	// ReadinessHandler.
	handler := a.handler
	if handler == nil {
		a.Router.Handle("GET /healthz", a.LivenessHandler())
		a.Router.Handle("GET /readyz", a.ReadinessHandler())
		handler = a.Router
	} else if a.Router.registrations() > 0 {
		// Configuring the built-in Router while serving a different
		// handler is dead configuration that would otherwise fail silently
		// — no routes, no middleware, no explanation.
		a.Logger.Error("Options.Handler is set, so the built-in Router is not served — " +
			"routes and middleware registered on app.Router will never run")
	}

	// Bind before starting the server goroutine so we can return the
	// bind error directly and avoid racing the readiness flag.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}

	useTLS := a.tlsCert != "" && a.tlsKey != ""

	a.server = &http.Server{
		Handler:           handler,
		ReadTimeout:       a.readTimeout,
		ReadHeaderTimeout: a.readHeaderTimeout,
		WriteTimeout:      a.writeTimeout,
		IdleTimeout:       a.idleTimeout,
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

	// Keep serving while the load balancer notices we are unready. The
	// listener stays open throughout, so requests still in flight or
	// already routed here complete normally instead of being refused.
	if a.preStopDelay > 0 {
		a.Logger.Info("draining before shutdown",
			"pre_stop_delay", a.preStopDelay.String(),
		)
		time.Sleep(a.preStopDelay)
	}

	drainCtx, drainCancel := context.WithTimeout(context.Background(), a.shutdownTimeout)
	defer drainCancel()

	if err := a.server.Shutdown(drainCtx); err != nil {
		a.Logger.Error("server shutdown error", "error", err)
	}

	runShutdownHooks()

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

// LivenessHandler returns 200 while the process is alive. Kubernetes uses
// this to decide whether to restart the container.
//
// Exported so it can be mounted on a router of your own when Options.Handler
// is set. With the built-in Router it is registered automatically at
// GET /healthz.
func (a *App) LivenessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		JSON(w, http.StatusOK, Map{"status": "alive"})
	})
}

// ReadinessHandler returns 200 if the service is ready to handle traffic,
// 503 otherwise. Failure responses list only the names of failing checks;
// set HEALTH_DETAIL=true to include error messages for debugging.
//
// Exported so it can be mounted on a router of your own when Options.Handler
// is set. With the built-in Router it is registered automatically at
// GET /readyz.
func (a *App) ReadinessHandler() http.Handler {
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
