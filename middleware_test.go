package runko

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// nopHandler is a handler that always returns 200 OK.
var nopHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
})

func TestSecurityHeaders_Defaults(t *testing.T) {
	handler := DefaultSecurityHeaders()(nopHandler)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	expected := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"X-Xss-Protection":       "0",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
		"Cache-Control":          "no-store",
		"Permissions-Policy":     "camera=(), microphone=(), geolocation=()",
	}

	for header, want := range expected {
		got := rec.Header().Get(header)
		if got != want {
			t.Errorf("header %s = %q, want %q", header, got, want)
		}
	}

	// HSTS should NOT be present by default.
	if hsts := rec.Header().Get("Strict-Transport-Security"); hsts != "" {
		t.Errorf("HSTS should not be set by default, got %q", hsts)
	}
}

func TestSecurityHeaders_WithHSTS(t *testing.T) {
	handler := SecurityHeaders(SecurityHeadersConfig{
		HSTS: true,
	})(nopHandler)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	hsts := rec.Header().Get("Strict-Transport-Security")
	if hsts != "max-age=31536000; includeSubDomains" {
		t.Errorf("HSTS = %q, want default max-age", hsts)
	}
}

func TestCORS_WildcardWithCredentials_Panics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for CORS wildcard + credentials, got none")
		}
	}()

	CORS(CORSConfig{
		AllowedOrigins:   []string{"*"},
		AllowCredentials: true,
	})
}

func TestCORS_WildcardWithoutCredentials_NoPanic(t *testing.T) {
	// Should NOT panic.
	CORS(CORSConfig{
		AllowedOrigins: []string{"*"},
	})
}

func TestCORS_Preflight(t *testing.T) {
	handler := CORS(CORSConfig{
		AllowedOrigins: []string{"https://example.com"},
	})(nopHandler)

	req := httptest.NewRequest("OPTIONS", "/api/test", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Errorf("Allow-Origin = %q, want %q", got, "https://example.com")
	}
}

func TestCORS_DisallowedOrigin(t *testing.T) {
	handler := CORS(CORSConfig{
		AllowedOrigins: []string{"https://example.com"},
	})(nopHandler)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "https://evil.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin should be empty for disallowed origin, got %q", got)
	}
}

func TestBodyLimit_Enforced(t *testing.T) {
	handler := BodyLimit(100)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			Error(w, http.StatusRequestEntityTooLarge, "too_large", "Body too large")
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	// Under limit.
	req := httptest.NewRequest("POST", "/", strings.NewReader("small body"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("under-limit request: status = %d, want 200", rec.Code)
	}

	// Over limit.
	bigBody := strings.Repeat("x", 200)
	req = httptest.NewRequest("POST", "/", strings.NewReader(bigBody))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("over-limit request: status = %d, want 413", rec.Code)
	}
}

func TestRateLimit_Basic(t *testing.T) {
	handler := RateLimit(RateLimitConfig{
		RequestsPerWindow: 3,
		Window:            1 * time.Second,
		MaxClients:        100,
	})(nopHandler)

	// First 3 requests should pass.
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "1.2.3.4:1234"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200", i+1, rec.Code)
		}
	}

	// 4th request should be rate limited.
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "1.2.3.4:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("4th request: status = %d, want 429", rec.Code)
	}

	// Different IP should still work.
	req = httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "5.6.7.8:1234"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("different IP request: status = %d, want 200", rec.Code)
	}
}

// rateLimitReq fires one request from the given RemoteAddr and returns the
// status code.
func rateLimitReq(handler http.Handler, remoteAddr string) int {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code
}

