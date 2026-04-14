package runko

import (
	"fmt"
	"net/http"
)

// SecurityHeadersConfig configures the security headers middleware.
// All protections are enabled by default. Fields use the "Disable" pattern
// so the zero value means "everything on".
type SecurityHeadersConfig struct {
	// HSTS enables Strict-Transport-Security. Only enable if ALL traffic
	// goes through TLS. Default: false.
	HSTS bool

	// HSTSMaxAge in seconds. Default: 31536000 (1 year).
	HSTSMaxAge int

	// FramePolicy controls X-Frame-Options. Default: "DENY". Set to
	// "SAMEORIGIN" if you embed your own pages.
	FramePolicy string

	// DisableNoSniff disables X-Content-Type-Options: nosniff. Default:
	// false (nosniff is on).
	DisableNoSniff bool

	// ReferrerPolicy controls the Referrer-Policy header.
	// Default: "strict-origin-when-cross-origin".
	ReferrerPolicy string

	// CacheControl sets Cache-Control on all responses. Default:
	// "no-store". Set to "DISABLE" to skip this header entirely.
	CacheControl string

	// PermissionsPolicy restricts browser features.
	// Default: "camera=(), microphone=(), geolocation=()".
	PermissionsPolicy string

	// ContentSecurityPolicy sets CSP. No default because this is an
	// API-first framework and a default would break most use cases.
	// Example: "default-src 'self'; script-src 'self'".
	ContentSecurityPolicy string
}

// SecurityHeaders returns a middleware that sets standard security headers
// on every response. Include as the first middleware in your chain via
// DefaultSecurityHeaders or with custom config.
//
// No Server header is set — the framework does not identify itself.
func SecurityHeaders(cfg SecurityHeadersConfig) Middleware {
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

// DefaultSecurityHeaders returns a SecurityHeaders middleware with safe
// defaults. HSTS is off (requires explicit opt-in because it assumes TLS
// termination).
func DefaultSecurityHeaders() Middleware {
	return SecurityHeaders(SecurityHeadersConfig{})
}
