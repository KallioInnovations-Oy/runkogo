package runko

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// markerMiddleware records that it ran and stamps a header so tests can
// assert the chain executed for a given request.
func markerMiddleware(hits *int) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*hits++
			w.Header().Set("X-Marker", "ran")
			next.ServeHTTP(w, r)
		})
	}
}

func newTestRouter(hits *int) *Router {
	rt := newRouter(nil)
	rt.Use(markerMiddleware(hits))
	rt.HandleFunc("GET /users", func(w http.ResponseWriter, r *http.Request) {
		JSON(w, http.StatusOK, Map{"ok": true})
	})
	return rt
}

// CONV-04 / CONV-08: security headers and the audit trail must apply to
// every response, including those that match no route. Before the
// ServeHTTP fix, unmatched requests went straight to ServeMux and skipped
// the middleware chain entirely.
func TestUnmatchedRequestsRunMiddlewareChain(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		target     string
		wantStatus int
		wantCode   string
	}{
		{"no route matches", "GET", "/nope", http.StatusNotFound, "not_found"},
		{"path matches, method does not", "POST", "/users", http.StatusMethodNotAllowed, "method_not_allowed"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hits := 0
			rt := newTestRouter(&hits)

			rec := httptest.NewRecorder()
			rt.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, nil))

			if hits != 1 {
				t.Errorf("middleware ran %d times, want 1", hits)
			}
			if rec.Header().Get("X-Marker") != "ran" {
				t.Error("middleware did not touch the response")
			}
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}

			if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Errorf("Content-Type = %q, want application/json; charset=utf-8", ct)
			}

			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not the runko error shape: %v (body=%q)", err, rec.Body.String())
			}
			if body.Error.Code != tc.wantCode {
				t.Errorf("error.code = %q, want %q", body.Error.Code, tc.wantCode)
			}
		})
	}
}

// A 405 must advertise the methods that are allowed, as the stdlib does.
func TestMethodNotAllowedSetsAllowHeader(t *testing.T) {
	hits := 0
	rt := newTestRouter(&hits)

	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("POST", "/users", nil))

	if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
		t.Errorf("Allow = %q, want %q", allow, "GET, HEAD")
	}
}

// Matched routes must run their chain exactly once — the ServeHTTP
// fallback must not double-apply middleware to routes that already
// froze a chain at registration time.
func TestMatchedRouteRunsMiddlewareExactlyOnce(t *testing.T) {
	hits := 0
	rt := newTestRouter(&hits)

	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/users", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if hits != 1 {
		t.Errorf("middleware ran %d times, want exactly 1", hits)
	}
}

// Path-cleaning and trailing-slash redirects are the mux's job and must
// survive the fallback dispatch — and, per CONV-04/CONV-08, must still run
// the middleware chain. The mux synthesizes these redirect handlers
// itself, so they are never wrapped at registration time.
func TestRedirectsStillWork(t *testing.T) {
	hits := 0
	rt := newRouter(nil)
	rt.Use(markerMiddleware(&hits))
	rt.HandleFunc("GET /tree/", func(w http.ResponseWriter, r *http.Request) {
		JSON(w, http.StatusOK, Map{"ok": true})
	})

	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/tree", nil))

	// The exact status is the standard library's business and it has
	// changed across Go versions — 1.23 issues 301 here, 1.26 issues 307.
	// Asserting a specific code pins the test to one toolchain; what this
	// test is actually about is that the redirect still runs the chain.
	if rec.Code < 300 || rec.Code > 399 {
		t.Errorf("status = %d, want a 3xx redirect", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/tree/" {
		t.Errorf("Location = %q, want /tree/", loc)
	}
	if hits != 1 {
		t.Errorf("middleware ran %d times on a redirect, want 1", hits)
	}
	if rec.Header().Get("X-Marker") != "ran" {
		t.Error("redirect bypassed the middleware chain")
	}
}

// The route is resolved exactly once, from inside the middleware chain.
//
// This matters for path-rewriting middleware. Resolving before the chain
// and again inside the fallback would run a real handler against a
// throwaway probe — the client would receive a 404 while the handler's
// side effects had already happened and its response was discarded. With a
// single lookup, a rewrite simply routes, and the handler's own response
// is what the client gets.
func TestRouteResolvedOnceAfterMiddleware(t *testing.T) {
	rt := newRouter(nil)

	executions := 0
	rt.HandleFunc("POST /charge", func(w http.ResponseWriter, r *http.Request) {
		executions++
		JSON(w, http.StatusOK, Map{"charged": true})
	})

	// Middleware that rewrites an otherwise-unmatched path onto a route.
	rt.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/nope" {
				r2 := r.Clone(r.Context())
				r2.URL.Path = "/charge"
				next.ServeHTTP(w, r2)
				return
			}
			next.ServeHTTP(w, r)
		})
	})

	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("POST", "/nope", nil))

	if executions != 1 {
		t.Errorf("handler executed %d times, want 1", executions)
	}
	// The handler's response must reach the client, not be swallowed and
	// replaced by a fallback.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — handler ran but its response was discarded", rec.Code)
	}
	if got := rec.Body.String(); got != `{"charged":true}`+"\n" {
		t.Errorf("body = %q, want the handler's own response", got)
	}
}

