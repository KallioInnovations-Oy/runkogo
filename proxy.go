package runko

import (
	"net"
	"strings"
)

// proxyResolver determines the real client IP by walking the
// X-Forwarded-For chain and stopping at the last untrusted hop.
//
// Security model (CONV-01):
//   - If no trusted proxies are configured, forwarding headers are
//     IGNORED entirely and RemoteAddr is always used. This is the
//     secure default.
//   - If trusted proxies are configured, the resolver reads
//     X-Forwarded-For right-to-left (since the rightmost entries
//     are added by infrastructure you control) and returns the
//     first IP that is NOT in the trusted set. This is the real
//     client IP.
//   - The leftmost X-Forwarded-For entry is client-controlled and
//     must NEVER be trusted unconditionally.
type proxyResolver struct {
	trusted []net.IPNet
	enabled bool
}

// newProxyResolver creates a resolver from a list of trusted
// proxy addresses. Accepts individual IPs ("127.0.0.1") and CIDR
// ranges ("10.0.0.0/8"). Panics on invalid entries to enforce
// fail-fast at startup (CONV-05).
func newProxyResolver(trustedProxies []string) *proxyResolver {
	if len(trustedProxies) == 0 {
		return &proxyResolver{enabled: false}
	}

	networks := make([]net.IPNet, 0, len(trustedProxies))
	for _, entry := range trustedProxies {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		// Try CIDR first.
		_, network, err := net.ParseCIDR(entry)
		if err == nil {
			networks = append(networks, *network)
			continue
		}

		// Try bare IP — convert to /32 or /128.
		ip := net.ParseIP(entry)
		if ip == nil {
			panic("runko: invalid trusted proxy entry: " + entry)
		}

		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		networks = append(networks, net.IPNet{
			IP:   ip,
			Mask: net.CIDRMask(bits, bits),
		})
	}

	return &proxyResolver{
		trusted: networks,
		enabled: len(networks) > 0,
	}
}

// isTrusted returns true if the given IP is within any trusted range.
func (pr *proxyResolver) isTrusted(ipStr string) bool {
	if !pr.enabled {
		return false
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, network := range pr.trusted {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// resolveClientIP determines the real client IP from the request.
//
// When trusted proxies are NOT configured:
//   - Returns RemoteAddr directly (stripped of port).
//   - X-Forwarded-For is completely ignored.
//
// When trusted proxies ARE configured:
//  1. Check if the direct connection (RemoteAddr) is from a trusted proxy.
//  2. If not trusted, return RemoteAddr (the client connected directly).
//  3. If trusted, walk X-Forwarded-For right-to-left and return the
//     first non-trusted IP. This is the real client.
//  4. If all IPs in the chain are trusted, return the leftmost
//     (entire path is internal infrastructure).
func (pr *proxyResolver) resolveClientIP(remoteAddr string, xForwardedFor string) string {
	directIP := stripPort(remoteAddr)

	// No trusted proxies configured — never read forwarding headers.
	if !pr.enabled {
		return directIP
	}

	// Direct connection is not from a trusted proxy — it IS the client.
	if !pr.isTrusted(directIP) {
		return directIP
	}

	// No forwarding header present despite trusted proxy.
	if xForwardedFor == "" {
		return directIP
	}

	// Parse the X-Forwarded-For chain.
	ips := strings.Split(xForwardedFor, ",")
	for i := range ips {
		ips[i] = strings.TrimSpace(ips[i])
	}

	// Walk right-to-left, find the first non-trusted IP.
	for i := len(ips) - 1; i >= 0; i-- {
		if ips[i] == "" {
			continue
		}
		// Validate that the entry is a real IP address.
		// Skip garbage entries injected by malicious clients.
		if net.ParseIP(ips[i]) == nil {
			continue
		}
		if !pr.isTrusted(ips[i]) {
			return ips[i]
		}
	}

	// Entire chain is trusted infrastructure — return leftmost.
	if len(ips) > 0 && ips[0] != "" && net.ParseIP(ips[0]) != nil {
		return ips[0]
	}

	return directIP
}

func stripPort(addr string) string {
	// IPv6 with port: [::1]:8080
	if strings.HasPrefix(addr, "[") {
		if idx := strings.LastIndex(addr, "]:"); idx != -1 {
			return addr[1:idx]
		}
		// [::1] without port
		return strings.Trim(addr, "[]")
	}

	// IPv4 with port: 1.2.3.4:8080
	// Only strip if there's exactly one colon (not an IPv6 address).
	if idx := strings.LastIndex(addr, ":"); idx != -1 {
		// Verify it's not an IPv6 address without brackets.
		if strings.Count(addr, ":") == 1 {
			return addr[:idx]
		}
	}

	return addr
}
