// RunkoGO example — service-to-service calls in a cluster.
//
// The scaffold (../scaffold) shows a complete single service: routing,
// storage, auth, CSRF, the error taxonomy, the demo UI. This example does
// not repeat any of that. It exists for the one thing the scaffold does
// not cover: what happens when your service has to call another one.
//
// Run it:
//
//	go run .
//	open http://localhost:19100/demo
//
// It starts two HTTP servers in one process:
//
//	:19100  orders  — a RunkoGO app, the service you are writing
//	:19101  users   — a deliberately unreliable peer, a plain net/http
//	                  server, standing in for a service you do not control
//
// "orders" calls "users" through runko.ServiceClient, so you can watch the
// retry, circuit-breaker and trace-propagation behaviour against a peer
// that fails on demand. Everything is observable from the endpoints under
// /demo — no external service and no test harness required.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	runko "github.com/kallioinnovations/runkogo"
)

// ==========================================================================
// The peer service — plain net/http, standing in for something you call
// ==========================================================================

// upstream simulates a service you depend on but do not control. It is
// plain net/http on purpose: RunkoGO's client talks to anything that
// speaks HTTP, and the peer in a real cluster is often not Go at all.
type upstream struct {
	// failuresRemaining makes /flaky fail a set number of times before it
	// recovers, which is how the retry path becomes observable.
	failuresRemaining atomic.Int32

	// calls counts every request that actually arrived, so the demo can
	// show the difference between calls the client made and calls that
	// reached the peer.
	calls atomic.Int64
}

func (u *upstream) routes() *http.ServeMux {
	mux := http.NewServeMux()

	// A healthy endpoint.
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		u.calls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"id":    r.PathValue("id"),
			"name":  "Ada Lovelace",
			"email": "ada@example.com",
		})
	})

	// Fails until its budget is exhausted, then succeeds. Retries make the
	// difference between a 503 and a 200 here.
	mux.HandleFunc("GET /flaky", func(w http.ResponseWriter, r *http.Request) {
		u.calls.Add(1)
		if u.failuresRemaining.Add(-1) >= 0 {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "temporarily unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"recovered": true})
	})

	// Always fails. Used to trip the circuit breaker.
	mux.HandleFunc("GET /down", func(w http.ResponseWriter, r *http.Request) {
		u.calls.Add(1)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "permanently down"})
	})

	// Echoes the correlation headers it received, which is how trace
	// propagation becomes visible.
	mux.HandleFunc("GET /echo", func(w http.ResponseWriter, r *http.Request) {
		u.calls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"seen_by_peer": map[string]string{
				"X-Request-ID": r.Header.Get("X-Request-ID"),
				"X-Trace-ID":   r.Header.Get("X-Trace-ID"),
				"traceparent":  r.Header.Get("traceparent"),
				"tracestate":   r.Header.Get("tracestate"),
			},
		})
	})

	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// ==========================================================================
// The service you are writing
// ==========================================================================

type handlers struct {
	users    *runko.ServiceClient
	upstream *upstream
	peerURL  string
	logger   *slog.Logger
}

// GetOrder is the ordinary case: fetch local data, enrich it from a peer.
//
// The client forwards this request's correlation IDs automatically, so the
// peer's logs line up with ours without any plumbing in the handler.
func (h *handlers) GetOrder(w http.ResponseWriter, r *http.Request) {
	orderID := runko.PathParam(r, "id")

	var customer map[string]any
	if err := h.users.GetJSON(r.Context(), "/users/42", &customer); err != nil {
		// The peer being unavailable is not the client's fault, and its
		// error text is not the client's business — CONV-03, CONV-15.
		runko.RespondError(w, r, h.logger,
			runko.NewAppError(http.StatusBadGateway, "upstream_unavailable",
				"Could not reach the users service").Wrap(err))
		return
	}

	runko.JSON(w, http.StatusOK, runko.Map{
		"order_id": orderID,
		"total":    "42.00",
		"customer": customer,
	})
}