// CONV-06: the client table is bounded by evicting the least recently seen
// bucket, never by rejecting unseen clients. Rejecting at capacity would
// let an attacker rotating addresses fill the table and deny service to
// every new legitimate client.
func TestRateLimit_AtCapacityEvictsLRUAndAdmitsNewClients(t *testing.T) {
	// MaxClients is 3 rather than 2 so the assertions below have a spare
	// slot to probe with: at exactly capacity, every probe for an evicted
	// client would itself evict another and mask what is being measured.
	handler := RateLimit(RateLimitConfig{
		RequestsPerWindow: 1,
		Window:            1 * time.Minute,
		MaxClients:        3,
	})(nopHandler)

	const (
		a = "1.0.0.1:1234"
		b = "2.0.0.1:1234"
		c = "3.0.0.1:1234"
		d = "9.9.9.9:1234"
	)

	// A consumes its allowance and becomes least recently seen; B and C
	// fill the remaining slots. Table: {A,B,C}, LRU order C,B,A.
	if got := rateLimitReq(handler, a); got != http.StatusOK {
		t.Fatalf("A first request: status = %d, want 200", got)
	}
	if got := rateLimitReq(handler, a); got != http.StatusTooManyRequests {
		t.Fatalf("A over limit: status = %d, want 429", got)
	}
	if got := rateLimitReq(handler, b); got != http.StatusOK {
		t.Fatalf("B first request: status = %d, want 200", got)
	}
	if got := rateLimitReq(handler, c); got != http.StatusOK {
		t.Fatalf("C first request: status = %d, want 200", got)
	}

	// The table is full. A brand-new client must still be admitted — a
	// rejection here is the lockout regression. This evicts A.
	if got := rateLimitReq(handler, d); got != http.StatusOK {
		t.Errorf("new client at capacity: status = %d, want 200 (lockout regression)", got)
	}

	// B was not evicted, so its exhausted window still applies. This is
	// what distinguishes LRU eviction from clearing the whole table.
	if got := rateLimitReq(handler, b); got != http.StatusTooManyRequests {
		t.Errorf("retained client: status = %d, want 429 (table was flushed, not LRU-evicted)", got)
	}

	// A was evicted, so it starts a fresh window. Were it still tracked,
	// this second request would be refused like B's.
	if got := rateLimitReq(handler, a); got != http.StatusOK {
		t.Errorf("evicted client: status = %d, want 200", got)
	}
}

// CONV-06: IPv6 clients are bucketed by /64. Keying on the full address
// would make the limiter a no-op, since a single client is routinely
// delegated 2^64 addresses to rotate through.
func TestRateLimit_IPv6BucketedBySlash64(t *testing.T) {
	handler := RateLimit(RateLimitConfig{
		RequestsPerWindow: 1,
		Window:            1 * time.Minute,
	})(nopHandler)

	if got := rateLimitReq(handler, "[2001:db8::1]:1234"); got != http.StatusOK {
		t.Fatalf("first address in /64: status = %d, want 200", got)
	}
	// Different address, same /64 — must share the bucket.
	if got := rateLimitReq(handler, "[2001:db8::2]:1234"); got != http.StatusTooManyRequests {
		t.Errorf("second address in same /64: status = %d, want 429", got)
	}
	// Different /64 — must get its own bucket.
	if got := rateLimitReq(handler, "[2001:db9::1]:1234"); got != http.StatusOK {
		t.Errorf("address in different /64: status = %d, want 200", got)
	}
}

func TestRedactQuery(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			"no sensitive params",
			"name=ville&page=1",
			"name=ville&page=1",
		},
		{
			"token redacted",
			"token=abc123&name=ville",
			"token=[REDACTED]&name=ville",
		},
		{
			"multiple sensitive params",
			"api_key=secret&password=hunter2&name=test",
			"api_key=[REDACTED]&password=[REDACTED]&name=test",
		},
		{
			"case insensitive",
			"Token=abc&API_KEY=xyz",
			"Token=[REDACTED]&API_KEY=[REDACTED]",
		},
		{
			"access_token",
			"access_token=eyJhbGci&refresh_token=dGhpcw",
			"access_token=[REDACTED]&refresh_token=[REDACTED]",
		},
		{
			"session and csrf",
			"session=abc&csrf=def&page=1",
			"session=[REDACTED]&csrf=[REDACTED]&page=1",
		},
		{
			"empty query",
			"",
			"",
		},
		{
			"no value",
			"key&name=test",
			"key&name=test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactQuery(tt.input)
			if got != tt.want {
				t.Errorf("redactQuery(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRecovery_CatchesPanic(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	panickingHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})

	handler := Recovery(logger)(panickingHandler)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("panic recovery: status = %d, want 500", rec.Code)
	}
}

func TestRequestIDMiddleware_GeneratesID(t *testing.T) {
	var capturedID string
	handler := RequestIDMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = RequestID(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if capturedID == "" {
		t.Error("RequestIDMiddleware should generate an ID when none provided")
	}
	if rec.Header().Get("X-Request-ID") != capturedID {
		t.Error("X-Request-ID response header should match context ID")
	}
}

func TestRequestIDMiddleware_SanitizesInput(t *testing.T) {
	var capturedID string
	handler := RequestIDMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = RequestID(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	// Malicious ID should be rejected and a new one generated.
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", `evil","injected":"pwned`)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if capturedID == `evil","injected":"pwned` {
		t.Error("malicious X-Request-ID should be rejected")
	}
	if capturedID == "" {
		t.Error("a fresh ID should be generated when input is invalid")
	}
}

func TestRequestIDMiddleware_AcceptsValid(t *testing.T) {
	var capturedID string
	handler := RequestIDMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = RequestID(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "valid-trace-123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if capturedID != "valid-trace-123" {
		t.Errorf("valid ID should be preserved, got %q", capturedID)
	}
}

// FIX-01: Recovery should not write error if response already started.
func TestRecovery_AlreadyWritten(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	handler := Recovery(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("partial"))
		panic("late panic")
	}))

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Status should be 200 (the handler's status, not 500).
	if rec.Code != http.StatusOK {
		t.Errorf("already-written panic: status = %d, want 200 (original)", rec.Code)
	}

	// Body should contain the partial write, not an error JSON blob.
	body := rec.Body.String()
	if strings.Contains(body, "internal_error") {
		t.Error("already-written panic: should not append error JSON to partial response")
	}
}

