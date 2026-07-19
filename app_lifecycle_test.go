package runko

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Shutdown hooks release what startup hooks acquired. If Run() returns
// early — a failed listen, a server error — those hooks must still run or
// database pools and file handles leak for the life of the process.
func TestApp_ShutdownHooksRunWhenListenFails(t *testing.T) {
	// Occupy a port so the app's listen fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	os.Setenv("HOST", "127.0.0.1")
	os.Setenv("PORT", port)
	t.Cleanup(func() { os.Unsetenv("PORT"); os.Unsetenv("HOST") })

	app := New(Options{ServiceName: "test", LogLevel: "error", PreStopDelay: -1})

	var mu sync.Mutex
	startupRan, shutdownRan := false, false
	app.OnStartup(func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		startupRan = true
		return nil
	})
	app.OnShutdown(func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		shutdownRan = true
		return nil
	})

	if err := app.Run(); err == nil {
		t.Fatal("expected Run() to fail on an occupied port")
	}

	mu.Lock()
	defer mu.Unlock()
	if !startupRan {
		t.Fatal("startup hook did not run")
	}
	if !shutdownRan {
		t.Error("shutdown hook did not run after a failed listen — resources leak")
	}
}

// When a startup hook fails, hooks that already succeeded must be rolled
// back via the shutdown hooks.
func TestApp_ShutdownHooksRunWhenLaterStartupHookFails(t *testing.T) {
	app := newTestApp(t)

	var mu sync.Mutex
	shutdownRan := false

	app.OnStartup(func(ctx context.Context) error { return nil }) // succeeds
	app.OnStartup(func(ctx context.Context) error {
		return fmt.Errorf("boom")
	})
	app.OnShutdown(func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		shutdownRan = true
		return nil
	})

	if err := app.Run(); err == nil {
		t.Fatal("expected Run() to fail")
	}

	mu.Lock()
	defer mu.Unlock()
	if !shutdownRan {
		t.Error("shutdown hook did not run after a partial startup — earlier hooks leak")
	}
}

// When the very first startup hook fails, nothing was acquired. Shutdown
// hooks may legitimately assume startup ran, so they must be skipped
// rather than handed uninitialised state.
func TestApp_ShutdownHooksSkippedWhenFirstStartupHookFails(t *testing.T) {
	app := newTestApp(t)

	var mu sync.Mutex
	shutdownRan := false

	app.OnStartup(func(ctx context.Context) error {
		return fmt.Errorf("boom")
	})
	app.OnShutdown(func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		shutdownRan = true
		return nil
	})

	if err := app.Run(); err == nil {
		t.Fatal("expected Run() to fail")
	}

	mu.Lock()
	defer mu.Unlock()
	if shutdownRan {
		t.Error("shutdown hook ran even though no startup hook succeeded")
	}
}

// Shutdown hooks must run exactly once on the normal path, not twice via
// both the explicit call and the deferred guard.
func TestApp_ShutdownHooksRunExactlyOnce(t *testing.T) {
	app := newTestApp(t)

	var mu sync.Mutex
	calls := 0
	app.OnShutdown(func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return nil
	})

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	waitReady(t, app, 3*time.Second)
	app.stop()
	<-errCh

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("shutdown hook ran %d times, want 1", calls)
	}
}

