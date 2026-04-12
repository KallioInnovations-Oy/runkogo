package runko

import (
	"fmt"
	"net/http"
)

// SecurityHeadersConfig configures the security headers middleware.
// All protections are enabled by default. Fields use the "Disable" pattern
// so that the zero value (an empty struct) means "everything on". (CONV-04)
type SecurityHeadersConfig struct {
	// HSTS enables Strict-Transport-Security.
	// Only enable if ALL traffic goes through TLS. Default: false.
	HSTS bool

	// HSTSMaxAge in seconds. Default: 31536000 (1 year).
	HSTSMaxAge int

	// FramePolicy controls X-Frame-Options.
	// Default: "DENY". Set to "SAMEORIGIN" if you embed your own pages.
	FramePolicy string

	// DisableNoSniff disables X-Content-Type-Options: nosniff.
	// Almost never a reason to do this. Default: false (nosniff is ON).
	DisableNoSniff bool

	// ReferrerPolicy controls the Referrer-Policy header.
	// Default: "strict-origin-when-cross-origin".
	ReferrerPolicy string

	// CacheControl sets Cache-Control on all responses.
	// Default: "no-store". Set to "DISABLE" to skip this header entirely
	// and let handlers manage their own cache policy.
	CacheControl string

	// PermissionsPolicy restricts browser features.
	// Default: "camera=(), microphone=(), geolocation=()".
	PermissionsPolicy string

	// ContentSecurityPolicy sets the Content-Security-Policy header.
	// No default is set because this is an API-first framework and
	// a default CSP would break most use cases. Set this when your
	// application serves HTML pages.
	// Example: "default-src 'self'; script-src 'self'"
	ContentSecurityPolicy string
}

// SecurityHeaders returns a middleware that sets standard security
// headers on every response. Include as the first middleware in your
// chain via DefaultSecurityHeaders() or with custom config. (CONV-04)
//
// Headers set:
//   - X-Content-Type-Options: nosniff (prevents MIME type sniffing)
//   - X-Frame-Options: DENY (prevents clickjacking)
//   - X-XSS-Protection: 0 (disables legacy XSS filter, CSP is preferred)
//   - Referrer-Policy: strict-origin-when-cross-origin
//   - Cache-Control: no-store (prevents caching of API responses)
//   - Permissions-Policy: restricts browser API access
//   - Strict-Transport-Security: only when HSTS is explicitly enabled
//
// No Server header is set — the framework does not identify itself
// in responses to avoid disclosing infrastructure details. (PRIV-04)
func SecurityHeaders(cfg SecurityHeadersConfig) Middleware {
	// Apply defaults for string fields (zero value = use default).
	if cfg.FramePolicy == "" {
		cfg.FramePolicy = "DENY"
	}
	if cfg.ReferrerPolicy == "" {
		cfg.ReferrerPolicy = "strict-origin-when-cross-origin"
	}
	if cfg.CacheControl == "" {
		cfg.CacheControl = "no-store"
	}
	if cfg.PermissionsPolicy == "" {
		cfg.PermissionsPolicy = "camera=(), microphone=(), geolocation=()"
	}
	if cfg.HSTS && cfg.HSTSMaxAge == 0 {
		cfg.HSTSMaxAge = 31536000
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !cfg.DisableNoSniff {
				w.Header().Set("X-Content-Type-Options", "nosniff")
			}
			w.Header().Set("X-Frame-Options", cfg.FramePolicy)
			w.Header().Set("X-XSS-Protection", "0")
			w.Header().Set("Referrer-Policy", cfg.ReferrerPolicy)

			if cfg.CacheControl != "DISABLE" {
				w.Header().Set("Cache-Control", cfg.CacheControl)
			}
			if cfg.PermissionsPolicy != "" {
				w.Header().Set("Permissions-Policy", cfg.PermissionsPolicy)
			}
			if cfg.HSTS {
				w.Header().Set("Strict-Transport-Security",
					fmt.Sprintf("max-age=%d; includeSubDomains", cfg.HSTSMaxAge))
			}
			if cfg.ContentSecurityPolicy != "" {
				w.Header().Set("Content-Security-Policy", cfg.ContentSecurityPolicy)
			}

			next.ServeHTTP(w, r)
		})
	}
}

// DefaultSecurityHeaders returns a SecurityHeaders middleware with
// all safe defaults enabled. HSTS is off (requires explicit opt-in
// because it assumes TLS termination).
func DefaultSecurityHeaders() Middleware {
	return SecurityHeaders(SecurityHeadersConfig{})
}
