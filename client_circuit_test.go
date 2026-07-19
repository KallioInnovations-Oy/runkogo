package runko

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A slot granted by allow() must be settled on every exit path. The
// half-open state has exactly one probe slot and only recordSuccess,
// recordFailure, or recordAbort can leave it — a leaked slot wedges the
// breaker permanently and every later call returns "circuit breaker open".
func TestCircuitBreaker_AbortReleasesHalfOpenProbe(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	cb := newCircuitBreaker(1, 30*time.Second, clock)

	// Trip the breaker.
	cb.recordFailure()
	if cb.allow() {
		t.Fatal("breaker should be open immediately after tripping")
	}

	// Let the cooldown elapse; the next allow() consumes the probe slot.
	now = now.Add(31 * time.Second)
	if !cb.allow() {
		t.Fatal("breaker should grant a probe after cooldown")
	}
	if cb.allow() {
		t.Fatal("half-open should permit only one probe")
	}

	// The probe ends without an upstream verdict — e.g. the caller's
	// context was cancelled during retry backoff.
	cb.recordAbort()

	// The slot must be released: after another cooldown a new probe is
	// granted rather than the breaker staying wedged forever.
	now = now.Add(31 * time.Second)
	if !cb.allow() {
		t.Fatal("breaker wedged in half-open after an aborted probe")
	}
}

// A cancelled context during retry backoff must not wedge the client.
//
// The threshold is 1 and the injected clock reports the cooldown as always
// elapsed, so the retry's allow() genuinely enters half-open — the only
// state where a leaked probe slot is permanent. A high threshold would
// keep the breaker closed and the test could never observe the bug.
func TestServiceClient_ContextCancellationDoesNotWedgeBreaker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	// A clock that advances a full minute on every read, so the cooldown
	// has always elapsed regardless of the host's timer granularity.
	tick := time.Now()
	clock := func() time.Time {
		tick = tick.Add(time.Minute)
		return tick
	}

	sc := NewServiceClient(ServiceClientConfig{
		BaseURL:          srv.URL,
		Timeout:          2 * time.Second,
		MaxRetries:       3,
		RetryDelay:       500 * time.Millisecond,
		CircuitThreshold: 1,
		CircuitCooldown:  time.Second,
		Clock:            clock,
	})

	// Attempt 0 fails and opens the breaker. The retry's allow() then moves
	// it to half-open, consuming the single probe slot. Cancel during that
	// backoff so the call exits without an upstream verdict.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := sc.Get(ctx, "/x"); err == nil {
		t.Fatal("expected an error from the cancelled call")
	}

	if state := sc.circuit.currentState(); state == "half-open" {
		t.Fatalf("breaker left in half-open after a cancelled retry — permanently wedged")
	}

	// And it must still be usable.
	if !sc.circuit.allow() {
		t.Error("breaker refused a new call after a cancelled request")
	}
}

// Retries must stop once this call's own failures open the breaker.
// Otherwise a single call keeps loading a service the breaker has already
// decided to shed.
func TestServiceClient_StopsRetryingOnceBreakerOpens(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	sc := NewServiceClient(ServiceClientConfig{
		BaseURL:          srv.URL,
		Timeout:          2 * time.Second,
		MaxRetries:       5,
		RetryDelay:       time.Millisecond,
		CircuitThreshold: 2, // opens after 2 failed attempts
		CircuitCooldown:  time.Minute,
	})

	if _, err := sc.Get(context.Background(), "/x"); err == nil {
		t.Fatal("expected an error")
	}

	// Threshold 2 means the breaker opens after the second attempt, so a
	// third must never reach the server despite MaxRetries being 5.
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("server saw %d attempts, want 2 (retries continued past an open breaker)", got)
	}
}

// A transport error is not proof the request was not processed. POST must
// not be replayed on one unless the caller opted in.
func TestServiceClient_TransportErrorDoesNotRetryPost(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		// Break the connection after receiving the request, simulating a
		// reset that arrives after the server already applied the write.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		conn.Close()
	}))
	defer srv.Close()

	sc := NewServiceClient(ServiceClientConfig{
		BaseURL:          srv.URL,
		Timeout:          2 * time.Second,
		MaxRetries:       3,
		RetryDelay:       time.Millisecond,
		CircuitThreshold: 100,
		CircuitCooldown:  time.Minute,
	})

	if _, err := sc.Post(context.Background(), "/charge", Map{"amount": 100}); err == nil {
		t.Fatal("expected an error")
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("server saw %d POSTs, want 1 — a non-idempotent request was replayed", got)
	}
}