// CONV-13: readiness flips to false before the listener closes, and the
// server keeps serving for PreStopDelay so the load balancer has time to
// stop routing here. Closing immediately is what produces 502s on rolling
// deploys.
func TestApp_PreStopDelayKeepsServingAfterUnready(t *testing.T) {
	app := newTestApp(t)
	app.preStopDelay = 500 * time.Millisecond
	baseURL := "http://127.0.0.1:" + os.Getenv("PORT") // newTestApp set PORT

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()

	waitReady(t, app, 3*time.Second)

	start := time.Now()
	app.stop()

	// Readiness must go false promptly, well before the listener closes.
	deadline := time.Now().Add(2 * time.Second)
	for {
		app.health.mu.RLock()
		ready := app.health.ready
		app.health.mu.RUnlock()
		if !ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("readiness never flipped to false")
		}
		time.Sleep(5 * time.Millisecond)
	}
	unreadyAt := time.Since(start)
	if unreadyAt > 400*time.Millisecond {
		t.Errorf("readiness flipped after %v — should be immediate, before the drain delay", unreadyAt)
	}

	// The load-bearing claim: the listener is still open and serving while
	// the balancer is being told we are unready. Without the delay this
	// request is refused, which is exactly the 502 seen on rolling deploys.
	resp, err := http.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("server refused a request during the pre-stop delay: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz during drain = %d, want 200", resp.StatusCode)
	}

	// Readiness, meanwhile, reports unready so the balancer drains us.
	resp, err = http.Get(baseURL + "/readyz")
	if err != nil {
		t.Fatalf("readyz during drain: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/readyz during drain = %d, want 503", resp.StatusCode)
	}

	<-errCh
	if total := time.Since(start); total < 500*time.Millisecond {
		t.Errorf("Run() returned after %v, want at least the 500ms pre-stop delay", total)
	}

	// Once shutdown completes the listener really is closed.
	if _, err := http.Get(baseURL + "/healthz"); err == nil {
		t.Error("server still accepting requests after shutdown completed")
	}
}

// A negative PreStopDelay disables the wait; zero selects the default.
func TestApp_PreStopDelayDefaults(t *testing.T) {
	if got := New(Options{PreStopDelay: -1}).preStopDelay; got != 0 {
		t.Errorf("negative PreStopDelay = %v, want 0 (disabled)", got)
	}
	if got := New(Options{}).preStopDelay; got != defaultPreStopDelay {
		t.Errorf("unset PreStopDelay = %v, want %v", got, defaultPreStopDelay)
	}
	if got := New(Options{PreStopDelay: 2 * time.Second}).preStopDelay; got != 2*time.Second {
		t.Errorf("explicit PreStopDelay = %v, want 2s", got)
	}
}

// CONV-13: the shutdown phases are sequential, and hooks receive a fresh
// full ShutdownTimeout rather than whatever the drain left over. This is
// the arithmetic the documented shutdown budget rests on.
func TestApp_ShutdownHooksGetFreshTimeout(t *testing.T) {
	app := newTestApp(t) // ShutdownTimeout: 2s

	var mu sync.Mutex
	var hookDeadline time.Duration
	app.OnShutdown(func(ctx context.Context) error {
		dl, ok := ctx.Deadline()
		if !ok {
			t.Error("shutdown hook context has no deadline")
			return nil
		}
		mu.Lock()
		hookDeadline = time.Until(dl)
		mu.Unlock()
		return nil
	})

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()
	waitReady(t, app, 3*time.Second)
	app.stop()
	<-errCh

	mu.Lock()
	defer mu.Unlock()
	// A fresh 2s budget, not a remnant of the drain phase.
	if hookDeadline < 1500*time.Millisecond {
		t.Errorf("shutdown hook deadline = %v, want ~2s — hooks are being starved by the drain phase", hookDeadline)
	}
}

// CONV-07: the HTTP server sets read, read-header, write and idle
// timeouts. Nothing waits indefinitely on a client that stalls.
func TestApp_ServerTimeoutsAreSet(t *testing.T) {
	app := newTestApp(t)

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()
	waitReady(t, app, 3*time.Second)

	srv := app.server
	checks := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"ReadTimeout", srv.ReadTimeout, 30 * time.Second},
		{"ReadHeaderTimeout", srv.ReadHeaderTimeout, 10 * time.Second},
		{"WriteTimeout", srv.WriteTimeout, 30 * time.Second},
		{"IdleTimeout", srv.IdleTimeout, 120 * time.Second},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
		if c.got == 0 {
			t.Errorf("%s is unset — the connection can be held open indefinitely", c.name)
		}
	}

	if srv.MaxHeaderBytes != defaultMaxHeaderBytes {
		t.Errorf("MaxHeaderBytes = %d, want %d", srv.MaxHeaderBytes, defaultMaxHeaderBytes)
	}

	app.stop()
	<-errCh
}

