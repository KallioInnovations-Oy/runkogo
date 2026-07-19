package runko

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const validTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func TestSanitizeTraceparent(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"valid", validTraceparent, validTraceparent},
		{"valid, unsampled flags", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00"},

		{"empty", "", ""},
		{"too short", "00-4bf92f3577b34da6-00f067aa0ba902b7-01", ""},

		// Version 00's ABNF is exactly 55 chars, so trailing content is
		// malformed rather than an extension.
		{"version 00 with trailing content", validTraceparent + "0", ""},
		{"version 00 with dash-separated extra", validTraceparent + "-ff", ""},

		// Structure.
		{"missing dashes", strings.ReplaceAll(validTraceparent, "-", "0"), ""},
		{"dash in wrong place", "000-4bf92f3577b34da6a3ce929d0e0e473-00f067aa0ba902b7-01", ""},

		// Per spec: all-zero ids are invalid, and version ff is forbidden.
		{"all-zero trace id", "00-00000000000000000000000000000000-00f067aa0ba902b7-01", ""},
		{"all-zero parent id", "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01", ""},
		{"forbidden version ff", "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", ""},

		// W3C §3.2.4: the format is additive, and a higher version MUST be
		// parsed rather than dropped — otherwise the trace is severed the
		// moment any upstream adopts version 01.
		{
			"future version is accepted",
			"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		},
		{
			"future version with extra dash-separated fields",
			"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-cafe",
			"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-cafe",
		},
		{
			"future version with unseparated trailing content",
			"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01x",
			"",
		},
		{
			// Extension fields are forwarded verbatim, so they cannot be
			// allowed to smuggle a header break.
			"future version with CRLF in the extension",
			"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-a\r\nX: 1",
			"",
		},

		// Hex discipline. Uppercase is invalid per spec, and accepting it
		// would let two spellings of one trace id diverge downstream.
		{"uppercase hex", "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01", ""},
		{"non-hex characters", "00-4bf92f3577b34da6a3ce929d0e0e473g-00f067aa0ba902b7-01", ""},

		// Exactly 55 chars with an embedded CRLF, so the length check
		// cannot be what rejects it — the hex validation must.
		{"CRLF inside a full-length value", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba90\r\n7-01", ""},
		{"whitespace padded", " " + validTraceparent[:54], ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeTraceparent(tc.input); got != tc.want {
				t.Errorf("sanitizeTraceparent(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// CONV-12: an incoming traceparent is carried through the request context
// so the outgoing client can forward it. Without this a RunkoGO service
// between two instrumented services severs the trace.
func TestRequestIDMiddleware_CapturesTraceparent(t *testing.T) {
	var got string
	h := RequestIDMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = Traceparent(r.Context())
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("traceparent", validTraceparent)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got != validTraceparent {
		t.Errorf("Traceparent = %q, want %q", got, validTraceparent)
	}
}

func TestRequestIDMiddleware_DropsMalformedTraceparent(t *testing.T) {
	var got string
	h := RequestIDMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = Traceparent(r.Context())
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("traceparent", "garbage")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got != "" {
		t.Errorf("Traceparent = %q, want \"\" for a malformed header", got)
	}
}

// The value must be forwarded byte-for-byte. RunkoGO creates no spans, so
// mutating it would corrupt a trace it is not equipped to participate in.
func TestServiceClient_ForwardsTraceparentUnchanged(t *testing.T) {
	received := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("traceparent")
		JSON(w, http.StatusOK, Map{"ok": true})
	}))
	defer srv.Close()

	sc := NewServiceClient(ServiceClientConfig{BaseURL: srv.URL, Timeout: 2 * time.Second})

	ctx := WithTraceparent(context.Background(), validTraceparent)
	resp, err := sc.Get(ctx, "/x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()

	if got := <-received; got != validTraceparent {
		t.Errorf("forwarded traceparent = %q, want %q", got, validTraceparent)
	}
}

func TestServiceClient_OmitsTraceparentWhenAbsent(t *testing.T) {
	received := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("traceparent")
		JSON(w, http.StatusOK, Map{"ok": true})
	}))
	defer srv.Close()

	sc := NewServiceClient(ServiceClientConfig{BaseURL: srv.URL, Timeout: 2 * time.Second})

	resp, err := sc.Get(context.Background(), "/x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()

	if got := <-received; got != "" {
		t.Errorf("forwarded traceparent = %q, want it absent", got)
	}
}

// End to end: an inbound traceparent survives the middleware chain and
// reaches the downstream service unchanged.
func TestTraceparentPropagatesEndToEnd(t *testing.T) {
	downstream := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downstream <- r.Header.Get("traceparent")
		JSON(w, http.StatusOK, Map{"ok": true})
	}))
	defer backend.Close()

	sc := NewServiceClient(ServiceClientConfig{BaseURL: backend.URL, Timeout: 2 * time.Second})

	rt := newRouter(nil)
	rt.Use(RequestIDMiddleware())
	rt.HandleFunc("GET /call", func(w http.ResponseWriter, r *http.Request) {
		resp, err := sc.Get(r.Context(), "/downstream")
		if err != nil {
			t.Errorf("downstream call: %v", err)
			return
		}
		resp.Body.Close()
		JSON(w, http.StatusOK, Map{"ok": true})
	})

	req := httptest.NewRequest("GET", "/call", nil)
	req.Header.Set("traceparent", validTraceparent)
	rt.ServeHTTP(httptest.NewRecorder(), req)

	if got := <-downstream; got != validTraceparent {
		t.Errorf("downstream saw traceparent %q, want %q — the trace was severed", got, validTraceparent)
	}
}