// Group middleware must run once per matched route, and must not leak onto
// routes outside the group. Root middleware wraps the mux, group
// middleware wraps the route — the two must not double up.
func TestGroupMiddlewareAppliesOnceAndOnlyToGroup(t *testing.T) {
	rootHits, groupHits := 0, 0
	rt := newRouter(nil)
	rt.Use(markerMiddleware(&rootHits))

	api := rt.Group("/api", markerMiddleware(&groupHits))
	api.HandleFunc("GET /users", func(w http.ResponseWriter, r *http.Request) {})
	rt.HandleFunc("GET /public", func(w http.ResponseWriter, r *http.Request) {})

	rt.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/users", nil))
	if rootHits != 1 || groupHits != 1 {
		t.Errorf("grouped route: root=%d group=%d, want 1 and 1", rootHits, groupHits)
	}

	rootHits, groupHits = 0, 0
	rt.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/public", nil))
	if rootHits != 1 || groupHits != 0 {
		t.Errorf("ungrouped route: root=%d group=%d, want 1 and 0", rootHits, groupHits)
	}
}

func TestCustomNotFoundAndMethodNotAllowed(t *testing.T) {
	hits := 0
	rt := newTestRouter(&hits)
	rt.NotFound(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		JSON(w, http.StatusNotFound, Map{"custom": "404"})
	}))
	rt.MethodNotAllowed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		JSON(w, http.StatusMethodNotAllowed, Map{"custom": "405"})
	}))

	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/nope", nil))
	if got := rec.Body.String(); got != `{"custom":"404"}`+"\n" {
		t.Errorf("custom 404 body = %q", got)
	}
	if hits != 1 {
		t.Errorf("custom 404 skipped middleware: hits = %d", hits)
	}

	rec = httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("POST", "/users", nil))
	if got := rec.Body.String(); got != `{"custom":"405"}`+"\n" {
		t.Errorf("custom 405 body = %q", got)
	}
}

// Fallback handlers set on a group must reach the root, since the root is
// what actually dispatches unmatched requests.
func TestGroupFallbackRegistersOnRoot(t *testing.T) {
	hits := 0
	rt := newTestRouter(&hits)
	api := rt.Group("/api")
	api.NotFound(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		JSON(w, http.StatusNotFound, Map{"from": "group"})
	}))

	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/nope", nil))

	if got := rec.Body.String(); got != `{"from":"group"}`+"\n" {
		t.Errorf("group-registered 404 not used by root: body = %q", got)
	}
}

// Unmatched requests must pick up middleware registered after the routes
// were, because the fallback chain is built at dispatch time.
func TestFallbackUsesMiddlewareRegisteredAfterRoutes(t *testing.T) {
	hits := 0
	rt := newRouter(nil)
	rt.HandleFunc("GET /users", func(w http.ResponseWriter, r *http.Request) {})
	rt.Use(markerMiddleware(&hits)) // registered after Handle

	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/nope", nil))

	if hits != 1 {
		t.Errorf("late-registered middleware did not apply to 404: hits = %d", hits)
	}
}

// PathParam must work through the dispatch path. The stdlib's
// ServeMux.Handler deliberately does NOT populate path wildcards, so
// resolving a handler and calling it directly silently yields "" for every
// route parameter. Routing must go through mux.ServeHTTP for wildcards to
// be set.
func TestPathParamPopulated(t *testing.T) {
	rt := newRouter(nil)
	hits := 0
	rt.Use(markerMiddleware(&hits))

	var gotID string
	rt.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		gotID = PathParam(r, "id")
	})

	rt.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/users/42", nil))

	if gotID != "42" {
		t.Errorf("PathParam(id) = %q, want %q", gotID, "42")
	}
}

func TestPathParamInNestedGroup(t *testing.T) {
	rt := newRouter(nil)

	var gotOrg, gotID string
	v1 := rt.Group("/api").Group("/v1")
	v1.HandleFunc("GET /orgs/{org}/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		gotOrg = PathParam(r, "org")
		gotID = PathParam(r, "id")
	})

	rt.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("GET", "/api/v1/orgs/acme/users/7", nil))

	if gotOrg != "acme" || gotID != "7" {
		t.Errorf("PathParam org=%q id=%q, want acme and 7", gotOrg, gotID)
	}
}