// CONV-07: the timeouts are configurable. Zero selects the default,
// negative disables — http.Server spells "no timeout" as zero, so the
// Options convention has to translate.
func TestApp_TimeoutOptions(t *testing.T) {
	defaults := New(Options{})
	if defaults.readTimeout != defaultReadTimeout ||
		defaults.readHeaderTimeout != defaultReadHeaderTimeout ||
		defaults.writeTimeout != defaultWriteTimeout ||
		defaults.idleTimeout != defaultIdleTimeout {
		t.Errorf("unset timeouts did not fall back to defaults: %+v", defaults)
	}

	custom := New(Options{
		ReadTimeout:       time.Minute,
		ReadHeaderTimeout: 2 * time.Second,
		WriteTimeout:      -1, // streaming: no write deadline
		IdleTimeout:       -1,
	})
	if custom.readTimeout != time.Minute {
		t.Errorf("ReadTimeout = %v, want 1m", custom.readTimeout)
	}
	if custom.readHeaderTimeout != 2*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 2s", custom.readHeaderTimeout)
	}
	if custom.writeTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 (disabled) for a negative value", custom.writeTimeout)
	}
	if custom.idleTimeout != 0 {
		t.Errorf("IdleTimeout = %v, want 0 (disabled) for a negative value", custom.idleTimeout)
	}
}

// Disabling WriteTimeout is what makes streaming possible. Without it the
// server cuts the response at 30s regardless of the handler flushing.
func TestApp_StreamingWorksWithoutWriteTimeout(t *testing.T) {
	port := freePort(t)
	os.Setenv("PORT", port)
	t.Cleanup(func() { os.Unsetenv("PORT") })

	app := New(Options{
		ServiceName:  "stream-test",
		LogLevel:     "error",
		PreStopDelay: -1,
		WriteTimeout: -1, // the point of the test
	})

	app.Router.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter does not implement http.Flusher — the middleware chain broke streaming")
			return
		}
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: chunk-%d\n\n", i)
			flusher.Flush()
		}
	})

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()
	waitReady(t, app, 3*time.Second)

	if app.server.WriteTimeout != 0 {
		t.Errorf("server WriteTimeout = %v, want 0", app.server.WriteTimeout)
	}

	resp, err := http.Get("http://127.0.0.1:" + port + "/stream")
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	for i := 0; i < 3; i++ {
		if !strings.Contains(string(body), fmt.Sprintf("data: chunk-%d", i)) {
			t.Errorf("stream body missing chunk-%d: %q", i, body)
		}
	}

	app.stop()
	<-errCh
}

// Options.Handler lets the lifecycle, health checks and middleware be used
// with a different router. The health endpoints are then the caller's to
// mount, via the exported handlers.
func TestApp_CustomHandlerIsServed(t *testing.T) {
	port := freePort(t)
	os.Setenv("PORT", port)
	t.Cleanup(func() { os.Unsetenv("PORT") })

	// Stand-in for chi or any other router.
	mux := http.NewServeMux()

	app := New(Options{
		ServiceName:  "byo-router",
		LogLevel:     "error",
		PreStopDelay: -1,
		Handler:      mux,
	})

	mux.HandleFunc("GET /mine", func(w http.ResponseWriter, r *http.Request) {
		JSON(w, http.StatusOK, Map{"from": "custom router"})
	})
	// Health endpoints are not auto-registered — mount them where we want.
	mux.Handle("GET /live", app.LivenessHandler())
	mux.Handle("GET /ready", app.ReadinessHandler())

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run() }()
	waitReady(t, app, 3*time.Second)

	base := "http://127.0.0.1:" + port

	resp, err := http.Get(base + "/mine")
	if err != nil {
		t.Fatalf("custom route: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "custom router") {
		t.Errorf("custom route: status=%d body=%q", resp.StatusCode, body)
	}

	// The exported health handlers work wherever they are mounted.
	for path, want := range map[string]int{"/live": 200, "/ready": 200} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: status = %d, want %d", path, resp.StatusCode, want)
		}
	}

	// The framework must NOT have registered its own health routes on a
	// router it is not serving.
	resp, err = http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("/healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/healthz = %d, want 404 — health endpoints should not be auto-mounted on a custom handler", resp.StatusCode)
	}

	app.stop()
	<-errCh
}
