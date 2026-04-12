package runko

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

// freePort finds an available TCP port by briefly binding to :0.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return strconv.Itoa(port)
}

// newTestApp creates an App bound to an ephemeral port with minimal config.
func newTestApp(t *testing.T) *App {
	t.Helper()
	port := freePort(t)
	os.Setenv("PORT", port)
	t.Cleanup(func() { os.Unsetenv("PORT") })

	app := New(Options{
		ServiceName:     "test-app",
		ShutdownTimeout: 2 * time.Second,
		LogLevel:        "error", // quiet during tests
	})
	return app
}

// waitReady polls until the app is ready or the deadline expires.
func waitReady(t *testing.T, app *App, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		app.health.mu.RLock()
		ready := app.health.ready
		app.health.mu.RUnlock()
		if ready && app.server != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("app did not become ready within timeout")
}

func TestApp_New_Defaults(t *testing.T) {
	app := New(Options{})

	if app.serviceName != "runko-app" {
		t.Errorf("default service name = %q, want %q", app.serviceName, "runko-app")
	}
	if app.shutdownTimeout != 15*time.Second {
		t.Errorf("default shutdown timeout = %v, want 15s", app.shutdownTimeout)
	}
	if app.Config == nil {
		t.Error("Config should not be nil")
	}
	if app.Logger == nil {
		t.Error("Logger should not be nil")
	}
	if app.Router == nil {
		t.Error("Router should not be nil")
	}
	if app.Proxy == nil {
		t.Error("Proxy should not be nil")
	}
	if app.health.ready {
		t.Error("health should not be ready before Run()")
	}
}

func TestApp_New_CustomOptions(t *testing.T) {
	app := New(Options{
		ServiceName:     "my-svc",
		ShutdownTimeout: 5 * time.Second,
		LogLevel:        "debug",
	})

	if app.serviceName != "my-svc" {
		t.Errorf("service name = %q, want %q", app.serviceName, "my-svc")
	}
	if app.shutdownTimeout != 5*time.Second {
		t.Errorf("shutdown timeout = %v, want 5s", app.shutdownTimeout)
	}
}

func TestApp_New_PartialTLS_Panics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for partial TLS config")
		}
	}()

	New(Options{TLSCert: "cert.pem"}) // TLSKey missing
}

