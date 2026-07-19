package runko

import (
	"log/slog"
	"net/http"
	"path"
	"strings"
)

// Middleware is a function that wraps an http.Handler.
// Middleware is composable: each one wraps the next in the chain.
//
// Example:
//
//	func myMiddleware(next http.Handler) http.Handler {
//	    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//	        // before
//	        next.ServeHTTP(w, r)
//	        // after
//	    })
//	}
type Middleware func(http.Handler) http.Handler

// Router wraps http.ServeMux with middleware chains and route grouping.
// It uses Go 1.22's enhanced ServeMux which supports method matching
// and path parameters natively (e.g., "GET /users/{id}").
type Router struct {
	mux        *http.ServeMux
	middleware []Middleware
	prefix     string
	logger     *slog.Logger

	// root points at the router returned by newRouter. Groups share the
	// root's mux and fallback handlers; nil on the root itself.
	root *Router

	// registered counts routes and middleware added to this router, so App
	// can warn when a custom Options.Handler leaves them dead.
	registered int

	// notFound and methodNotAllowed handle requests that match no route.
	// Stored on the root only — see rootRouter. When nil, the framework's
	// JSON defaults are used.
	notFound         http.Handler
	methodNotAllowed http.Handler
}

func newRouter(logger *slog.Logger) *Router {
	return &Router{
		mux:    http.NewServeMux(),
		logger: logger,
	}
}

// registrations reports how many routes and middleware have been added,
// across this router and any groups derived from it.
func (rt *Router) registrations() int {
	return rt.rootRouter().registered
}

// rootRouter returns the router that owns the mux and fallback handlers.
func (rt *Router) rootRouter() *Router {
	if rt.root != nil {
		return rt.root
	}
	return rt
}

// Use appends middleware to the router's chain. Middleware runs in the
// order added: Use(A); Use(B) means A runs first, calls B, which calls
// the handler.
//
// On the root router, middleware wraps the mux itself and is resolved at
// request time, so ordering relative to Handle does not matter and every
// request is covered — matched routes, redirects, 404s and 405s alike.
//
// On a Group, middleware is frozen onto each route as it is registered.
// Call Use before Handle on a group, or the routes already registered
// will not carry it.
//
// Use is not safe for concurrent calls once the server is serving.
func (rt *Router) Use(mw ...Middleware) {
	rt.middleware = append(rt.middleware, mw...)
	rt.rootRouter().registered += len(mw)
}

// Handle registers a handler for the given pattern. The pattern uses
// Go 1.22 syntax: "METHOD /path" or "METHOD /path/{param}".
//
// Examples:
//
//	r.Handle("GET /users", listHandler)
//	r.Handle("POST /users", createHandler)
//	r.Handle("GET /users/{id}", getHandler)
//	r.Handle("DELETE /users/{id}", deleteHandler)
func (rt *Router) Handle(pattern string, handler http.Handler) {
	fullPattern := rt.prefixPattern(pattern)

	// Only group middleware is frozen onto the route here. Root middleware
	// wraps the mux itself in ServeHTTP, so it covers every request —
	// including redirects, 404s and 405s, which never reach a route.
	wrapped := handler
	if rt.root != nil {
		wrapped = rt.chain(handler)
	}
	rt.mux.Handle(fullPattern, wrapped)
	rt.rootRouter().registered++

	if rt.logger != nil {
		rt.logger.Debug("route registered", "pattern", fullPattern)
	}
}

// HandleFunc is a convenience wrapper around Handle.
func (rt *Router) HandleFunc(pattern string, fn http.HandlerFunc) {
	rt.Handle(pattern, fn)
}

// Group creates a sub-router with a path prefix and optional additional
// middleware. The group inherits the parent's middleware chain and adds
// its own on top.
//
// Example:
//
//	api := r.Group("/api/v1", authMiddleware, rateLimitMiddleware)
//	api.Handle("GET /users", listUsers)    // matches GET /api/v1/users
//	api.Handle("GET /users/{id}", getUser) // matches GET /api/v1/users/{id}
//
// Group middleware is attached to routes, not to the prefix. A request
// under the prefix that matches no route is a 404 handled by the root
// chain, so group middleware does not run for it — an unauthenticated
// caller can still learn that a path does not exist. Do not rely on group
// auth middleware to conceal the route table.
func (rt *Router) Group(prefix string, mw ...Middleware) *Router {
	// A group carries only the middleware added beyond the root's. Root
	// middleware is applied once in ServeHTTP, so copying it in here would
	// run it twice.
	var parentGroupMW []Middleware
	if rt.root != nil {
		parentGroupMW = rt.middleware
	}
	combined := make([]Middleware, len(parentGroupMW), len(parentGroupMW)+len(mw))
	copy(combined, parentGroupMW)
	combined = append(combined, mw...)

	return &Router{
		mux:        rt.mux,
		middleware: combined,
		prefix:     rt.prefix + prefix,
		logger:     rt.logger,
		root:       rt.rootRouter(),
	}
}