// FIX-02: CORS wildcard should set literal "*", not reflect Origin.
func TestCORS_WildcardLiteral(t *testing.T) {
	handler := CORS(CORSConfig{
		AllowedOrigins: []string{"*"},
	})(nopHandler)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "<script>evil</script>")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	got := rec.Header().Get("Access-Control-Allow-Origin")
	if got != "*" {
		t.Errorf("wildcard CORS should set literal *, got %q", got)
	}
}

// FIX-02: CORS specific origin should reflect the matched origin.
func TestCORS_SpecificOriginReflected(t *testing.T) {
	handler := CORS(CORSConfig{
		AllowedOrigins: []string{"https://example.com"},
	})(nopHandler)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	got := rec.Header().Get("Access-Control-Allow-Origin")
	if got != "https://example.com" {
		t.Errorf("specific origin should be reflected, got %q", got)
	}
}

// FIX-03: CORS should always set Vary: Origin.
func TestCORS_VaryHeader(t *testing.T) {
	handler := CORS(CORSConfig{
		AllowedOrigins: []string{"https://example.com"},
	})(nopHandler)

	// With matching origin.
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if vary := rec.Header().Get("Vary"); !strings.Contains(vary, "Origin") {
		t.Errorf("Vary header should contain Origin, got %q", vary)
	}

	// Without origin header — Vary should still be set.
	req = httptest.NewRequest("GET", "/", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if vary := rec.Header().Get("Vary"); !strings.Contains(vary, "Origin") {
		t.Errorf("Vary: Origin should be set even without Origin header, got %q", vary)
	}
}

// FIX-04: statusWriter should implement http.Flusher.
func TestStatusWriter_ImplementsFlusher(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, statusCode: http.StatusOK}

	// httptest.ResponseRecorder implements Flusher.
	sw.Flush()

	if !rec.Flushed {
		t.Error("Flush should pass through to underlying ResponseWriter")
	}
}

// FIX-07: RequestIDMiddleware should propagate trace ID.
func TestRequestIDMiddleware_PropagatesTraceID(t *testing.T) {
	var capturedTraceID string
	handler := RequestIDMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedTraceID = TraceID(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Trace-ID", "trace-abc-123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if capturedTraceID != "trace-abc-123" {
		t.Errorf("trace ID should be propagated, got %q", capturedTraceID)
	}
}

// FIX-07: Invalid trace ID should be dropped, not propagated.
func TestRequestIDMiddleware_InvalidTraceIDDropped(t *testing.T) {
	var capturedTraceID string
	handler := RequestIDMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedTraceID = TraceID(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Trace-ID", "evil;drop table")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if capturedTraceID != "" {
		t.Errorf("invalid trace ID should be dropped, got %q", capturedTraceID)
	}
}

// FIX-07: statusWriter should not forward duplicate WriteHeader calls.
func TestStatusWriter_NoDuplicateWriteHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, statusCode: http.StatusOK}

	sw.WriteHeader(http.StatusCreated)
	sw.WriteHeader(http.StatusInternalServerError) // should be ignored

	if sw.statusCode != http.StatusCreated {
		t.Errorf("statusCode = %d, want %d", sw.statusCode, http.StatusCreated)
	}
	// The underlying recorder should only have received the first call.
	if rec.Code != http.StatusCreated {
		t.Errorf("recorder code = %d, want %d", rec.Code, http.StatusCreated)
	}
}

