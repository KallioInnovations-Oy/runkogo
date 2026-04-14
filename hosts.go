package runko

import (
	"net/http"
	"strings"
)

// AllowedHostsConfig configures the AllowedHosts middleware.
type AllowedHostsConfig struct {
	// Hosts is the list of allowed hostnames. Matching is case-insensitive.
	// The port is stripped from the Host header before comparison.
	// Example: []string{"example.com", "api.example.com"}
	Hosts []string
}

// AllowedHosts returns a middleware that validates the HTTP Host header
// against a whitelist. Requests with a Host not in the list receive
// 421 Misdirected Request. This prevents host-header injection that can
// poison caches and generate incorrect URLs.
//
// Panics if Hosts is empty (fail-fast on misconfiguration).
func AllowedHosts(cfg AllowedHostsConfig) Middleware {
	if len(cfg.Hosts) == 0 {
		panic("runko: AllowedHosts requires at least one allowed hostname")
	}

	allowed := make(map[string]bool, len(cfg.Hosts))
	for _, h := range cfg.Hosts {
		allowed[strings.ToLower(strings.TrimSpace(h))] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := stripHostPort(r.Host)
			host = strings.ToLower(host)

			if !allowed[host] {
				Error(w, http.StatusMisdirectedRequest, "invalid_host",
					"The request host is not allowed")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// stripHostPort removes the port from a host string.
// Handles IPv6 addresses in brackets (e.g., "[::1]:8080").
func stripHostPort(host string) string {
	// IPv6 with port: [::1]:8080
	if strings.HasPrefix(host, "[") {
		if idx := strings.LastIndex(host, "]:"); idx != -1 {
			return host[1:idx]
		}
		return strings.Trim(host, "[]")
	}

	// Regular host:port or IPv4:port.
	// Only strip if there's exactly one colon (not an IPv6 address).
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		if strings.Count(host, ":") == 1 {
			return host[:idx]
		}
	}

	return host
}