// DemoRetry shows MaxRetries turning a transient failure into a success.
//
// The peer is primed to fail twice, so a client with retries enabled
// eventually gets a 200 while the peer records three separate calls.
func (h *handlers) DemoRetry(w http.ResponseWriter, r *http.Request) {
	h.upstream.failuresRemaining.Store(2)
	before := h.upstream.calls.Load()

	start := time.Now()
	resp, err := h.users.Get(r.Context(), "/flaky")
	elapsed := time.Since(start)

	result := runko.Map{
		"peer_calls":     h.upstream.calls.Load() - before,
		"elapsed_ms":     elapsed.Milliseconds(),
		"explanation":    "the peer failed twice; the client retried with exponential backoff and jitter",
		"conv_reference": "CONV-10",
	}
	if err != nil {
		result["outcome"] = "error: " + err.Error()
		runko.JSON(w, http.StatusOK, result)
		return
	}
	defer resp.Body.Close()
	result["outcome"] = fmt.Sprintf("succeeded with %d after retrying", resp.StatusCode)
	runko.JSON(w, http.StatusOK, result)
}

// DemoBreaker shows the circuit breaker shedding load.
//
// Against a peer that always fails, the breaker opens and later calls stop
// leaving this process at all — which is the point. Compare peer_calls with
// attempts: the gap is load the peer never had to absorb.
func (h *handlers) DemoBreaker(w http.ResponseWriter, r *http.Request) {
	// A dedicated client, so the demo cannot trip the breaker that the
	// other endpoints share.
	client := runko.NewServiceClient(runko.ServiceClientConfig{
		BaseURL:          h.peerURL,
		Timeout:          2 * time.Second,
		MaxRetries:       -1, // one attempt per call, so the breaker math is legible
		CircuitThreshold: 3,
		CircuitCooldown:  30 * time.Second,
	})

	before := h.upstream.calls.Load()
	outcomes := make([]string, 0, 6)

	for i := 1; i <= 6; i++ {
		resp, err := client.Get(r.Context(), "/down")
		switch {
		case err != nil:
			outcomes = append(outcomes, fmt.Sprintf("call %d: %v", i, err))
		default:
			outcomes = append(outcomes, fmt.Sprintf("call %d: HTTP %d", i, resp.StatusCode))
			resp.Body.Close()
		}
	}

	runko.JSON(w, http.StatusOK, runko.Map{
		"outcomes":       outcomes,
		"peer_calls":     h.upstream.calls.Load() - before,
		"attempts":       6,
		"explanation":    "MaxRetries: -1 means one attempt each; after 3 failures the breaker opened and the remaining calls never left this process",
		"conv_reference": "CONV-10",
	})
}

// DemoTrace shows W3C trace context surviving the hop.
//
// Send a traceparent with your request and the peer reports the same value
// back: RunkoGO forwards it byte-for-byte rather than creating a span of
// its own, so it does not sever a trace passing through.
func (h *handlers) DemoTrace(w http.ResponseWriter, r *http.Request) {
	var echoed map[string]any
	if err := h.users.GetJSON(r.Context(), "/echo", &echoed); err != nil {
		runko.RespondError(w, r, h.logger, runko.Internal(err))
		return
	}

	runko.JSON(w, http.StatusOK, runko.Map{
		"sent_by_you": runko.Map{
			"traceparent": r.Header.Get("traceparent"),
			"tracestate":  r.Header.Get("tracestate"),
		},
		"peer_received":  echoed["seen_by_peer"],
		"request_id":     runko.RequestID(r.Context()),
		"explanation":    "traceparent and tracestate are forwarded unchanged; the request ID is generated here and propagated",
		"conv_reference": "CONV-12",
		"try_this":       `curl -H 'traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01' http://localhost:19100/demo/trace`,
	})
}