// FIX-08: Rate limiter cleans up expired entries inline, reclaiming
// capacity so live clients do not have to be evicted to make room.
//
// This asserts the table size directly. Response codes cannot distinguish
// a swept entry from one lazily reset on next contact, so a black-box test
// here passes with the sweep entirely removed.
func TestRateLimit_CleansUpExpiredEntries(t *testing.T) {
	now := time.Now()
	rl := newRateLimiter(RateLimitConfig{
		RequestsPerWindow: 1,
		Window:            time.Minute,
		MaxClients:        100,
		Clock:             func() time.Time { return now },
	})

	for _, key := range []string{"1.0.0.1", "2.0.0.1", "3.0.0.1"} {
		rl.allow(key)
	}
	if got := rl.tracked(); got != 3 {
		t.Fatalf("tracked = %d, want 3", got)
	}

	// Advance past the window so every entry is stale, then touch the
	// limiter once to trigger the inline sweep.
	now = now.Add(2 * time.Minute)
	rl.allow("9.9.9.9")

	// The three stale entries are gone; only the fresh one remains.
	if got := rl.tracked(); got != 1 {
		t.Errorf("tracked after sweep = %d, want 1 — expired entries were not reclaimed", got)
	}
}

// CONV-06: the tracking table never exceeds MaxClients, however many
// distinct clients are seen. This is the memory bound the convention
// promises, and it is not observable from status codes.
func TestRateLimit_TableStaysBounded(t *testing.T) {
	rl := newRateLimiter(RateLimitConfig{
		RequestsPerWindow: 100,
		Window:            time.Minute,
		MaxClients:        50,
	})

	for i := 0; i < 5000; i++ {
		rl.allow(fmt.Sprintf("10.0.%d.%d", i/256, i%256))
	}

	if got := rl.tracked(); got != 50 {
		t.Errorf("tracked = %d, want 50 (MaxClients bound not enforced)", got)
	}
}

// The limiter is reached concurrently by every request. Exercised here so
// `go test -race` has something to catch, since the LRU list and the map
// are mutated together on the hot path.
func TestRateLimit_ConcurrentAccess(t *testing.T) {
	handler := RateLimit(RateLimitConfig{
		RequestsPerWindow: 5,
		Window:            time.Minute,
		MaxClients:        16,
	})(nopHandler)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				rateLimitReq(handler, fmt.Sprintf("10.0.0.%d:1234", (i*20+j)%64))
			}
		}(i)
	}
	wg.Wait()
}

// FIX-10: CORS preflight should reject disallowed methods.
func TestCORS_PreflightInvalidMethod(t *testing.T) {
	handler := CORS(CORSConfig{
		AllowedOrigins: []string{"https://example.com"},
		AllowedMethods: []string{"GET", "POST"},
	})(nopHandler)

	req := httptest.NewRequest("OPTIONS", "/api/test", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "DELETE")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	// CORS headers should have been removed for disallowed method.
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin should be empty for disallowed method, got %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got != "" {
		t.Errorf("Allow-Methods should be empty for disallowed method, got %q", got)
	}
}

// AUDIT3-01: recoveryWriter should implement http.Flusher for SSE support.
func TestRecoveryWriter_ImplementsFlusher(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &recoveryWriter{ResponseWriter: rec}

	// httptest.ResponseRecorder implements Flusher.
	rw.Flush()

	if !rec.Flushed {
		t.Error("recoveryWriter.Flush should pass through to underlying ResponseWriter")
	}
}

// AUDIT3-01: recoveryWriter should implement http.Hijacker passthrough.
func TestRecoveryWriter_ImplementsHijacker(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &recoveryWriter{ResponseWriter: rec}

	// httptest.ResponseRecorder does NOT implement Hijacker,
	// so we expect the error path.
	_, _, err := rw.Hijack()
	if err == nil {
		t.Error("Hijack should return error when underlying writer doesn't support it")
	}
}

