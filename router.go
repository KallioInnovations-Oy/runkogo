package runko

import (
	"log/slog"
	"net/http"
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
}

func newRouter(logger *slog.Logger) *Router {
	return &Router{
		mux:    http.NewServeMux(),
		logger: logger,
	}
}

// Use appends middleware to the router's chain. Middleware runs in the
// order added: Use(A); Use(B) means A runs first, calls B, which calls
// the handler.
//
// Register all middleware BEFORE calling Handle. Routes freeze their chain
// at registration time, so middleware added after a Handle call will not
// apply to that route. Use is not safe for concurrent calls once the
// server is serving requests.
func (rt *Router) Use(mw ...Middleware) {
	rt.middleware = append(rt.middleware, mw...)
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
	wrapped := rt.chain(handler)
	rt.mux.Handle(fullPattern, wrapped)

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
func (rt *Router) Group(prefix string, mw ...Middleware) *Router {
	// Combine parent and group middleware.
	combined := make([]Middleware, len(rt.middleware), len(rt.middleware)+len(mw))
	copy(combined, rt.middleware)
	combined = append(combined, mw...)

	return &Router{
		mux:        rt.mux,
		middleware: combined,
		prefix:     rt.prefix + prefix,
		logger:     rt.logger,
	}
}

// ServeHTTP implements http.Handler, making Router usable as the
// server's handler directly.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt.mux.ServeHTTP(w, r)
}

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