// Mount attaches a handler at a path prefix, for every method, including
// everything beneath it. Use it for sub-handlers the framework does not
// ship: a metrics endpoint, pprof, an embedded admin UI.
//
//	app.Router.Mount("/metrics", promhttp.Handler())
//	app.Router.Mount("/debug/pprof", http.DefaultServeMux)
//
// Mount does NOT rewrite the request path. The handler sees the full path,
// which is what net/http/pprof requires — its handlers are registered on
// DefaultServeMux under their absolute paths. For a sub-application that
// expects paths relative to its mount point, strip the prefix yourself:
//
//	app.Router.Mount("/admin", http.StripPrefix("/admin", adminApp))
//
// Mounted handlers run inside the root middleware chain like any other
// route, so they inherit security headers, logging and rate limiting.
// Mounting on a Group puts the handler behind that group's middleware —
// worth doing for pprof, which exposes heap and goroutine state to anyone
// who can reach it.
//
// Because Mount does not rewrite the path, a handler that dispatches on
// absolute paths (again, pprof) only works when the combined prefix equals
// the path it registered itself under. Group("/debug") + Mount("/pprof")
// works; Group("/admin") + Mount("/debug/pprof") serves 404s from within
// pprof's own mux, because it never sees the "/admin" prefix.
//
// Mount cannot coexist with a catch-all "GET /" route. A method-less
// subtree pattern and a method-bearing catch-all are mutually ambiguous to
// ServeMux — neither is more specific — so registration panics. Use
// "GET /{$}" to match only the root path, which is what a catch-all almost
// always should have been: plain "GET /" also swallows every unmatched GET
// and hides the JSON 404.
//
// A mounted handler does not own its subtree exclusively. Normal ServeMux
// precedence applies, so a more specific route wins:
//
//	rt.Mount("/api", h)               // POST /api/users -> h
//	rt.Handle("GET /api/users", g)    // GET  /api/users -> g
//
// which is usually what you want, but does mean one path can be served by
// two handlers depending on the method.
//
// Panics if prefix is empty, does not begin with "/", or is "/" alone —
// mounting at the root would shadow every route on the router.
func (rt *Router) Mount(prefix string, handler http.Handler) {
	if prefix == "" || prefix[0] != '/' {
		panic("runko: Mount prefix must begin with \"/\", got " + prefix)
	}
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" {
		panic("runko: Mount at \"/\" would shadow every route — use " +
			"Router.NotFound to handle unmatched requests instead")
	}

	// Mount registers method-less patterns, and ServeMux only rejects an
	// unclean path when the pattern carries a method. Left alone, a group
	// prefix ending in "/" would produce "/api//metrics", register without
	// complaint, and match nothing ever — a handler silently unreachable
	// for the life of the process. Fail at startup like Handle does.
	full := rt.prefixPattern(prefix)
	if cleaned := path.Clean(full); cleaned != full {
		panic("runko: Mount pattern " + full + " has an unclean path and " +
			"can never match — did the group prefix " + rt.prefix +
			" end in a slash? Try " + cleaned)
	}

	// The exact path and the subtree beneath it. Registering both means
	// "/metrics" and "/debug/pprof/heap" alike reach the handler.
	rt.Handle(prefix, handler)
	rt.Handle(prefix+"/", handler)
}

// NotFound sets the handler for requests that match no registered route.
// The default emits the standard runko.Error JSON shape. The handler runs
// inside the root middleware chain, so unmatched requests still receive
// security headers, a request ID, and an access-log line.
//
// Registering a catch-all route (e.g. "GET /") bypasses this handler for
// the methods it covers, because such a route matches every path.
func (rt *Router) NotFound(h http.Handler) {
	rt.rootRouter().notFound = h
}