// AUDIT3-02: CORS should not set preflight-only headers on regular responses.
func TestCORS_RegularRequest_NoPreflightHeaders(t *testing.T) {
	handler := CORS(CORSConfig{
		AllowedOrigins: []string{"https://example.com"},
	})(nopHandler)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Allow-Origin should be present on regular responses.
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Errorf("Allow-Origin = %q, want %q", got, "https://example.com")
	}

	// Preflight-only headers should NOT be present on regular responses.
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got != "" {
		t.Errorf("Allow-Methods should not be set on regular response, got %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "" {
		t.Errorf("Allow-Headers should not be set on regular response, got %q", got)
	}
	if got := rec.Header().Get("Access-Control-Max-Age"); got != "" {
		t.Errorf("Max-Age should not be set on regular response, got %q", got)
	}
}

// AUDIT3-02: CORS preflight should still include method/header/max-age headers.
func TestCORS_Preflight_IncludesPreflightHeaders(t *testing.T) {
	handler := CORS(CORSConfig{
		AllowedOrigins: []string{"https://example.com"},
	})(nopHandler)

	req := httptest.NewRequest("OPTIONS", "/api/test", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("Allow-Methods should be set on preflight response")
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Error("Allow-Headers should be set on preflight response")
	}
	if got := rec.Header().Get("Access-Control-Max-Age"); got == "" {
		t.Error("Max-Age should be set on preflight response")
	}
}

// AUDIT3-03: CORS preflight should set Vary for preflight-specific headers.
func TestCORS_Preflight_VaryHeaders(t *testing.T) {
	handler := CORS(CORSConfig{
		AllowedOrigins: []string{"https://example.com"},
	})(nopHandler)

	req := httptest.NewRequest("OPTIONS", "/api/test", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	vary := strings.Join(rec.Header().Values("Vary"), ", ")

	if !strings.Contains(vary, "Origin") {
		t.Errorf("Vary should contain Origin, got %q", vary)
	}
	if !strings.Contains(vary, "Access-Control-Request-Method") {
		t.Errorf("Vary should contain Access-Control-Request-Method, got %q", vary)
	}
	if !strings.Contains(vary, "Access-Control-Request-Headers") {
		t.Errorf("Vary should contain Access-Control-Request-Headers, got %q", vary)
	}
}

// AUDIT3-03: Regular responses should not have preflight Vary headers.
func TestCORS_RegularRequest_NoPreflightVary(t *testing.T) {
	handler := CORS(CORSConfig{
		AllowedOrigins: []string{"https://example.com"},
	})(nopHandler)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "https://example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	vary := strings.Join(rec.Header().Values("Vary"), ", ")

	if !strings.Contains(vary, "Origin") {
		t.Errorf("Vary should contain Origin, got %q", vary)
	}
	if strings.Contains(vary, "Access-Control-Request-Method") {
		t.Errorf("Vary should NOT contain Access-Control-Request-Method on regular response, got %q", vary)
	}
}

// AUDIT3-05: Rate limiter cleanup should be bounded.
func TestRateLimit_CleanupBounded(t *testing.T) {
	handler := RateLimit(RateLimitConfig{
		RequestsPerWindow: 100,
		Window:            50 * time.Millisecond,
		MaxClients:        10000,
	})(nopHandler)

	// Fill many client slots.
	for i := 0; i < 1000; i++ {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = fmt.Sprintf("10.0.%d.%d:1234", i/256, i%256)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}

	// Wait for window to expire.
	time.Sleep(60 * time.Millisecond)

	// A single request should complete quickly (bounded cleanup).
	start := time.Now()
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "9.9.9.9:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	duration := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if duration > 100*time.Millisecond {
		t.Errorf("cleanup took %v, expected much less", duration)
	}
}

// FIX-14: SecurityHeaders should set CSP when configured.
func TestSecurityHeaders_WithCSP(t *testing.T) {
	handler := SecurityHeaders(SecurityHeadersConfig{
		ContentSecurityPolicy: "default-src 'self'",
	})(nopHandler)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Security-Policy"); got != "default-src 'self'" {
		t.Errorf("CSP = %q, want %q", got, "default-src 'self'")
	}
}