// Index lists what there is to try.
func (h *handlers) Index(w http.ResponseWriter, r *http.Request) {
	runko.JSON(w, http.StatusOK, runko.Map{
		"what": "RunkoGO service-to-service example. The scaffold covers a single service; this covers calling another one.",
		"endpoints": []runko.Map{
			{"GET /api/v1/orders/{id}": "ordinary call — enriches a local order from the users service"},
			{"GET /demo/retry": "the peer fails twice; watch the client retry (CONV-10)"},
			{"GET /demo/breaker": "the peer always fails; watch the breaker shed load (CONV-10)"},
			{"GET /demo/trace": "send a traceparent header and see it survive the hop (CONV-12)"},
		},
		"peer": "a plain net/http server on :19101, standing in for a service you do not control",
	})
}

func main() {
	app := runko.New(runko.Options{
		ServiceName:     "orders",
		ShutdownTimeout: 10 * time.Second,
		LogLevel:        os.Getenv("LOG_LEVEL"),
		PreStopDelay:    -1, // no load balancer in front of a local demo
	})

	port := app.Config.GetDefault("PORT", "19100")
	peerPort := app.Config.GetDefault("UPSTREAM_PORT", "19101")
	peerURL := "http://127.0.0.1:" + peerPort

	// ---------------------------------------------------------------
	// The peer, started and stopped with this process.
	// ---------------------------------------------------------------
	peer := &upstream{}
	peerServer := &http.Server{
		Addr:              "127.0.0.1:" + peerPort,
		Handler:           peer.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	app.OnStartup(func(ctx context.Context) error {
		go func() {
			if err := peerServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				app.Logger.Error("peer service failed", "error", err)
			}
		}()
		app.Logger.Info("peer service started", "addr", peerServer.Addr)
		return nil
	})
	app.OnShutdown(func(ctx context.Context) error {
		app.Logger.Info("stopping peer service")
		return peerServer.Shutdown(ctx)
	})

	// ---------------------------------------------------------------
	// The client. These are the knobs worth understanding.
	// ---------------------------------------------------------------
	users := runko.NewServiceClient(runko.ServiceClientConfig{
		BaseURL: peerURL,

		// Bounds one attempt, not the whole call — with retries the total
		// can approach Timeout × (MaxRetries + 1) plus backoff.
		Timeout: 2 * time.Second,

		// Retries apply to idempotent methods and 5xx responses. POST is
		// never replayed unless you opt in, because a transport error is
		// not proof the peer did not process the request (CONV-10).
		MaxRetries: 2,
		RetryDelay: 100 * time.Millisecond,

		// The breaker is per client instance and per process. It is not
		// shared across replicas.
		CircuitThreshold: 3,
		CircuitCooldown:  10 * time.Second,

		// Caps the response body so a misbehaving peer cannot exhaust
		// memory here (CONV-06).
		MaxResponseSize: 1 << 20,
	})

	h := &handlers{users: users, upstream: peer, peerURL: peerURL, logger: app.Logger}

	// ---------------------------------------------------------------
	// Middleware. Order matters — see CONV-04 and CONV-08.
	// ---------------------------------------------------------------
	app.Router.Use(
		runko.Recovery(app.Logger),
		runko.BodyLimit(1<<20),
		runko.DefaultSecurityHeaders(),
		runko.RequestIDMiddleware(),
		runko.ClientIPMiddleware(app.Proxy),
		runko.Logger(app.Logger),
	)

	app.Router.HandleFunc("GET /{$}", h.Index)
	app.Router.HandleFunc("GET /demo", h.Index)
	app.Router.HandleFunc("GET /api/v1/orders/{id}", h.GetOrder)
	app.Router.HandleFunc("GET /demo/retry", h.DemoRetry)
	app.Router.HandleFunc("GET /demo/breaker", h.DemoBreaker)
	app.Router.HandleFunc("GET /demo/trace", h.DemoTrace)

	app.AddHealthCheck("users-service", 2*time.Second, func(ctx context.Context) error {
		resp, err := users.Get(ctx, "/users/1")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		return nil
	})

	app.Logger.Info("try it", "url", "http://localhost:"+port+"/demo")

	if err := app.Run(); err != nil {
		app.Logger.Error("application error", "error", err)
		os.Exit(1)
	}
}
