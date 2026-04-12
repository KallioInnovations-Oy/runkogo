package runko

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCircuitBreaker_ClosedByDefault(t *testing.T) {
	cb := newCircuitBreaker(3, 1*time.Second)

	if !cb.allow() {
		t.Error("circuit breaker should be closed (allowing) by default")
	}
}

func TestCircuitBreaker_OpensAfterThreshold(t *testing.T) {
	cb := newCircuitBreaker(3, 1*time.Second)

	// Record failures up to threshold.
	for i := 0; i < 3; i++ {
		cb.recordFailure()
	}

	if cb.allow() {
		t.Error("circuit breaker should be open after reaching threshold")
	}
}

func TestCircuitBreaker_ResetOnSuccess(t *testing.T) {
	cb := newCircuitBreaker(3, 1*time.Second)

	cb.recordFailure()
	cb.recordFailure()
	// 2 failures, then a success should reset.
	cb.recordSuccess()

	// Should still be closed.
	if !cb.allow() {
		t.Error("circuit breaker should reset to closed after success")
	}

	// Need 3 more failures to trip again.
	cb.recordFailure()
	cb.recordFailure()
	if !cb.allow() {
		t.Error("circuit breaker should still be closed with only 2 failures after reset")
	}
}

func TestCircuitBreaker_HalfOpenAfterCooldown(t *testing.T) {
	cb := newCircuitBreaker(2, 50*time.Millisecond)

	cb.recordFailure()
	cb.recordFailure()

	// Should be open.
	if cb.allow() {
		t.Error("circuit breaker should be open")
	}

	// Wait for cooldown.
	time.Sleep(60 * time.Millisecond)

	// First call after cooldown should be allowed (half-open probe).
	if !cb.allow() {
		t.Error("circuit breaker should allow one probe after cooldown")
	}

	// Second call while probing should be rejected.
	if cb.allow() {
		t.Error("circuit breaker should reject additional requests during half-open probe")
	}
}

func TestCircuitBreaker_HalfOpen_SuccessCloses(t *testing.T) {
	cb := newCircuitBreaker(2, 50*time.Millisecond)

	cb.recordFailure()
	cb.recordFailure()

	time.Sleep(60 * time.Millisecond)

	// Probe allowed.
	cb.allow()

	// Probe succeeds — circuit should close.
	cb.recordSuccess()

	if !cb.allow() {
		t.Error("circuit breaker should be closed after successful probe")
	}
}

func TestCircuitBreaker_HalfOpen_FailureReopens(t *testing.T) {
	cb := newCircuitBreaker(2, 50*time.Millisecond)

	cb.recordFailure()
	cb.recordFailure()

	time.Sleep(60 * time.Millisecond)

	// Probe allowed.
	cb.allow()

	// Probe fails — circuit should reopen.
	cb.recordFailure()

	if cb.allow() {
		t.Error("circuit breaker should reopen after failed probe")
	}
}

// FIX-02: POST should not be retried on 5xx by default.
func TestDo_POST_NoRetryByDefault(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	sc := NewServiceClient(ServiceClientConfig{
		BaseURL:    srv.URL,
		Timeout:    2 * time.Second,
		MaxRetries: 3,
		RetryDelay: 10 * time.Millisecond,
	})

	_, err := sc.Post(context.Background(), "/test", map[string]string{"key": "val"})
	if err == nil {
		t.Fatal("expected error from POST to 500 endpoint")
	}

	if got := count.Load(); got != 1 {
		t.Errorf("POST should not retry by default: got %d attempts, want 1", got)
	}
}

// FIX-02: GET should still be retried on 5xx.
func TestDo_GET_RetriesOn5xx(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sc := NewServiceClient(ServiceClientConfig{
		BaseURL:    srv.URL,
		Timeout:    2 * time.Second,
		MaxRetries: 3,
		RetryDelay: 10 * time.Millisecond,
	})

	resp, err := sc.Get(context.Background(), "/test")
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	resp.Body.Close()

	if got := count.Load(); got != 3 {
		t.Errorf("GET should retry on 5xx: got %d attempts, want 3", got)
	}
}

// FIX-02: POST retries when opt-in via RetryNonIdempotent.
func TestDo_POST_RetriesWhenOptIn(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		if n <= 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sc := NewServiceClient(ServiceClientConfig{
		BaseURL:            srv.URL,
		Timeout:            2 * time.Second,
		MaxRetries:         3,
		RetryDelay:         10 * time.Millisecond,
		RetryNonIdempotent: true,
	})

	resp, err := sc.Post(context.Background(), "/test", map[string]string{"key": "val"})
	if err != nil {
		t.Fatalf("expected success with RetryNonIdempotent, got: %v", err)
	}
	resp.Body.Close()

	if got := count.Load(); got != 2 {
		t.Errorf("POST with RetryNonIdempotent should retry: got %d attempts, want 2", got)
	}
}

// AUDIT3-07: ServiceClient should reject paths not starting with "/".
func TestServiceClient_PathValidation(t *testing.T) {
	sc := NewServiceClient(ServiceClientConfig{
		BaseURL: "http://example.com",
	})

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"valid path", "/api/users", false},
		{"no leading slash", "api/users", true},
		{"at-sign attack", "@evil.com/steal", true},
		{"empty path", "", true},
		{"root path", "/", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := sc.Get(context.Background(), tt.path)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "path must start with") {
					t.Errorf("expected path validation error, got: %v", err)
				}
			}
			// For valid paths, errors come from connection failure, not validation.
		})
	}
}

// AUDIT3-09: Retry backoff should include jitter.
func TestRetryBackoff_HasJitter(t *testing.T) {
	var timestamps []time.Time

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timestamps = append(timestamps, time.Now())
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	sc := NewServiceClient(ServiceClientConfig{
		BaseURL:    srv.URL,
		Timeout:    2 * time.Second,
		MaxRetries: 3,
		RetryDelay: 100 * time.Millisecond,
	})

	sc.Get(context.Background(), "/test")

	if len(timestamps) < 3 {
		t.Fatalf("expected at least 3 attempts, got %d", len(timestamps))
	}

	// First retry delay should be roughly 50-100ms (jittered from 100ms base).
	delay1 := timestamps[1].Sub(timestamps[0])
	if delay1 < 30*time.Millisecond || delay1 > 150*time.Millisecond {
		t.Errorf("first retry delay = %v, expected ~50-100ms (jittered)", delay1)
	}
}

// FIX-05: Response body should be limited by MaxResponseSize.
func TestDo_ResponseBodyLimited(t *testing.T) {
	// Server sends 2KB body.
	bigBody := strings.Repeat("x", 2048)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(bigBody))
	}))
	defer srv.Close()

	sc := NewServiceClient(ServiceClientConfig{
		BaseURL:         srv.URL,
		Timeout:         2 * time.Second,
		MaxResponseSize: 1024, // Limit to 1KB.
	})

	resp, err := sc.Get(context.Background(), "/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if len(data) > 1024 {
		t.Errorf("response body should be limited to 1024 bytes, got %d", len(data))
	}
}