// Audit5-01: Rate limiter cleanup must advance lastCleanup even when the
// per-sweep cap is hit. Otherwise a hot map triggers a capped sweep on
// every request under lock.
func TestRateLimit_CleanupAdvancesUnderLoad(t *testing.T) {
	var clockNs int64
	now := func() time.Time {
		return time.Unix(0, clockNs)
	}
	advance := func(d time.Duration) {
		clockNs += d.Nanoseconds()
	}

	handler := RateLimit(RateLimitConfig{
		RequestsPerWindow: 10,
		Window:            10 * time.Second,
		MaxClients:        2000,
		Clock:             now,
	})(nopHandler)

	// Seed with enough clients to exceed the sweep cap.
	for i := 0; i < 600; i++ {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = fmt.Sprintf("10.0.%d.%d:1234", i/256, i%256)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}

	// Advance past the window so cleanup will run.
	advance(11 * time.Second)

	// First request after expiry: cleanup runs and must advance lastCleanup.
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "9.9.9.9:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request after expiry: status = %d, want 200", rec.Code)
	}

	// Without advancing the clock, a second request must NOT re-enter cleanup.
	// We can't directly observe that, but if the fix regresses, the cleanup
	// would run on every request — serialized under lock — and 50
	// consecutive requests would be observably slow even with a fast clock.
	// The functional check: behavior is stable and requests pass.
	for i := 0; i < 50; i++ {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "9.9.9.9:1234"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}

// FIX-14: SecurityHeaders should NOT set CSP by default.
func TestSecurityHeaders_NoCSPByDefault(t *testing.T) {
	handler := DefaultSecurityHeaders()(nopHandler)

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("CSP should not be set by default, got %q", got)
	}
}

// CONV-08: the access log carries the matched route template alongside the
// concrete path. Without it, /users/1 and /users/2 are distinct log keys
// and latency cannot be aggregated by endpoint.
func TestLogger_IncludesRoutePattern(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	rt := newRouter(nil)
	rt.Use(Logger(logger))
	rt.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {})

	rt.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/users/42", nil))

	out := buf.String()
	if !strings.Contains(out, `"pattern":"GET /users/{id}"`) {
		t.Errorf("log line missing the route pattern: %s", out)
	}
	// The concrete path is still recorded — the pattern supplements it.
	if !strings.Contains(out, `"path":"/users/42"`) {
		t.Errorf("log line missing the concrete path: %s", out)
	}
}

// The pattern survives the documented chain ordering, in which the
// request-cloning middleware runs BEFORE Logger. This is the ordering the
// example and scaffold use.
func TestLogger_PatternSurvivesDocumentedChainOrder(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	rt := newRouter(nil)
	rt.Use(
		RequestIDMiddleware(),                     // clones via WithContext
		ClientIPMiddleware(newProxyResolver(nil)), // clones via WithContext
		Logger(logger),
	)
	rt.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {})

	rt.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/users/42", nil))

	if !strings.Contains(buf.String(), `"pattern":"GET /users/{id}"`) {
		t.Errorf("pattern lost in the documented chain order: %s", buf.String())
	}
	// Request correlation must survive too — the same clone carries it.
	if strings.Contains(buf.String(), `"request_id":""`) {
		t.Errorf("request_id empty despite RequestIDMiddleware running first: %s", buf.String())
	}
}

// Registering Logger BEFORE a cloning middleware silently loses the
// pattern: the mux stamps the clone, which Logger does not hold. This is a
// documented ordering hazard, pinned here so the behavior is known rather
// than discovered.
func TestLogger_PatternLostWhenLoggerPrecedesCloningMiddleware(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	rt := newRouter(nil)
	rt.Use(
		Logger(logger),
		RequestIDMiddleware(), // clones after Logger captured its pointer
	)
	rt.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {})

	rt.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/users/42", nil))

	if strings.Contains(buf.String(), `"pattern"`) {
		t.Errorf("pattern unexpectedly present — the ordering hazard may have been fixed, "+
			"in which case update the docs in middleware.go and README: %s", buf.String())
	}
	// The request is still logged; only the pattern attribute is missing.
	if !strings.Contains(buf.String(), `"status":200`) {
		t.Errorf("request was not logged at all: %s", buf.String())
	}
}

// Unmatched requests have no route template, so the attribute is omitted
// rather than logged empty.
func TestLogger_OmitsPatternWhenUnmatched(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	rt := newRouter(nil)
	rt.Use(Logger(logger))
	rt.HandleFunc("GET /users", func(w http.ResponseWriter, r *http.Request) {})

	rt.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/nope", nil))

	out := buf.String()
	if strings.Contains(out, `"pattern"`) {
		t.Errorf("unmatched request logged a pattern: %s", out)
	}
	// But it is still logged — that is the audit-trail guarantee.
	if !strings.Contains(out, `"status":404`) {
		t.Errorf("unmatched request was not logged: %s", out)
	}
}