// A request bearing two traceparent headers is ambiguous. Taking the first
// would let a client prepend its own ahead of the real one, the same
// spoofing shape X-Forwarded-For is hardened against.
func TestTraceparentFromHeader_RejectsAmbiguousDuplicates(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Add("traceparent", "00-11111111111111111111111111111111-1111111111111111-01")
	req.Header.Add("traceparent", validTraceparent)

	if got := TraceparentFromHeader(req); got != "" {
		t.Errorf("TraceparentFromHeader with duplicates = %q, want \"\"", got)
	}
}

func TestSanitizeTracestate(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"valid", "vendor=abc123,other=xyz", "vendor=abc123,other=xyz"},
		{"empty", "", ""},
		{"CR/LF injection", "vendor=abc\r\nX-Evil: 1", ""},
		{"NUL byte", "vendor=a\x00b", ""},
		{"non-ASCII", "vendor=café", ""},
		{"over 512 chars", strings.Repeat("a", 513), ""},
		{"exactly 512 chars", strings.Repeat("a", 512), strings.Repeat("a", 512)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeTracestate(tc.input); got != tc.want {
				t.Errorf("sanitizeTracestate(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// W3C §3.5 requires a receiver to pass tracestate on; §3.4 forbids
// modifying it when traceparent itself is unchanged. Dropping it discards
// another vendor's trace data.
func TestTracestatePropagatesWithTraceparent(t *testing.T) {
	const ts = "vendor=abc123,other=xyz"

	type seen struct{ tp, ts string }
	received := make(chan seen, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- seen{r.Header.Get("traceparent"), r.Header.Get("tracestate")}
		JSON(w, http.StatusOK, Map{"ok": true})
	}))
	defer backend.Close()

	sc := NewServiceClient(ServiceClientConfig{BaseURL: backend.URL, Timeout: 2 * time.Second})

	rt := newRouter(nil)
	rt.Use(RequestIDMiddleware())
	rt.HandleFunc("GET /call", func(w http.ResponseWriter, r *http.Request) {
		resp, err := sc.Get(r.Context(), "/downstream")
		if err != nil {
			t.Errorf("downstream call: %v", err)
			return
		}
		resp.Body.Close()
	})

	req := httptest.NewRequest("GET", "/call", nil)
	req.Header.Set("traceparent", validTraceparent)
	req.Header.Set("tracestate", ts)
	rt.ServeHTTP(httptest.NewRecorder(), req)

	got := <-received
	if got.tp != validTraceparent {
		t.Errorf("downstream traceparent = %q, want %q", got.tp, validTraceparent)
	}
	if got.ts != ts {
		t.Errorf("downstream tracestate = %q, want %q — vendor trace data was dropped", got.ts, ts)
	}
}

// tracestate without a traceparent is orphaned vendor data with no trace
// to attach to, so it is not carried.
func TestTracestateIgnoredWithoutTraceparent(t *testing.T) {
	var got string
	h := RequestIDMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = Tracestate(r.Context())
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("tracestate", "vendor=abc123")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got != "" {
		t.Errorf("Tracestate = %q, want \"\" without a traceparent", got)
	}
}

// A group prefix ending in "/" produces an unclean pattern that ServeMux
// accepts for method-less registrations but can never match. Mount must
// fail at startup rather than leave a permanently unreachable handler.
func TestMountRejectsUncleanCombinedPattern(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Mount on a group prefix ending in \"/\" did not panic — " +
				"the handler would be silently unreachable")
		}
	}()

	rt := newRouter(nil)
	rt.Group("/api/").Mount("/v1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
}
