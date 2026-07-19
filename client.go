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

	// MaxRetries is how many times to retry after the first attempt.
	// Defaults to 2; set a negative value for no retries at all.
	//
	// Zero means "use the default" rather than "no retries", matching
	// Options elsewhere in the package. Without the negative escape hatch
	// there would be no way to express "attempt once", which is what you
	// want in front of an endpoint that is not safe to replay.
	//
	// It is a ceiling, not a guarantee: the circuit breaker is re-checked
	// before each retry and can end the loop early (CONV-10).
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
	} else if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
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

	// allow() granted this call a slot — in the half-open state, the single
	// probe slot. Every exit path must settle that slot by recording an
	// outcome, or release it here. Without this, one cancelled context
	// during a retry backoff would leave the breaker half-open forever and
	// refuse every subsequent call for the life of the process.
	settled := false
	defer func() {
		if !settled {
			sc.circuit.recordAbort()
		}
	}()

	url := sc.baseURL + path

	var lastErr error
	attempts := 0
	for attempt := 0; attempt <= sc.maxRetries; attempt++ {
		if attempt > 0 {
			// Re-check the breaker before each retry. This call's own
			// failures may have opened it, and continuing to retry against
			// a service the breaker has decided to shed defeats the point
			// of having one.
			if !sc.circuit.allow() {
				break
			}
			// This allow() granted a fresh slot, which may again be the
			// half-open probe. The previous attempt's outcome does not
			// settle it, so clear the flag or an exit during the backoff
			// below would leak the new slot and wedge the breaker.
			settled = false

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
		// Forward the W3C traceparent byte-for-byte. RunkoGO does not
		// create spans, so it must not synthesize or mutate this value —
		// passing it through unchanged is what keeps a trace intact across
		// a hop it does not itself instrument. See CONV-12.
		if tp := Traceparent(ctx); tp != "" {
			req.Header.Set("traceparent", tp)
			// §3.5 requires a receiver to pass tracestate on, and §3.4
			// forbids modifying it when traceparent itself is unchanged.
			// Dropping it would discard another vendor's trace data.
			if ts := Tracestate(ctx); ts != "" {
				req.Header.Set("tracestate", ts)
			}
		}

		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		attempts++

		resp, err := sc.client.Do(req)
		if err != nil {
			lastErr = err
			sc.circuit.recordFailure()
			settled = true
			// A transport error is not proof the server did not process
			// the request — a connection reset while awaiting the response
			// may follow a fully applied write. Retrying here would
			// duplicate charges and orders, so the same idempotency rule
			// that guards the 5xx branch applies.
			if !isIdempotent(method) && !sc.retryNonIdempotent {
				break
			}
			continue
		}

		// Client errors (4xx) are not retried.
		if resp.StatusCode >= 500 {
			resp.Body.Close()
			lastErr = fmt.Errorf("server error: %d", resp.StatusCode)
			sc.circuit.recordFailure()
			settled = true
			if !isIdempotent(method) && !sc.retryNonIdempotent {
				break
			}
			continue
		}

		sc.circuit.recordSuccess()
		settled = true
		resp.Body = newLimitedReadCloser(resp.Body, sc.maxResponseSize)
		return resp, nil
	}

	// Report attempts actually made, not the configured ceiling. The
	// breaker or the idempotency guard can end the loop early, and a
	// message claiming three attempts when one was made sends whoever is
	// debugging it looking in the wrong place.
	return nil, fmt.Errorf("%s %s failed after %d attempt(s): %w",
		method, url, attempts, lastErr)
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

// currentState reports the breaker's state. Used by tests to distinguish a
// released probe slot from a wedged one, which status codes cannot show.
func (cb *circuitBreaker) currentState() string {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

func (cb *circuitBreaker) recordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures = 0
	cb.state = "closed"
}

// recordAbort releases a slot granted by allow() when the call ended for a
// reason that says nothing about upstream health — a cancelled context, a
// body that would not marshal, an unbuildable request. It must not count as
// a failure, but it must not leave the half-open probe slot consumed
// either: nothing else can move the breaker out of half-open, so a leaked
// slot wedges it permanently.
func (cb *circuitBreaker) recordAbort() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.state == "half-open" {
		// Back to open, and restart the cooldown. Without refreshing
		// lastFailure the next allow() would grant a probe immediately,
		// so a caller cancelling in a loop could issue unlimited probes
		// against a service the breaker is supposed to be shielding.
		cb.state = "open"
		cb.lastFailure = cb.now()
	}
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
//
// CONV-10: retries are safe by default. Non-idempotent methods are not
// replayed on 5xx responses or on transport errors unless the caller opts
// in via RetryNonIdempotent.
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