// Idempotent methods are still retried on transport errors.
func TestServiceClient_TransportErrorRetriesGet(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		conn.Close()
	}))
	defer srv.Close()

	sc := NewServiceClient(ServiceClientConfig{
		BaseURL:          srv.URL,
		Timeout:          2 * time.Second,
		MaxRetries:       2,
		RetryDelay:       time.Millisecond,
		CircuitThreshold: 100,
		CircuitCooldown:  time.Minute,
	})

	if _, err := sc.Get(context.Background(), "/x"); err == nil {
		t.Fatal("expected an error")
	}

	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("server saw %d GETs, want 3 (1 initial + 2 retries)", got)
	}
}

// recordAbort must restart the cooldown, not just release the state.
// Without refreshing lastFailure, allow() would grant a probe immediately
// after every abort, so a caller cancelling in a loop could issue
// unlimited probes at a service the breaker exists to shield.
func TestCircuitBreaker_AbortRestartsCooldown(t *testing.T) {
	now := time.Now()
	cb := newCircuitBreaker(1, 30*time.Second, func() time.Time { return now })

	cb.recordFailure() // open
	now = now.Add(31 * time.Second)
	if !cb.allow() {
		t.Fatal("expected a probe after the cooldown elapsed")
	}

	// Abort the probe without advancing the clock.
	cb.recordAbort()

	if cb.allow() {
		t.Error("breaker granted a probe immediately after an abort — cooldown was not restarted")
	}

	// It recovers once the cooldown elapses again.
	now = now.Add(31 * time.Second)
	if !cb.allow() {
		t.Error("breaker did not grant a probe after the restarted cooldown elapsed")
	}
}

// CONV-10: zero means "use the default of 2", so a negative value is the
// only way to express "attempt once". Without it there is no way to put a
// client in front of an endpoint that must never be replayed.
func TestServiceClient_NegativeMaxRetriesDisablesRetries(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	sc := NewServiceClient(ServiceClientConfig{
		BaseURL:          srv.URL,
		Timeout:          2 * time.Second,
		MaxRetries:       -1, // one attempt, no retries
		RetryDelay:       time.Millisecond,
		CircuitThreshold: 100,
		CircuitCooldown:  time.Minute,
	})

	if _, err := sc.Get(context.Background(), "/x"); err == nil {
		t.Fatal("expected an error")
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("server saw %d attempts, want 1 — a negative MaxRetries did not disable retries", got)
	}
}

// Zero still selects the default, so existing callers are unaffected.
func TestServiceClient_ZeroMaxRetriesUsesDefault(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	sc := NewServiceClient(ServiceClientConfig{
		BaseURL:          srv.URL,
		Timeout:          2 * time.Second,
		MaxRetries:       0, // "use the default"
		RetryDelay:       time.Millisecond,
		CircuitThreshold: 100,
		CircuitCooldown:  time.Minute,
	})

	if _, err := sc.Get(context.Background(), "/x"); err == nil {
		t.Fatal("expected an error")
	}

	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("server saw %d attempts, want 3 (1 initial + the default 2 retries)", got)
	}
}

// The error must report attempts actually made. Reporting the configured
// ceiling sends whoever is debugging a truncated call looking for requests
// that were never sent.
func TestServiceClient_ErrorReportsActualAttempts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	// MaxRetries allows 5, but the breaker opens after 2 failures, so the
	// loop ends early and the message must say 2 rather than 6.
	sc := NewServiceClient(ServiceClientConfig{
		BaseURL:          srv.URL,
		Timeout:          2 * time.Second,
		MaxRetries:       5,
		RetryDelay:       time.Millisecond,
		CircuitThreshold: 2,
		CircuitCooldown:  time.Minute,
	})

	_, err := sc.Get(context.Background(), "/x")
	if err == nil {
		t.Fatal("expected an error")
	}

	if !strings.Contains(err.Error(), "after 2 attempt(s)") {
		t.Errorf("error = %q, want it to report the 2 attempts actually made, not the configured ceiling", err)
	}
}
