package runko

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ServiceClient is an HTTP client for service-to-service communication.
// It provides automatic retries, circuit breaking, timeout enforcement,
// and request ID propagation across service boundaries.
type ServiceClient struct {
	client          *http.Client
	baseURL         string
	defaultTimeout  time.Duration
	maxRetries      int
	retryDelay      time.Duration
	maxResponseSize int64
	circuit         *circuitBreaker
}

// ServiceClientConfig configures a ServiceClient.
type ServiceClientConfig struct {
	// BaseURL is the root URL of the target service.
	// Example: "http://user-service:8080"
	BaseURL string

	// Timeout is the per-request timeout. Defaults to 10 seconds.
	Timeout time.Duration

	// MaxRetries is how many times to retry on failure. Defaults to 2.
	MaxRetries int

	// RetryDelay is the base delay between retries (doubles each retry).
	// Defaults to 500ms.
	RetryDelay time.Duration

	// CircuitThreshold is how many consecutive failures before the
	// circuit opens (stops sending requests). Defaults to 5.
	CircuitThreshold int

	// CircuitCooldown is how long to wait before trying again after
	// the circuit opens. Defaults to 30 seconds.
	CircuitCooldown time.Duration

	// MaxResponseSize is the maximum response body size in bytes that
	// GetJSON will read. Prevents OOM from malicious or buggy
	// downstream services. Default: 10MB. (CONV-06)
	MaxResponseSize int64
}

// NewServiceClient creates a new HTTP client for calling another service.
func NewServiceClient(cfg ServiceClientConfig) *ServiceClient {
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 2
	}
	if cfg.RetryDelay == 0 {
		cfg.RetryDelay = 500 * time.Millisecond
	}
	if cfg.CircuitThreshold == 0 {
		cfg.CircuitThreshold = 5
	}
	if cfg.CircuitCooldown == 0 {
		cfg.CircuitCooldown = 30 * time.Second
	}
	if cfg.MaxResponseSize == 0 {
		cfg.MaxResponseSize = 10 << 20 // 10MB
	}

	return &ServiceClient{
		client: &http.Client{
			Timeout: cfg.Timeout,
		},
		baseURL:         strings.TrimRight(cfg.BaseURL, "/"),
		defaultTimeout:  cfg.Timeout,
		maxRetries:      cfg.MaxRetries,
		retryDelay:      cfg.RetryDelay,
		maxResponseSize: cfg.MaxResponseSize,
		circuit:         newCircuitBreaker(cfg.CircuitThreshold, cfg.CircuitCooldown),
	}
}

// Get performs a GET request to the given path with context propagation.
func (sc *ServiceClient) Get(ctx context.Context, path string) (*http.Response, error) {
	return sc.do(ctx, http.MethodGet, path, nil)
}

// Post performs a POST request with a JSON body.
func (sc *ServiceClient) Post(ctx context.Context, path string, body any) (*http.Response, error) {
	return sc.do(ctx, http.MethodPost, path, body)
}

// Put performs a PUT request with a JSON body.
func (sc *ServiceClient) Put(ctx context.Context, path string, body any) (*http.Response, error) {
	return sc.do(ctx, http.MethodPut, path, body)
}

// Delete performs a DELETE request.
func (sc *ServiceClient) Delete(ctx context.Context, path string) (*http.Response, error) {
	return sc.do(ctx, http.MethodDelete, path, nil)
}

// GetJSON performs a GET and decodes the JSON response into target.
// The response body is limited to MaxResponseSize to prevent OOM
// from malicious downstream services. (CONV-06)
func (sc *ServiceClient) GetJSON(ctx context.Context, path string, target any) error {
	resp, err := sc.Get(ctx, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		// Drain body to enable connection reuse, but limit the drain.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("service returned %d", resp.StatusCode)
	}

	// Limit response body to prevent OOM. (CONV-06)
	limited := io.LimitReader(resp.Body, sc.maxResponseSize)
	return json.NewDecoder(limited).Decode(target)
}

// do executes the HTTP request with retries and circuit breaking.
func (sc *ServiceClient) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	// Check circuit breaker.
	if !sc.circuit.allow() {
		return nil, fmt.Errorf("circuit breaker open for %s", sc.baseURL)
	}

	url := sc.baseURL + path

	var lastErr error
	for attempt := 0; attempt <= sc.maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff.
			delay := sc.retryDelay * time.Duration(1<<(attempt-1))
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		var bodyReader io.Reader
		if body != nil {
			data, err := json.Marshal(body)
			if err != nil {
				return nil, fmt.Errorf("marshal body: %w", err)
			}
			bodyReader = strings.NewReader(string(data))
		}

		req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}

		// Propagate headers from context (request ID, trace ID).
		if rid := RequestID(ctx); rid != "" {
			req.Header.Set("X-Request-ID", rid)
		}
		if tid := TraceID(ctx); tid != "" {
			req.Header.Set("X-Trace-ID", tid)
		}

		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := sc.client.Do(req)
		if err != nil {
			lastErr = err
			sc.circuit.recordFailure()
			continue
		}

		// Don't retry client errors (4xx), only server errors (5xx).
		if resp.StatusCode >= 500 {
			resp.Body.Close()
			lastErr = fmt.Errorf("server error: %d", resp.StatusCode)
			sc.circuit.recordFailure()
			continue
		}

		sc.circuit.recordSuccess()
		return resp, nil
	}

	return nil, fmt.Errorf("all %d attempts failed for %s %s: %w",
		sc.maxRetries+1, method, url, lastErr)
}

// circuitBreaker prevents cascading failures by stopping requests to
// a service that's consistently failing. It uses a simple consecutive
// failure counter.
//
// States:
//   - Closed (normal): requests pass through
//   - Open (tripped): requests rejected immediately
//   - Half-open (after cooldown): ONE request allowed to test recovery;
//     all others are rejected until the probe succeeds or fails.
type circuitBreaker struct {
	mu          sync.Mutex
	failures    int
	threshold   int
	cooldown    time.Duration
	lastFailure time.Time
	state       string // "closed", "open", "half-open"
}

func newCircuitBreaker(threshold int, cooldown time.Duration) *circuitBreaker {
	return &circuitBreaker{
		threshold: threshold,
		cooldown:  cooldown,
		state:     "closed",
	}
}

func (cb *circuitBreaker) allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case "closed":
		return true
	case "open":
		// Check if cooldown has elapsed.
		if time.Since(cb.lastFailure) > cb.cooldown {
			// Transition to half-open and allow exactly one probe.
			cb.state = "half-open"
			return true
		}
		return false
	case "half-open":
		// Already probing — reject additional requests until the
		// probe completes. This prevents the thundering herd problem
		// where many goroutines all probe simultaneously.
		return false
	}
	return true
}

func (cb *circuitBreaker) recordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures = 0
	cb.state = "closed"
}

func (cb *circuitBreaker) recordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures++
	cb.lastFailure = time.Now()
	if cb.failures >= cb.threshold {
		cb.state = "open"
	}
}