// MethodNotAllowed sets the handler for requests whose path matches a
// registered route but whose method does not. The default emits the
// standard runko.Error JSON shape. The Allow header is set by the
// framework before the handler runs.
func (rt *Router) MethodNotAllowed(h http.Handler) {
	rt.rootRouter().methodNotAllowed = h
}

// ServeHTTP implements http.Handler, making Router usable as the
// server's handler directly.
//
// The root middleware chain wraps the mux itself, so it covers every
// request — matched routes, redirects, 404s and 405s alike. Applying it
// per-route instead would let unmatched requests slip past security
// headers, logging, rate limiting and host validation, since those
// requests never reach a registered route.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	root := rt.rootRouter()
	root.chain(http.HandlerFunc(root.dispatch)).ServeHTTP(w, r)
}

// dispatch resolves the request against the mux from inside the root
// middleware chain.
//
// The lookup happens exactly once and the resulting handler is used
// directly. Re-resolving after the chain has run would be unsafe: any
// middleware that rewrites the path could make the second lookup land on
// a real handler, executing it as a side effect of rendering a 404.
func (rt *Router) dispatch(w http.ResponseWriter, r *http.Request) {
	// "OPTIONS *" is an origin-wide request with no path to route. The mux
	// answers it directly; classifying it as unmatched would turn it into
	// a 405.
	if r.RequestURI == "*" {
		rt.mux.ServeHTTP(w, r)
		return
	}

	h, pattern := rt.mux.Handler(r)

	// A non-empty pattern means a matched route or a path-cleaning
	// redirect. Serve it through the mux rather than calling h directly:
	// Handler() deliberately does not populate path wildcards, so calling
	// the handler by hand would make r.PathValue — and therefore
	// PathParam — return "" for every route parameter.
	if pattern != "" {
		rt.mux.ServeHTTP(w, r)
		return
	}

	// No route matched, so h is the stdlib's own 404/405 handler. Only
	// that handler is ever run against the probe below; a user handler
	// cannot be reached from here.
	rt.serveFallback(w, r, h)
}

// serveFallback renders 404 and 405 responses in the framework's JSON
// error shape. It runs the stdlib's own handler against a probe to learn
// which of the two applies and to capture the Allow header, without
// letting its text/plain body reach the client.
func (rt *Router) serveFallback(w http.ResponseWriter, r *http.Request, h http.Handler) {
	probe := &fallbackProbe{header: make(http.Header), status: http.StatusNotFound}
	h.ServeHTTP(probe, r)

	if allow := probe.header.Get("Allow"); allow != "" {
		w.Header().Set("Allow", allow)
	}

	if probe.status == http.StatusMethodNotAllowed {
		if rt.methodNotAllowed != nil {
			rt.methodNotAllowed.ServeHTTP(w, r)
			return
		}
		Error(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"Method not allowed for this resource")
		return
	}

	if rt.notFound != nil {
		rt.notFound.ServeHTTP(w, r)
		return
	}
	Error(w, http.StatusNotFound, "not_found", "Resource not found")
}

// fallbackProbe captures the status and headers the stdlib mux would have
// written, discarding its body.
type fallbackProbe struct {
	header http.Header
	status int
}

func (p *fallbackProbe) Header() http.Header         { return p.header }
func (p *fallbackProbe) WriteHeader(code int)        { p.status = code }
func (p *fallbackProbe) Write(b []byte) (int, error) { return len(b), nil }

// chain applies the middleware stack to a handler, outermost first.
func (rt *Router) chain(handler http.Handler) http.Handler {
	// Apply in reverse so the first Use'd middleware runs first.
	h := handler
	for i := len(rt.middleware) - 1; i >= 0; i-- {
		h = rt.middleware[i](h)
	}
	return h
}

// prefixPattern prepends the group prefix to the route pattern.
// Handles the "METHOD /path" format correctly.
func (rt *Router) prefixPattern(pattern string) string {
	if rt.prefix == "" {
		return pattern
	}

	// Split "GET /path" into method and path parts.
	parts := strings.SplitN(pattern, " ", 2)
	if len(parts) == 2 {
		return parts[0] + " " + rt.prefix + parts[1]
	}

	// Pattern without method prefix.
	return rt.prefix + pattern
}

// PathParam extracts a path parameter from the request.
// Go 1.22's ServeMux stores path parameters accessible via
// r.PathValue(name).
//
// Example:
//
//	// Route: "GET /users/{id}"
//	id := runko.PathParam(r, "id")
func PathParam(r *http.Request, name string) string {
	return r.PathValue(name)
}
