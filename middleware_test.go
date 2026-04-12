package runko

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
		"X-Frame-Options":       "DENY",
		"X-Xss-Protection":      "0",
		"Referrer-Policy":       "strict-origin-when-cross-origin",
		"Cache-Control":         "no-store",
		"Permissions-Policy":    "camera=(), microphone=(), geolocation=()",
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

func TestRateLimit_MaxClients(t *testing.T) {
	handler := RateLimit(RateLimitConfig{
		RequestsPerWindow: 100,
		Window:            1 * time.Minute,
		MaxClients:        2,
	})(nopHandler)

	// Fill up 2 client slots.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = strings.Replace("X.0.0.1:1234", "X", string(rune('1'+i)), 1)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("client %d: status = %d, want 200", i+1, rec.Code)
		}
	}

	// 3rd unique client should be rejected (map full).
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "9.9.9.9:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("overflow client: status = %d, want 429", rec.Code)
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

// FIX-08: Rate limiter should clean up expired entries inline.
func TestRateLimit_CleansUpExpiredEntries(t *testing.T) {
	handler := RateLimit(RateLimitConfig{
		RequestsPerWindow: 100,
		Window:            50 * time.Millisecond,
		MaxClients:        2,
	})(nopHandler)

	// Fill up 2 client slots.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = strings.Replace("X.0.0.1:1234", "X", string(rune('1'+i)), 1)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("client %d: status = %d, want 200", i+1, rec.Code)
		}
	}

	// 3rd client should be rejected (map full).
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "9.9.9.9:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("overflow before cleanup: status = %d, want 429", rec.Code)
	}

	// Wait for the window to expire.
	time.Sleep(60 * time.Millisecond)

	// Now the inline cleanup should free slots, allowing the 3rd client.
	req = httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "9.9.9.9:1234"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("after cleanup: status = %d, want 200", rec.Code)
	}
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
