package runko

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ServiceClient is an HTTP client for service-to-service communication with
// automatic retries, circuit breaking, timeout enforcement, and request-ID
// propagation.
type ServiceClient struct {
	client             *http.Client
	baseURL            string
	defaultTimeout     time.Duration
	maxRetries         int
	retryDelay         time.Duration
	maxResponseSize    int64
	retryNonIdempotent bool
	circuit            *circuitBreaker
}

// ServiceClientConfig configures a ServiceClient.
type ServiceClientConfig struct {
	// BaseURL is the root URL of the target service.
	// Example: "http://user-service:8080".
	BaseURL string

	// Timeout is the per-request timeout. Defaults to 10 seconds.
	Timeout time.Duration

	// MaxRetries is how many times to retry on failure. Defaults to 2.
	MaxRetries int

	// RetryDelay is the base delay between retries (doubles each retry).
	// Defaults to 500ms.
	RetryDelay time.Duration

	// CircuitThreshold is how many consecutive failures before the circuit
	// opens. Defaults to 5.
	CircuitThreshold int

	// CircuitCooldown is how long to wait before a half-open probe after
	// the circuit opens. Defaults to 30 seconds.
	CircuitCooldown time.Duration

	// MaxResponseSize caps response body size to protect against OOM from
	// malicious or buggy downstream services. Default: 10 MB.
	MaxResponseSize int64

	// RetryNonIdempotent enables retrying POST/PATCH on 5xx. Default
	// false — only idempotent methods (per RFC 9110) are retried. Set
	// true if your non-idempotent endpoints use idempotency keys.
	RetryNonIdempotent bool

	// Clock returns the current time. Defaults to time.Now. Tests inject
	// a fake clock to exercise circuit breaker cooldown deterministically.
	Clock func() time.Time
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
		cfg.MaxResponseSize = 10 << 20
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}

	return &ServiceClient{
		client: &http.Client{
			Timeout: cfg.Timeout,
		},
		baseURL:            strings.TrimRight(cfg.BaseURL, "/"),
		defaultTimeout:     cfg.Timeout,
		maxRetries:         cfg.MaxRetries,
		retryDelay:         cfg.RetryDelay,
		maxResponseSize:    cfg.MaxResponseSize,
		retryNonIdempotent: cfg.RetryNonIdempotent,
		circuit:            newCircuitBreaker(cfg.CircuitThreshold, cfg.CircuitCooldown, cfg.Clock),
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
func (sc *ServiceClient) GetJSON(ctx context.Context, path string, target any) error {
	resp, err := sc.Get(ctx, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		// Drain (limited) so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("service returned %d", resp.StatusCode)
	}

	return json.NewDecoder(resp.Body).Decode(target)
}

func (sc *ServiceClient) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("path must start with \"/\", got %q", path)
	}

	if !sc.circuit.allow() {
		return nil, fmt.Errorf("circuit breaker open for %s", sc.baseURL)
	}

	url := sc.baseURL + path

	var lastErr error
	for attempt := 0; attempt <= sc.maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff with jitter to avoid thundering herd.
			base := sc.retryDelay * time.Duration(1<<(attempt-1))
			delay := base/2 + time.Duration(rand.Int64N(int64(base/2+1)))
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
			bodyReader = bytes.NewReader(data)
		}

		req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}

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

		// Client errors (4xx) are not retried.
		if resp.StatusCode >= 500 {
			resp.Body.Close()
			lastErr = fmt.Errorf("server error: %d", resp.StatusCode)
			sc.circuit.recordFailure()
			if !isIdempotent(method) && !sc.retryNonIdempotent {
				break
			}
			continue
		}

		sc.circuit.recordSuccess()
		resp.Body = newLimitedReadCloser(resp.Body, sc.maxResponseSize)
		return resp, nil
	}

	return nil, fmt.Errorf("all %d attempts failed for %s %s: %w",
		sc.maxRetries+1, method, url, lastErr)
}

// circuitBreaker trips after a configurable number of consecutive failures.
// In the half-open state, exactly one probe is allowed through; subsequent
// requests are rejected until the probe completes. This is a deliberate
// trade-off against the thundering-herd problem — recovery latency is
// bounded by the per-request timeout, not the cooldown.
type circuitBreaker struct {
	mu          sync.Mutex
	failures    int
	threshold   int
	cooldown    time.Duration
	lastFailure time.Time
	state       string // "closed", "open", "half-open"
	now         func() time.Time
}

func newCircuitBreaker(threshold int, cooldown time.Duration, now func() time.Time) *circuitBreaker {
	if now == nil {
		now = time.Now
	}
	return &circuitBreaker{
		threshold: threshold,
		cooldown:  cooldown,
		state:     "closed",
		now:       now,
	}
}

func (cb *circuitBreaker) allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case "closed":
		return true
	case "open":
		if cb.now().Sub(cb.lastFailure) > cb.cooldown {
			cb.state = "half-open"
			return true
		}
		return false
	case "half-open":
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
	cb.lastFailure = cb.now()
	if cb.failures >= cb.threshold {
		cb.state = "open"
	}
}

// isIdempotent returns true if the HTTP method is idempotent per RFC 9110.
func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut,
		http.MethodDelete, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// limitedReadCloser wraps a response body with a size limit.
type limitedReadCloser struct {
	io.Reader
	closer io.Closer
}

func newLimitedReadCloser(body io.ReadCloser, limit int64) *limitedReadCloser {
	return &limitedReadCloser{
		Reader: io.LimitReader(body, limit),
		closer: body,
	}
}

func (l *limitedReadCloser) Close() error {
	return l.closer.Close()
}
