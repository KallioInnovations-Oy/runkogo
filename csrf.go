package runko

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
)

// CSRFConfig configures the CSRF middleware.
type CSRFConfig struct {
	// CookieName is the name of the cookie holding the CSRF token.
	// Defaults to "csrf_token".
	CookieName string

	// HeaderName is the request header that must echo the cookie value.
	// Defaults to "X-CSRF-Token".
	HeaderName string

	// Secure marks the cookie as Secure (HTTPS only). Default: true.
	// Set to false for local development over plain HTTP.
	Secure *bool

	// CookiePath is the Path attribute of the cookie. Defaults to "/".
	CookiePath string

	// CookieDomain is the Domain attribute. Leave empty (the default) to
	// scope the cookie to the host that set it. Set to ".example.com" to
	// share the token across subdomains — the subdomains must also match
	// on SameSite and TLS.
	CookieDomain string

	// SameSite is the SameSite attribute. Defaults to http.SameSiteLaxMode,
	// which allows top-level cross-site GET navigation to carry the cookie.
	// Set to http.SameSiteStrictMode if your app has no legitimate
	// cross-site entry points.
	SameSite http.SameSite

	// SkipAuthHeader, when true, bypasses CSRF checks for requests that
	// carry an Authorization header. This is safe for token-authenticated
	// APIs (bearer tokens are not sent by browsers automatically) but
	// must not be enabled for cookie-authenticated apps. Default: false.
	SkipAuthHeader bool
}

// CSRF returns a double-submit-cookie CSRF middleware.
//
// On safe methods (GET, HEAD, OPTIONS) the middleware issues a fresh token
// as a cookie if one is not already present. On unsafe methods it requires
// the header to match the cookie using a constant-time comparison.
//
// The double-submit pattern defends against cross-origin form submissions:
// an attacker's page can trigger a request that carries the victim's
// cookies automatically, but cannot read or echo the token value into a
// custom header because same-origin policy blocks that.
//
// Requires cookies to be SameSite=Lax or Strict (the default) to prevent
// the token from being sent on cross-site top-level navigations.
//
// LIMITATION — subdomain cookie injection: this implementation stores the
// token as a plain cookie (no HMAC). If an attacker controls a sibling
// subdomain (via XSS on evil.example.com or a compromised host) they can
// set the parent-domain cookie via document.cookie and the main app will
// trust it. The standard mitigation is a signed double-submit cookie
// (HMAC over the session identifier). Until signed sessions are added,
// protect against subdomain compromise at the infrastructure layer:
// constrain who can run subdomains, enable HSTS with includeSubDomains,
// and prefer SameSite=Strict where the app allows.
func CSRF(cfg CSRFConfig) Middleware {
	if cfg.CookieName == "" {
		cfg.CookieName = "csrf_token"
	}
	if cfg.HeaderName == "" {
		cfg.HeaderName = "X-CSRF-Token"
	}
	if cfg.CookiePath == "" {
		cfg.CookiePath = "/"
	}
	if cfg.SameSite == 0 {
		cfg.SameSite = http.SameSiteLaxMode
	}
	secure := true
	if cfg.Secure != nil {
		secure = *cfg.Secure
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Safe methods: ensure a token exists, never validate.
			if isSafeMethod(r.Method) {
				if _, err := r.Cookie(cfg.CookieName); err != nil {
					token := newCSRFToken()
					http.SetCookie(w, &http.Cookie{
						Name:     cfg.CookieName,
						Value:    token,
						Path:     cfg.CookiePath,
						Domain:   cfg.CookieDomain,
						HttpOnly: false, // JS needs to read this to echo in header
						Secure:   secure,
						SameSite: cfg.SameSite,
					})
				}
				next.ServeHTTP(w, r)
				return
			}

			if cfg.SkipAuthHeader && r.Header.Get("Authorization") != "" {
				next.ServeHTTP(w, r)
				return
			}

			cookie, err := r.Cookie(cfg.CookieName)
			if err != nil || cookie.Value == "" {
				Error(w, http.StatusForbidden, "csrf_missing", "CSRF token missing")
				return
			}
			header := r.Header.Get(cfg.HeaderName)
			if header == "" {
				Error(w, http.StatusForbidden, "csrf_missing", "CSRF token missing")
				return
			}
			if subtle.ConstantTimeCompare([]byte(header), []byte(cookie.Value)) != 1 {
				Error(w, http.StatusForbidden, "csrf_invalid", "CSRF token mismatch")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// newCSRFToken returns a URL-safe 32-byte random token. Crypto/rand failure
// panics — by the time we're issuing CSRF tokens the process must have a
// working entropy source, and a weak token would defeat the defense.
func newCSRFToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("runko: crypto/rand failed while generating CSRF token: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