func TestApp_StartupHook_Called(t *testing.T) {
	app := newTestApp(t)

	hookCalled := make(chan struct{})
	app.OnStartup(func(ctx context.Context) error {
		close(hookCalled)
		return nil
	})

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	// Wait for the hook to be called.
	select {
	case <-hookCalled:
		// success
	case err := <-errCh:
		t.Fatalf("Run() returned early: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("startup hook was not called within timeout")
	}

	// Clean shutdown via context cancellation (graceful path).
	waitReady(t, app, 3*time.Second)
	app.stop()
	<-errCh
}

func TestApp_StartupHook_Error_PreventsStart(t *testing.T) {
	app := newTestApp(t)

	app.OnStartup(func(ctx context.Context) error {
		return fmt.Errorf("db connection failed")
	})

	err := app.Run()
	if err == nil {
		t.Fatal("expected error when startup hook fails")
	}
	if err.Error() != "startup failed: db connection failed" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestApp_ShutdownHook_Called(t *testing.T) {
	app := newTestApp(t)

	hookCalled := make(chan struct{})
	app.OnShutdown(func(ctx context.Context) error {
		close(hookCalled)
		return nil
	})

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	waitReady(t, app, 3*time.Second)

	// Trigger graceful shutdown via context cancellation.
	app.stop()
	<-errCh

	select {
	case <-hookCalled:
		// success
	default:
		t.Error("shutdown hook was not called")
	}
}

func TestApp_LivenessHandler(t *testing.T) {
	app := New(Options{ServiceName: "test", LogLevel: "error"})
	handler := app.livenessHandler()

	rec := &responseRecorder{headers: http.Header{}}
	req, _ := http.NewRequest("GET", "/healthz", nil)
	handler.ServeHTTP(rec, req)

	if rec.statusCode != http.StatusOK {
		t.Errorf("liveness status = %d, want 200", rec.statusCode)
	}

	var result map[string]string
	json.Unmarshal(rec.body, &result)
	if result["status"] != "alive" {
		t.Errorf("liveness status = %q, want %q", result["status"], "alive")
	}
}

func TestApp_ReadinessHandler_Ready_NoChecks(t *testing.T) {
	app := New(Options{ServiceName: "test", LogLevel: "error"})
	app.health.ready = true

	handler := app.readinessHandler()
	rec := &responseRecorder{headers: http.Header{}}
	req, _ := http.NewRequest("GET", "/readyz", nil)
	handler.ServeHTTP(rec, req)

	if rec.statusCode != http.StatusOK {
		t.Errorf("readiness status = %d, want 200", rec.statusCode)
	}

	var result map[string]string
	json.Unmarshal(rec.body, &result)
	if result["status"] != "ready" {
		t.Errorf("readiness body status = %q, want %q", result["status"], "ready")
	}
}

func TestApp_ReadinessHandler_NotReady(t *testing.T) {
	app := New(Options{ServiceName: "test", LogLevel: "error"})
	// health.ready defaults to false

	handler := app.readinessHandler()
	rec := &responseRecorder{headers: http.Header{}}
	req, _ := http.NewRequest("GET", "/readyz", nil)
	handler.ServeHTTP(rec, req)

	if rec.statusCode != http.StatusServiceUnavailable {
		t.Errorf("readiness status = %d, want 503", rec.statusCode)
	}
}

func TestApp_ReadinessHandler_FailingCheck(t *testing.T) {
	app := New(Options{ServiceName: "test", LogLevel: "error"})
	app.health.ready = true
	app.AddHealthCheck("db", 1*time.Second, func(ctx context.Context) error {
		return fmt.Errorf("connection refused")
	})

	handler := app.readinessHandler()
	rec := &responseRecorder{headers: http.Header{}}
	req, _ := http.NewRequest("GET", "/readyz", nil)
	handler.ServeHTTP(rec, req)

	if rec.statusCode != http.StatusServiceUnavailable {
		t.Errorf("readiness status = %d, want 503", rec.statusCode)
	}

	var result map[string]any
	json.Unmarshal(rec.body, &result)
	if result["status"] != "degraded" {
		t.Errorf("readiness body status = %q, want %q", result["status"], "degraded")
	}

	// Default: failures should list names only, not error details.
	failures, ok := result["failures"].([]any)
	if !ok {
		t.Fatal("failures should be an array of names")
	}
	if len(failures) != 1 || failures[0] != "db" {
		t.Errorf("failures = %v, want [db]", failures)
	}
}

func TestApp_ReadinessHandler_DetailMode(t *testing.T) {
	os.Setenv("HEALTH_DETAIL", "true")
	defer os.Unsetenv("HEALTH_DETAIL")

	app := New(Options{ServiceName: "test", LogLevel: "error"})
	app.health.ready = true
	app.AddHealthCheck("db", 1*time.Second, func(ctx context.Context) error {
		return fmt.Errorf("connection refused")
	})

	handler := app.readinessHandler()
	rec := &responseRecorder{headers: http.Header{}}
	req, _ := http.NewRequest("GET", "/readyz", nil)
	handler.ServeHTTP(rec, req)

	var result map[string]any
	json.Unmarshal(rec.body, &result)

	// Detail mode: failures should be a map with error messages.
	failures, ok := result["failures"].(map[string]any)
	if !ok {
		t.Fatalf("failures in detail mode should be a map, got %T", result["failures"])
	}
	if failures["db"] != "connection refused" {
		t.Errorf("failure detail = %q, want %q", failures["db"], "connection refused")
	}
}

func TestApp_ReadinessHandler_PassingCheck(t *testing.T) {
	app := New(Options{ServiceName: "test", LogLevel: "error"})
	app.health.ready = true
	app.AddHealthCheck("db", 1*time.Second, func(ctx context.Context) error {
		return nil // healthy
	})

	handler := app.readinessHandler()
	rec := &responseRecorder{headers: http.Header{}}
	req, _ := http.NewRequest("GET", "/readyz", nil)
	handler.ServeHTTP(rec, req)

	if rec.statusCode != http.StatusOK {
		t.Errorf("readiness status = %d, want 200", rec.statusCode)
	}
}

func TestApp_ReadinessHandler_CheckTimeout(t *testing.T) {
	app := New(Options{ServiceName: "test", LogLevel: "error"})
	app.health.ready = true

	// Health check that blocks longer than its timeout.
	app.AddHealthCheck("slow", 50*time.Millisecond, func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	})

	handler := app.readinessHandler()
	rec := &responseRecorder{headers: http.Header{}}
	req, _ := http.NewRequest("GET", "/readyz", nil)
	handler.ServeHTTP(rec, req)

	if rec.statusCode != http.StatusServiceUnavailable {
		t.Errorf("readiness status = %d, want 503 (check should timeout)", rec.statusCode)
	}
}

func TestApp_InvalidPort(t *testing.T) {
	os.Setenv("PORT", "notanumber")
	defer os.Unsetenv("PORT")

	app := New(Options{LogLevel: "error"})
	err := app.Run()
	if err == nil {
		t.Fatal("expected error for invalid port")
	}
	if err.Error() != `invalid PORT "notanumber": must be a number between 1 and 65535` {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestApp_PortOutOfRange(t *testing.T) {
	os.Setenv("PORT", "99999")
	defer os.Unsetenv("PORT")

	app := New(Options{LogLevel: "error"})
	err := app.Run()
	if err == nil {
		t.Fatal("expected error for port out of range")
	}
}

func TestApp_RunLifecycle_Full(t *testing.T) {
	app := newTestApp(t)

	// Track lifecycle events in order.
	var events atomicSlice

	app.OnStartup(func(ctx context.Context) error {
		events.Append("startup")
		return nil
	})

	app.OnShutdown(func(ctx context.Context) error {
		events.Append("shutdown")
		return nil
	})

	app.AddHealthCheck("test", 1*time.Second, func(ctx context.Context) error {
		return nil
	})

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	waitReady(t, app, 3*time.Second)

	events.Append("ready")

	// Trigger graceful shutdown.
	app.stop()
	<-errCh

	events.Append("stopped")
	got := events.Get()

	// Verify ordering: startup -> ready -> shutdown -> stopped.
	if len(got) < 4 {
		t.Fatalf("expected 4 events, got %d: %v", len(got), got)
	}
	if got[0] != "startup" {
		t.Errorf("events[0] = %q, want %q", got[0], "startup")
	}
	if got[1] != "ready" {
		t.Errorf("events[1] = %q, want %q", got[1], "ready")
	}
	if got[2] != "shutdown" {
		t.Errorf("events[2] = %q, want %q", got[2], "shutdown")
	}
	if got[3] != "stopped" {
		t.Errorf("events[3] = %q, want %q", got[3], "stopped")
	}
}

func TestApp_GracefulDrain(t *testing.T) {
	app := newTestApp(t)

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	waitReady(t, app, 3*time.Second)

	// Trigger graceful shutdown.
	app.stop()
	<-errCh

	// Verify the app is no longer ready.
	app.health.mu.RLock()
	ready := app.health.ready
	app.health.mu.RUnlock()
	if ready {
		t.Error("app should not be ready after shutdown")
	}
}

// responseRecorder is a minimal http.ResponseWriter for testing handlers
// directly without going through a full HTTP server.
type responseRecorder struct {
	statusCode int
	headers    http.Header
	body       []byte
}

func (r *responseRecorder) Header() http.Header { return r.headers }
func (r *responseRecorder) WriteHeader(code int) { r.statusCode = code }
func (r *responseRecorder) Write(b []byte) (int, error) {
	r.body = append(r.body, b...)
	if r.statusCode == 0 {
		r.statusCode = http.StatusOK
	}
	return len(b), nil
}

// atomicSlice is a thread-safe string slice for tracking ordered events.
type atomicSlice struct {
	mu     sync.Mutex
	events []string
}

func (a *atomicSlice) Append(s string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, s)
}

func (a *atomicSlice) Get() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	cp := make([]string, len(a.events))
	copy(cp, a.events)
	return cp
}