func TestPathParamTrailingWildcard(t *testing.T) {
	rt := newRouter(nil)

	var gotPath string
	rt.HandleFunc("GET /files/{path...}", func(w http.ResponseWriter, r *http.Request) {
		gotPath = PathParam(r, "path")
	})

	rt.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("GET", "/files/a/b/c.txt", nil))

	if gotPath != "a/b/c.txt" {
		t.Errorf("PathParam(path) = %q, want %q", gotPath, "a/b/c.txt")
	}
}

// Mount attaches a sub-handler for every method across a whole subtree,
// which is how a metrics endpoint or pprof gets attached without the
// framework shipping either.
func TestMountServesPrefixAndSubtree(t *testing.T) {
	hits := 0
	rt := newRouter(nil)
	rt.Use(markerMiddleware(&hits))

	var seenPaths []string
	rt.Mount("/debug/pprof", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPaths = append(seenPaths, r.URL.Path)
		JSON(w, http.StatusOK, Map{"ok": true})
	}))

	for _, target := range []string{"/debug/pprof", "/debug/pprof/", "/debug/pprof/heap"} {
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", target, rec.Code)
		}
	}

	// The path must reach the handler unrewritten — net/http/pprof
	// dispatches on the absolute path.
	want := []string{"/debug/pprof", "/debug/pprof/", "/debug/pprof/heap"}
	if len(seenPaths) != len(want) {
		t.Fatalf("handler saw %v, want %v", seenPaths, want)
	}
	for i := range want {
		if seenPaths[i] != want[i] {
			t.Errorf("handler saw path %q, want %q (Mount must not rewrite the path)", seenPaths[i], want[i])
		}
	}
}

// Mounted handlers are routes like any other, so the root chain applies.
func TestMountRunsRootMiddleware(t *testing.T) {
	hits := 0
	rt := newRouter(nil)
	rt.Use(markerMiddleware(&hits))
	rt.Mount("/metrics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	if hits != 1 {
		t.Errorf("root middleware ran %d times on a mounted handler, want 1", hits)
	}
	if rec.Header().Get("X-Marker") != "ran" {
		t.Error("mounted handler bypassed the root middleware chain")
	}
}

// Mounting on a Group puts the handler behind that group's middleware —
// the documented way to protect pprof, which exposes heap and goroutine
// state to anyone who can reach it.
func TestMountOnGroupInheritsGroupMiddleware(t *testing.T) {
	blocked := false
	denyAll := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			blocked = true
			Error(w, http.StatusForbidden, "forbidden", "Nope")
		})
	}

	rt := newRouter(nil)
	admin := rt.Group("/admin", denyAll)
	admin.Mount("/pprof", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("mounted handler ran despite the group middleware denying the request")
	}))

	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/pprof/heap", nil))

	if !blocked {
		t.Error("group middleware did not run for the mounted handler")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestMountRejectsRootAndInvalidPrefixes(t *testing.T) {
	for _, prefix := range []string{"", "/", "//", "metrics"} {
		t.Run("prefix="+prefix, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("Mount(%q) did not panic", prefix)
				}
			}()
			newRouter(nil).Mount(prefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		})
	}
}

// StripPrefix is the documented escape hatch for sub-apps that expect
// paths relative to their mount point.
func TestMountWithStripPrefix(t *testing.T) {
	rt := newRouter(nil)

	var seen string
	sub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
	})
	rt.Mount("/admin", http.StripPrefix("/admin", sub))

	rt.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/admin/users", nil))

	if seen != "/users" {
		t.Errorf("sub-app saw %q, want %q", seen, "/users")
	}
}

// A method-less subtree pattern and a method-bearing catch-all are mutually
// ambiguous to ServeMux, so Mount cannot coexist with "GET /". This is a
// startup panic rather than a silent shadowing, which is the right failure
// mode — but it is surprising enough to pin.
func TestMountConflictsWithCatchAllRoute(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Mount alongside a catch-all \"GET /\" did not panic")
		}
	}()

	rt := newRouter(nil)
	rt.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {})
	rt.Mount("/debug/vars", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
}

// The documented remedy: "{$}" matches only the root path, so it neither
// collides with a Mount nor swallows unmatched GETs.
func TestMountCoexistsWithRootOnlyRoute(t *testing.T) {
	rt := newRouter(nil)
	rt.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		JSON(w, http.StatusOK, Map{"root": true})
	})
	rt.Mount("/debug/vars", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		JSON(w, http.StatusOK, Map{"vars": true})
	}))

	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "root") {
		t.Errorf("root route: status=%d body=%q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/debug/vars", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "vars") {
		t.Errorf("mounted route: status=%d body=%q", rec.Code, rec.Body.String())
	}

	// And the root-only route no longer swallows unmatched paths.
	rec = httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/typo", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unmatched path: status=%d, want 404 — the catch-all is still swallowing", rec.Code)
	}
}
